package main

import (
	"bufio"
	"crypto/md5"
	b64 "encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type RtspClient struct {
	socket   net.Conn
	reader   *bufio.Reader
	ioMu     sync.Mutex
	received chan []byte //out chanel
	signals  chan bool   //signals quit
	host     string      //host
	port     string      //port
	uri      string      //url
	login    string
	password string   //password
	session  string   //rtsp session
	auth     string   //authorization
	track    []string //rtsp track
	cseq     int      //qury number
	videow   int
	videoh   int
}

// returns an empty initialized object
func NewRtspClient() *RtspClient {
	Obj := &RtspClient{
		cseq:     1,                      // starting query number
		signals:  make(chan bool, 1),     // buffered channel for 1 message
		received: make(chan []byte, 512), // bounded queue to limit retained RTP buffers under backpressure
	}
	return Obj
}

// main function for working with RTSP
func (rc *RtspClient) Client(rtsp_url string, _ bool) error {
	// check and parse URL
	if !rc.ParseUrl(rtsp_url) {
		return errors.New("Invalid URL")
	}

	// establish connection to the camera
	if !rc.Connect() {
		return errors.New("Unable to connect")
	}
	// phase 1 OPTIONS - first stage of communication with the camera
	// send OPTIONS request and read response to the OPTIONS request
	if status, _, _ := rc.SendRequest("OPTIONS", rc.uri, nil); status != 200 {
		rc.SendRequest("OPTIONS", rc.uri, nil)
	}

	// PHASE 2 DESCRIBE
	if status, message, err := rc.SendRequest("DESCRIBE", rc.uri, nil); err != nil {
		return errors.New("Unable to read DESCRIBE response; connection lost?")
	} else if status != 200 {
		return errors.New("DESCRIBE error; not status code 200 OK " + message)
	} else {
		rc.track = rc.ParseMedia(message)
	}
	if len(rc.track) == 0 {
		return errors.New("Error; track not found")
	}

	// PHASE 3 SETUP
	transport := "RTP/AVP/TCP;unicast;interleaved=0-1"

	if status, message, err := rc.SendRequest("SETUP", rc.uri+"/"+rc.track[0], map[string]string{"Transport": transport}); err != nil {
		return errors.New("Unable to read SETUP response; connection lost")
	} else if status == 200 {
		rc.session = ParseSession(message)
		Info("RTSP", "Session: %s", rc.session)
	}

	if len(rc.track) > 1 {
		transport := "RTP/AVP/TCP;unicast;interleaved=2-3"

		if status, message, err := rc.SendRequest("SETUP", rc.uri+"/"+rc.track[1], map[string]string{"Transport": transport}); err != nil {
			return errors.New("Unable to read SETUP response; connection lost")
		} else if status == 200 {
			rc.session = ParseSession(message)
			Info("RTSP", "Session: %s", rc.session)
		}
	}

	// PHASE 4 PLAY
	if status, _, err := rc.SendRequest("PLAY", rc.uri, map[string]string{"Range": "npt=0-"}); err != nil {
		return errors.New("Unable to read PLAY response; connection lost")
	} else if status == 200 {
		go rc.RtspRtpLoop()
		return nil
	}
	return errors.New("other error")
}

/*
The RTP header has the following format:

	 0                   1                   2                   3
	 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	|V=2|P|X|  CC   |M|     PT      |       sequence number         |
	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	|                           timestamp                           |
	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
	|           synchronization source (SSRC) identifier            |
	+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+=+
	|            contributing source (CSRC) identifiers             |
	|                             ....                              |
	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+

version (V): 2 bits

	This field identifies the version of RTP. The version defined by
	this specification is two (2).

padding (P): 1 bit

	If the padding bit is set, the packet contains one or more
	additional padding octets at the end which are not part of the
	payload. The last octet of the padding contains a count of how
	many padding octets should be ignored, including itself. Padding
	may be needed by some encryption algorithms with fixed block sizes
	or for carrying several RTP packets in a lower-layer protocol data
	unit.

extension (X): 1 bit

	If the extension bit is set, the fixed header MUST be followed by
	exactly one header extension.
*/
func (rc *RtspClient) RtspRtpLoop() {
	defer func() {
		if r := recover(); r != nil {
			Error("RTSP", "Recovered from panic in RtspRtpLoop: %v", r)
		}
		rc.signals <- true
	}()

	header := make([]byte, 4)
	payload := make([]byte, 8192)

	// Keepalive ticker: fires every 50 s off the hot path.
	keepaliveTicker := time.NewTicker(50 * time.Second)
	defer keepaliveTicker.Stop()

	reconnectAttempts := 0
	const maxReconnectAttempts = 5

	for {
		// Non-blocking check for keepalive tick.
		select {
		case <-keepaliveTicker.C:
			if _, _, err := rc.SendRequest("OPTIONS", rc.uri, nil); err != nil {
				Error("RTSP", "Keepalive failed: %v", err)
				reconnectAttempts++
				if reconnectAttempts > maxReconnectAttempts {
					Error("RTSP", "Max reconnect attempts reached (%d), giving up", maxReconnectAttempts)
					return
				}
				backoff := time.Duration(1<<uint(reconnectAttempts-1)) * time.Second
				if backoff > 30*time.Second {
					backoff = 30 * time.Second
				}
				Warn("RTSP", "Reconnect attempt %d/%d in %v", reconnectAttempts, maxReconnectAttempts, backoff)
				time.Sleep(backoff)
				continue
			}
			reconnectAttempts = 0
			Debug("RTSP", "Keepalive successful")
		default:
		}

		if err := rc.handleTCPPackets(header, payload); err != nil {
			Error("RTSP", "TCP read error: %v", err)
			return
		}
	}
}

// rtpPacketPool reuses RTP packet buffers to reduce GC pressure.
// 2 KB covers the typical RTP/H.264 packet (≤ MTU). Larger packets cause append
// to grow the slice; those oversized buffers are not returned to the pool.
var rtpPacketPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, 0, 2048)
	},
}

// Method to handle TCP packets
func (rc *RtspClient) handleTCPPackets(header []byte, payload []byte) error {
	// Set a read deadline only once per call rather than per-byte.
	// 5 s gives enough slack for a bursty camera without burning syscalls on every packet.
	rc.socket.SetReadDeadline(time.Now().Add(5 * time.Second))

	// Read the interleaved header
	n, err := io.ReadFull(rc.reader, header)
	if err != nil {
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			// Read timeout - this is normal, continue
			return nil
		}
		return fmt.Errorf("failed to read interleaved header: %v", err)
	}

	if n != 4 {
		return fmt.Errorf("incomplete header read: %d bytes", n)
	}

	// Check the RTP marker (0x24 in hex, or 36 in decimal)
	if header[0] != 36 {
		Warn("RTSP", "Unexpected start byte; synchronization lost")
		// Try to resynchronize by reading byte by byte until finding 0x24
		return nil
	}

	// Extract the payload length from the header
	payloadLen := int(header[2])<<8 + int(header[3])
	if payloadLen > len(payload) {
		// Grow the read buffer to fit this unusually large frame.
		payload = make([]byte, payloadLen)
	}

	if payloadLen < 12 {
		Debug("RTSP", "Invalid payload length; discarding packet")
		return nil
	}

	// Read the RTP payload
	n, err = io.ReadFull(rc.reader, payload[:payloadLen])
	if err != nil {
		return fmt.Errorf("error reading RTP payload: %v", err)
	}

	if n != payloadLen {
		return fmt.Errorf("incomplete payload read: %d/%d bytes", n, payloadLen)
	}

	// Get a buffer from the pool and use it to build the complete packet
	// This avoids repeated memory allocations
	rtpPacket := rtpPacketPool.Get().([]byte)
	rtpPacket = rtpPacket[:0] // Reset the buffer without reallocating

	// Build the complete packet (header + payload)
	rtpPacket = append(rtpPacket, header...)
	rtpPacket = append(rtpPacket, payload[:payloadLen]...)

	// Send the packet to the channel, with non-blocking handling
	select {
	case rc.received <- rtpPacket:
		// Packet sent successfully
		Debug("RTSP", "Packet received (size: %d bytes)", payloadLen+4)
	default:
		// Channel full, drop this packet to avoid blocking
		// Return the buffer to the pool for reuse
		rtpPacketPool.Put(rtpPacket)
		Warn("RTSP", "Channel full, dropping packet")
	}

	return nil
}

func (rc *RtspClient) Connect() bool {
	// Longer timeout for slow or unstable networks
	d := &net.Dialer{Timeout: 5 * time.Second}
	Info("RTSP", "Connecting to %s:%s", rc.host, rc.port)
	conn, err := d.Dial("tcp", rc.host+":"+rc.port)
	if err != nil {
		Error("RTSP", "Connection error: %v", err)
		return false
	}

	// Set read/write timeout to avoid blocking
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(30 * time.Second)
		tcpConn.SetNoDelay(true) // Disable Nagle's algorithm to reduce latency
	}

	rc.socket = conn
	rc.reader = bufio.NewReaderSize(conn, 16*1024)
	Info("RTSP", "Connected successfully")
	return true
}

// Reusable buffer for RTSP requests
var requestBufferPool = sync.Pool{
	New: func() interface{} {
		return new(strings.Builder)
	},
}

// SendRequest acquires ioMu and delegates to sendRequest.
func (rc *RtspClient) SendRequest(requestType string, uri string, content map[string]string) (int, string, error) {
	rc.ioMu.Lock()
	defer rc.ioMu.Unlock()
	return rc.sendRequest(requestType, uri, content)
}

// sendRequest executes an RTSP request. Caller must hold ioMu.
func (rc *RtspClient) sendRequest(requestType string, uri string, content map[string]string) (int, string, error) {
	if rc.socket == nil {
		return 0, "", errors.New("socket is not initialized")
	}

	rc.cseq += 1

	// Get a buffer from the pool to build the request
	sb := requestBufferPool.Get().(*strings.Builder)
	sb.Reset()                      // Reset the buffer
	defer requestBufferPool.Put(sb) // Return the buffer to the pool when done

	// Build the request efficiently
	sb.WriteString(requestType)
	sb.WriteString(" ")
	sb.WriteString(uri)
	sb.WriteString(" RTSP/1.0\r\nUser-Agent: rtsp-client\r\nAccept: application/sdp\r\nCSeq: ")
	sb.WriteString(strconv.Itoa(rc.cseq))
	sb.WriteString("\r\n")

	if rc.auth != "" {
		sb.WriteString(rc.auth)
		sb.WriteString("\r\n")
	}

	for k, v := range content {
		sb.WriteString(k)
		sb.WriteString(": ")
		sb.WriteString(v)
		sb.WriteString("\r\n")
	}

	if rc.session != "" {
		sb.WriteString("Session: ")
		sb.WriteString(rc.session)
		sb.WriteString("\r\n")
	}

	sb.WriteString("\r\n")
	message := sb.String()

	// Reduce logging to decrease CPU usage
	isOptions := requestType == "OPTIONS"
	if isOptions {
		// Don't log OPTIONS requests which are frequent
		Debug("RTSP", "Sending OPTIONS request")
	} else {
		Info("RTSP", "Sending %s request", requestType)
	}

	if _, e := rc.socket.Write([]byte(message)); e != nil {
		Error("RTSP", "Socket write failed: %v", e)
		return 0, "", e
	}

	result, statusCode, err := rc.readRTSPResponse()
	if err != nil {
		Error("RTSP", "Socket read failed: %v", err)
		return 0, "", err
	}

	// Reduce logging - don't display the full content of responses
	if isOptions {
		Debug("RTSP", "Received OPTIONS response")
	} else {
		Info("RTSP", "Received %s response with status code %d", requestType, statusCode)
	}

	// check authentication method if not set
	if rc.auth == "" {
		if strings.Contains(result, "Digest") {
			nonce := ParseDirective(result, "nonce")
			realm := ParseDirective(result, "realm")
			hs1 := GetMD5Hash(rc.login + ":" + realm + ":" + rc.password)
			hs2 := GetMD5Hash(requestType + ":" + rc.uri)
			response := GetMD5Hash(hs1 + ":" + nonce + ":" + hs2)

			// Build the authentication header efficiently
			var authBuilder strings.Builder
			authBuilder.WriteString(`Authorization: Digest username="`)
			authBuilder.WriteString(rc.login)
			authBuilder.WriteString(`", realm="`)
			authBuilder.WriteString(realm)
			authBuilder.WriteString(`", nonce="`)
			authBuilder.WriteString(nonce)
			authBuilder.WriteString(`", uri="`)
			authBuilder.WriteString(rc.uri)
			authBuilder.WriteString(`", response="`)
			authBuilder.WriteString(response)
			authBuilder.WriteString(`"`)
			rc.auth = authBuilder.String()
			Info("RTSP", "Using Digest authentication")
		} else if strings.Contains(result, "Basic") {
			rc.auth = "Authorization: Basic " + b64.StdEncoding.EncodeToString([]byte(rc.login+":"+rc.password))
			Info("RTSP", "Using Basic authentication")
		}
	}

	return statusCode, result, nil
}

func (rc *RtspClient) readRTSPResponse() (string, int, error) {
	if rc.socket == nil || rc.reader == nil {
		return "", 0, errors.New("socket is not initialized")
	}

	var frameHeader [4]byte
	for {
		if err := rc.socket.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
			return "", 0, err
		}

		peek, err := rc.reader.Peek(1)
		if err != nil {
			return "", 0, err
		}

		// Skip interleaved RTP/RTCP frames that may arrive while waiting for OPTIONS responses.
		if peek[0] == 0x24 {
			if _, err := io.ReadFull(rc.reader, frameHeader[:]); err != nil {
				return "", 0, fmt.Errorf("failed to read interleaved frame header: %w", err)
			}
			frameLen := int(frameHeader[2])<<8 | int(frameHeader[3])
			if frameLen > 0 {
				if _, err := io.CopyN(io.Discard, rc.reader, int64(frameLen)); err != nil {
					return "", 0, fmt.Errorf("failed to discard interleaved frame: %w", err)
				}
			}
			Debug("RTSP", "Discarded interleaved frame while reading RTSP response")
			continue
		}

		statusLine, err := rc.reader.ReadString('\n')
		if err != nil {
			return "", 0, err
		}
		if !strings.HasPrefix(statusLine, "RTSP/") {
			continue
		}

		var responseBuilder strings.Builder
		responseBuilder.WriteString(statusLine)

		contentLength := 0
		for {
			line, err := rc.reader.ReadString('\n')
			if err != nil {
				return "", 0, err
			}
			responseBuilder.WriteString(line)
			if line == "\r\n" {
				break
			}
			if strings.HasPrefix(strings.ToLower(line), "content-length:") {
				v := strings.TrimSpace(strings.TrimPrefix(strings.ToLower(line), "content-length:"))
				contentLength, _ = strconv.Atoi(v)
			}
		}

		if contentLength > 0 {
			body := make([]byte, contentLength)
			if _, err := io.ReadFull(rc.reader, body); err != nil {
				return "", 0, err
			}
			responseBuilder.Write(body)
		}

		fullResponse := responseBuilder.String()
		statusCode := parseRTSPStatusCode(statusLine)
		return fullResponse, statusCode, nil
	}
}

func parseRTSPStatusCode(statusLine string) int {
	parts := strings.Fields(statusLine)
	if len(parts) < 2 {
		return 0
	}
	code, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0
	}
	return code
}

func (rc *RtspClient) ParseUrl(rtsp_url string) bool {
	u, err := url.Parse(rtsp_url)
	if err != nil {
		Error("RTSP", "Failed to parse URL: %v", err)
		return false
	}
	phost := strings.Split(u.Host, ":")
	rc.host = phost[0]
	if len(phost) == 2 {
		rc.port = phost[1]
	} else {
		rc.port = "554"
	}
	rc.login = u.User.Username()
	rc.password, _ = u.User.Password()
	Info("RTSP", "URL parsed - host: %s, port: %s, login: %s", rc.host, rc.port, rc.login)
	rc.uri = "rtsp://" + rc.host + ":" + rc.port + u.Path
	if u.RawQuery != "" {
		rc.uri += "?" + string(u.RawQuery)
	}
	return true
}

func (rc *RtspClient) Close() {
	rc.ioMu.Lock()
	defer rc.ioMu.Unlock()

	// Properly close the RTSP connection
	if rc.socket != nil {
		// Send TEARDOWN to inform the RTSP server.
		// Use sendRequest (no lock) because Close() already holds ioMu.
		if rc.session != "" {
			Info("RTSP", "Sending TEARDOWN request")
			rc.sendRequest("TEARDOWN", rc.uri, nil)
		}

		// Close the TCP socket
		rc.socket.Close()
		rc.socket = nil
		rc.reader = nil
	}

	// Drain all buffered packets and return their memory to the pool.
drain:
	for {
		select {
		case b := <-rc.received:
			rtpPacketPool.Put(b[:0])
		default:
			break drain
		}
	}

	Info("RTSP", "Resources released")
}

func ParseDirective(header, name string) string {
	start := strings.Index(header, name+`="`)
	if start == -1 {
		return ""
	}
	start += len(name) + 2
	end := start + strings.Index(header[start:], `"`)
	return strings.TrimSpace(header[start:end])
}

func ParseSession(header string) string {
	mparsed := strings.Split(header, "\r\n")
	for _, element := range mparsed {
		if strings.Contains(element, "Session:") {
			if strings.Contains(element, ";") {
				first := strings.Split(element, ";")[0]
				return first[9:]
			} else {
				return element[9:]
			}
		}
	}
	return ""
}

func GetMD5Hash(text string) string {
	hash := md5.Sum([]byte(text))
	return hex.EncodeToString(hash[:])
}

// Constants to avoid repeated string allocations
const (
	controlPrefix       = "a=control:"
	dimensionsPrefix    = "a=x-dimensions:"
	controlPrefixLen    = len(controlPrefix)
	dimensionsPrefixLen = len(dimensionsPrefix)
)

// Pool of slices for dimensions
var dimsPool = sync.Pool{
	New: func() interface{} {
		return make([]int, 0, 2)
	},
}

func (rc *RtspClient) ParseMedia(header string) []string {
	// Preallocate the slice to avoid reallocations
	letters := make([]string, 0, 2) // Most streams have 1-2 tracks

	// Avoid reallocating a slice for each line
	mparsed := strings.Split(header, "\r\n")

	for _, element := range mparsed {
		// Search for track control
		if idx := strings.Index(element, controlPrefix); idx != -1 && !strings.Contains(element, "*") {
			paste := element[idx+controlPrefixLen:]
			if slashIdx := strings.LastIndex(paste, "/"); slashIdx != -1 {
				paste = paste[slashIdx+1:]
			}
			letters = append(letters, paste)
		}

		// Search for dimensions
		if strings.HasPrefix(element, dimensionsPrefix) {
			// Get a slice from the pool
			dims := dimsPool.Get().([]int)
			dims = dims[:0] // Reset without reallocating

			// Parse the dimensions
			dimPart := element[dimensionsPrefixLen:]
			for _, s := range strings.Split(dimPart, ",") {
				v := 0
				fmt.Sscanf(s, "%d", &v)
				if v <= 0 {
					break
				}
				dims = append(dims, v)
			}

			// Use the dimensions if valid
			if len(dims) == 2 {
				rc.videow = dims[0]
				rc.videoh = dims[1]
				Info("RTSP", "Video dimensions: %dx%d", rc.videow, rc.videoh)
			}

			// Return the slice to the pool
			dimsPool.Put(dims)
		}
	}

	Debug("RTSP", "Found %d tracks", len(letters))
	return letters
}
