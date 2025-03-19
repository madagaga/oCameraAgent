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
	"time"
	"errors"
)

type RtspClient struct {
	socket   net.Conn
	rtpConn  *net.UDPConn  // UDP connection for RTP packets
	rtcpConn  *net.UDPConn  // UDP connection for RTP packets
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
	udp      bool
}
// returns an empty initialized object
func NewRtspClient() *RtspClient {
	Obj := &RtspClient{
		cseq:     1,                         // starting query number
		signals:  make(chan bool, 1),        // buffered channel for 1 message
		received: make(chan []byte, 100000), // buffered channel for 100,000 bytes
	}
	return Obj
}

// main function for working with RTSP
func (rc *RtspClient) Client(rtsp_url string, udp bool) error {
	// check and parse URL
	if !rc.ParseUrl(rtsp_url) {
		return errors.New( "Invalid URL")
	}

	rc.udp = udp
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

	if rc.udp {
		//var err error
		tmp, err := net.ListenPacket("udp4", "0.0.0.0:26968")
		if err != nil {
			panic(err)
		}
		rc.rtpConn = tmp.(*net.UDPConn)
		log.Printf("udp listenning on address %s", rc.rtpConn.LocalAddr().String())
		
		err = rc.rtpConn.SetReadBuffer(0x80000)
		if err != nil {
			panic(err)
		}
		rc.rtpConn.SetReadDeadline(time.Time{})
		// rc.rtcpConn, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("0.0.0.0"), Port: 26969})
		// if err != nil {
		// 	panic(err)
		// }
	}



	transport := "RTP/AVP/TCP;unicast;interleaved=0-1"
	if(rc.udp){
		transport = "RTP/AVP;unicast;client_port=26968-26969"
	}
	
	if status, message, err := rc.SendRequest("SETUP", rc.uri + "/" + rc.track[0], map[string]string{"Transport": transport}); err != nil {
		return errors.New( "Unable to read SETUP response; connection lost")
	} else if status == 200 {
			rc.session = ParseSession(message)
			log.Printf("Session : %s", rc.session)			
	}
	
	if len(rc.track) > 1 {
		transport := "RTP/AVP/TCP;unicast;interleaved=2-3"
		if(rc.udp){
			transport = "RTP/AVP;unicast;client_port=5002-5003"
		}

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
		rc.signals <- true
	}()
	header := make([]byte, 4)
	payload := make([]byte, 1600)
	
	timer := time.Now()
	for {
			if int(time.Now().Sub(timer).Seconds()) > 50 {				
				if _, _, err := rc.SendRequest("OPTIONS", rc.uri, nil); err != nil {
					return
				}
				timer = time.Now()
			}
			if rc.udp {
				
				// log.Println("UDP")
				// n, _, err := rc.rtcpConn.ReadFromUDP(payload)
				log.Println("UDP2 ")

				n, _, err := rc.rtpConn.ReadFrom(payload)
				if err != nil {
					panic(fmt.Sprintf("error during read: %s", err))
				}
				rc.received <- payload[:n]
				// Lecture des paquets RTCP
				n, addr, err := rc.rtcpConn.ReadFrom(payload)
				if err != nil {
					log.Printf("Erreur de lecture RTCP : %v", err)
					continue
				}
				log.Printf("Paquet RTCP reçu de %s : %d octets", addr, n)				
				
			} else {

				
				rc.socket.SetDeadline(time.Now().Add(50 * time.Second))
				// Read the 4-byte interleaved header
				if n, err := io.ReadFull(rc.socket, header); err != nil || n != 4 {
					log.Println("Failed to read interleaved header:", err)
					return
				}

				// Check for the RTP marker (0x24 in hex, or 36 in decimal)
				if header[0] != 36 {
					log.Println("Unexpected start byte; synchronization lost")
					continue
				}

				// Extract the payload length from the interleaved header
				payloadLen := int(header[2])<<8 + int(header[3])
				if payloadLen > len(payload) || payloadLen < 12 {
					log.Println("Invalid payload length; discarding packet")
					continue
				}

				// Read the actual RTP payload
				if n, err := io.ReadFull(rc.socket, payload[:payloadLen]); err != nil || n != payloadLen {
					log.Println("Error reading RTP payload:", err)
					return
				}

				
    			
				// Send the complete RTP packet (header + payload) to the received channel
				rtpPacket := append(header, payload[:payloadLen]...)
				rc.received <- rtpPacket
		}
	}
}


func (rc *RtspClient) Connect() bool {
	d := &net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.Dial("tcp", rc.host+":"+rc.port)
	if err != nil {
		return false
	}
	rc.socket = conn
	return true
}


func (rc *RtspClient) SendRequest(requestType string, uri string, content map[string]string) (int, string, error) {
	rc.cseq += 1
	message := requestType + " " + uri + " RTSP/1.0\r\nUser-Agent: rtsp-client\r\nAccept: application/sdp\r\nCSeq: " + strconv.Itoa(rc.cseq) + "\r\n"
	if rc.auth != "" {
		message += rc.auth + "\r\n"
	}
	for k, v := range content {
		message += k + ": " + v + "\r\n"
	}

	if rc.session != "" {
		message += "Session: " + rc.session + "\r\n"
	}


	log.Println("***** SENDING *****")
	log.Println(message)
	if _, e := rc.socket.Write([]byte(message + "\r\n")); e != nil {
		log.Println("socket write failed", e)
		return 0, "", e
	}	
	
	buffer := make([]byte, 4096)
	if nb, err := rc.socket.Read(buffer); err != nil || nb <= 0 {
		log.Println("socket read failed", err)
		return 0, "", err
	} else {
		result := string(buffer[:nb])
		log.Println("***** RECEIVED *****")
		log.Println(result)

		// check authentication method if not set 
		if rc.auth == "" {
			if strings.Contains(result, "Digest") {
				nonce := ParseDirective(result, "nonce")
				realm := ParseDirective(result, "realm")
				hs1 := GetMD5Hash(rc.login + ":" + realm + ":" + rc.password)
				hs2 := GetMD5Hash(requestType + ":" + rc.uri)
				response := GetMD5Hash(hs1 + ":" + nonce + ":" + hs2)
				rc.auth = `Authorization: Digest username="` + rc.login + `", realm="` + realm + `", nonce="` + nonce + `", uri="` + rc.uri + `", response="` + response + `"`
	
			} else if strings.Contains(result, "Basic") {
				rc.auth = "\r\nAuthorization: Basic " + b64.StdEncoding.EncodeToString([]byte(rc.login+":"+rc.password))	
			}
			// resend request 
		}

		// extract status code : RTSP/1.0 200 OK convert to int
		statusCode, _ := strconv.Atoi(result[9:9+3])

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
	if rc.socket != nil {
		rc.socket.Close()
	}

	if rc.rtpConn != nil {
		rc.rtpConn.Close()
	}
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
func (rc *RtspClient) ParseMedia(header string) []string {
	letters := []string{}
	mparsed := strings.Split(header, "\r\n")
	paste := ""
	for _, element := range mparsed {
		if strings.Contains(element, "a=control:") && !strings.Contains(element, "*") {
			paste = element[10:]
			if strings.Contains(element, "/") {
				striped := strings.Split(element, "/")
				paste = striped[len(striped)-1]
			}
			letters = append(letters, paste)
		}

		dimensionsPrefix := "a=x-dimensions:"
		if strings.HasPrefix(element, dimensionsPrefix) {
			dims := []int{}
			for _, s := range strings.Split(element[len(dimensionsPrefix):], ",") {
				v := 0
				fmt.Sscanf(s, "%d", &v)
				if v <= 0 {
					break
				}
				dims = append(dims, v)
			}
			if len(dims) == 2 {
				rc.videow = dims[0]
				rc.videoh = dims[1]
			}
		}
	}
	return letters
}