package main

import (
	"crypto/md5"
	b64 "encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"errors"
)

type RtspClient struct {
	socket   net.Conn
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
		received: make(chan []byte, 5000), // reduced from 100000 to 5000 to save memory
	}
	return Obj
}

// main function for working with RTSP
func (rc *RtspClient) Client(rtsp_url string, _ bool) error {
	// check and parse URL
	if !rc.ParseUrl(rtsp_url) {
		return errors.New( "Invalid URL")
	}

	// establish connection to the camera
	if !rc.Connect() {
		return errors.New( "Unable to connect")
	}
	// phase 1 OPTIONS - first stage of communication with the camera
	// send OPTIONS request and read response to the OPTIONS request
	if status, _, _ := rc.SendRequest("OPTIONS", rc.uri, nil); status != 200 {
		rc.SendRequest("OPTIONS", rc.uri, nil)
	}
	

	// PHASE 2 DESCRIBE		
	if status, message, err := rc.SendRequest("DESCRIBE", rc.uri, nil); err != nil {
		return errors.New(  "Unable to read DESCRIBE response; connection lost?")
	} else if status != 200 {
		return errors.New(  "DESCRIBE error; not status code 200 OK " + message)
	} else {		
		rc.track = rc.ParseMedia(message)
	}
	if len(rc.track) == 0 {
		return errors.New( "Error; track not found")
	}

	// PHASE 3 SETUP
	transport := "RTP/AVP/TCP;unicast;interleaved=0-1"
	
	if status, message, err := rc.SendRequest("SETUP", rc.uri + "/" + rc.track[0], map[string]string{"Transport": transport}); err != nil {
		return errors.New( "Unable to read SETUP response; connection lost")
	} else if status == 200 {
			rc.session = ParseSession(message)
			log.Printf("Session : %s", rc.session)			
	}
	
	if len(rc.track) > 1 {
		transport := "RTP/AVP/TCP;unicast;interleaved=2-3"

		if status, message, err := rc.SendRequest("SETUP", rc.uri + "/" + rc.track[1], map[string]string{"Transport": transport}); err != nil {
			return errors.New(  "Unable to read SETUP response; connection lost")
		} else if status == 200 {
					rc.session = ParseSession(message)
					log.Printf("Session : %s", rc.session)
		}
	}
	

	// PHASE 4 PLAY		
	if status, _, err := rc.SendRequest("PLAY", rc.uri, map[string]string{"Range": "npt=0-"}); err != nil {
		return errors.New( "Unable to read PLAY response; connection lost")
	} else if status == 200 {
		go rc.RtspRtpLoop()		
		return nil
	}
	return errors.New(  "other error")
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
		// Recover from panics to avoid crashes
		if r := recover(); r != nil {
			log.Printf("[RTSP] - Recovered from panic in RtspRtpLoop: %v", r)
		}
		// Signal the end of the loop
		rc.signals <- true
	}()
	
	header := make([]byte, 4)
	payload := make([]byte, 1600)
	
	timer := time.Now()
	keepaliveInterval := 50 * time.Second
	reconnectAttempts := 0
	maxReconnectAttempts := 5
	
	for {
		// Send OPTIONS request periodically to keep the connection alive
		if time.Since(timer) > keepaliveInterval {
			if _, _, err := rc.SendRequest("OPTIONS", rc.uri, nil); err != nil {
				log.Printf("[RTSP] - Keepalive failed: %v", err)
				
				// Reconnection attempt with exponential backoff
				reconnectAttempts++
				if reconnectAttempts > maxReconnectAttempts {
					log.Printf("[RTSP] - Max reconnect attempts reached (%d), giving up", maxReconnectAttempts)
					return
				}
				
				// Wait before retrying (exponential backoff)
				backoffTime := time.Duration(1<<uint(reconnectAttempts-1)) * time.Second
				if backoffTime > 30*time.Second {
					backoffTime = 30 * time.Second // Cap at 30 seconds
				}
				
				log.Printf("[RTSP] - Reconnect attempt %d/%d in %v", reconnectAttempts, maxReconnectAttempts, backoffTime)
				time.Sleep(backoffTime)
				continue
			}
			
			// Reset timer and attempt counter after successful keepalive
			timer = time.Now()
			reconnectAttempts = 0
		}
		
		// Read TCP packets
		if err := rc.handleTCPPackets(header, payload); err != nil {
			log.Printf("[RTSP] - TCP read error: %v", err)
			return
		}
	}
}

// Preallocated buffer for complete RTP packets (header + payload)
// This avoids repeated allocations that create pressure on the GC
var rtpPacketPool = sync.Pool{
	New: func() interface{} {
		// Preallocate a buffer large enough for most packets
		return make([]byte, 0, 2048)
	},
}

// Method to handle TCP packets
func (rc *RtspClient) handleTCPPackets(header []byte, payload []byte) error {
	// Set a timeout for reading
	rc.socket.SetDeadline(time.Now().Add(1 * time.Second))
	
	// Read the interleaved header
	n, err := io.ReadFull(rc.socket, header)
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
		log.Println("[RTSP] - Unexpected start byte; synchronization lost")
		// Try to resynchronize by reading byte by byte until finding 0x24
		return nil
	}
	
	// Extract the payload length from the header
	payloadLen := int(header[2])<<8 + int(header[3])
	if payloadLen > len(payload) {
		return fmt.Errorf("payload length too large: %d > %d", payloadLen, len(payload))
	}
	
	if payloadLen < 12 {
		log.Println("[RTSP] - Invalid payload length; discarding packet")
		return nil
	}
	
	// Read the RTP payload
	n, err = io.ReadFull(rc.socket, payload[:payloadLen])
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
	default:
		// Channel full, drop this packet to avoid blocking
		// Return the buffer to the pool for reuse
		rtpPacketPool.Put(rtpPacket)
		log.Println("[RTSP] - Channel full, dropping packet")
	}
	
	return nil
}


func (rc *RtspClient) Connect() bool {
	// Longer timeout for slow or unstable networks
	d := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.Dial("tcp", rc.host+":"+rc.port)
	if err != nil {
		log.Printf("[RTSP] - Connection error: %v", err)
		return false
	}
	
	// Set read/write timeout to avoid blocking
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(30 * time.Second)
		tcpConn.SetNoDelay(true) // Disable Nagle's algorithm to reduce latency
	}
	
	rc.socket = conn
	return true
}


// Reusable buffer for RTSP requests
var requestBufferPool = sync.Pool{
	New: func() interface{} {
		return new(strings.Builder)
	},
}

// Reusable buffer for RTSP responses
var responseBufferPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, 4096)
	},
}

func (rc *RtspClient) SendRequest(requestType string, uri string, content map[string]string) (int, string, error) {
	rc.cseq += 1
	
	// Get a buffer from the pool to build the request
	sb := requestBufferPool.Get().(*strings.Builder)
	sb.Reset() // Reset the buffer
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
		log.Println("[RTSP] - Sending OPTIONS request")
	} else {
		log.Printf("[RTSP] - Sending %s request", requestType)
	}
	
	if _, e := rc.socket.Write([]byte(message)); e != nil {
		log.Println("[RTSP] - Socket write failed:", e)
		return 0, "", e
	}	
	
	// Get a buffer from the pool for the response
	buffer := responseBufferPool.Get().([]byte)
	defer responseBufferPool.Put(buffer)
	
	if nb, err := rc.socket.Read(buffer); err != nil || nb <= 0 {
		log.Println("[RTSP] - Socket read failed:", err)
		return 0, "", err
	} else {
		result := string(buffer[:nb])
		
		// Reduce logging - don't display the full content of responses
		if isOptions {
			log.Println("[RTSP] - Received OPTIONS response")
		} else {
			log.Printf("[RTSP] - Received %s response with status code %s", requestType, result[9:12])
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
	
			} else if strings.Contains(result, "Basic") {
				rc.auth = "Authorization: Basic " + b64.StdEncoding.EncodeToString([]byte(rc.login+":"+rc.password))
			}
			// resend request 
		}

		// extract status code : RTSP/1.0 200 OK convert to int
		statusCode := 0
		if len(result) >= 12 {
			statusCode, _ = strconv.Atoi(result[9:12])
		}

		return statusCode, result, nil
	}

	return 0, "", errors.New("unknown error")
}

func (rc *RtspClient) ParseUrl(rtsp_url string) bool {
	u, err := url.Parse(rtsp_url)
	if err != nil {
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
	log.Printf("host: %s, port: %s, login: %s, password: %s", rc.host, rc.port, rc.login, rc.password)
	rc.uri = "rtsp://" + rc.host + ":" + rc.port + u.Path
	if u.RawQuery != "" {
		rc.uri += "?" + string(u.RawQuery)
	} 
	return true
}

func (rc *RtspClient) Close() {
	// Properly close the RTSP connection
	if rc.socket != nil {
		// Send TEARDOWN to inform the RTSP server
		if rc.session != "" {
			// Ignore errors since we're closing the connection anyway
			rc.SendRequest("TEARDOWN", rc.uri, nil)
		}
		
		// Close the TCP socket
		rc.socket.Close()
		rc.socket = nil
	}
	
	// Clear channels to avoid memory leaks
	// Empty the received channel without blocking
	select {
	case <-rc.received:
		// Empty one message
	default:
		// Channel already empty
	}
	
	log.Println("[RTSP] - Resources released")
}

func ParseDirective(header, name string) string {
	start := strings.Index(header, name + `="`)
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
	controlPrefix     = "a=control:"
	dimensionsPrefix  = "a=x-dimensions:"
	controlPrefixLen  = len(controlPrefix)
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
			}
			
			// Return the slice to the pool
			dimsPool.Put(dims)
		}
	}
	
	return letters
}
