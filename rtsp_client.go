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
func (this *RtspClient) Client(rtsp_url string, udp bool) (bool, string) {
	// check and parse URL
	if !this.ParseUrl(rtsp_url) {
		return false, "Invalid URL"
	}

	this.udp = udp
	// establish connection to the camera
	if !this.Connect() {
		return false, "Unable to connect"
	}
	// phase 1 OPTIONS - first stage of communication with the camera
	// send OPTIONS request and read response to the OPTIONS request
	if _, _, err := this.SendRequest("OPTIONS", this.uri, nil); err != nil {
		return false, "Unable to read OPTIONS response; connection lost"
	}

	// PHASE 2 DESCRIBE		
	if status, message, err := this.SendRequest("DESCRIBE", this.uri, nil); err != nil {
		return false, "Unable to read DESCRIBE response; connection lost?"
	} else if status != 200 {
		return false, "DESCRIBE error; not status code 200 OK " + message
	} else {		
		this.track = this.ParseMedia(message)
	}
	if len(this.track) == 0 {
		return false, "Error; track not found"
	}

	// PHASE 3 SETUP

	if this.udp {
		//var err error
		tmp, err := net.ListenPacket("udp4", "0.0.0.0:26968")
		if err != nil {
			panic(err)
		}
		this.rtpConn = tmp.(*net.UDPConn)
		log.Printf("udp listenning on address %s", this.rtpConn.LocalAddr().String())
		
		err = this.rtpConn.SetReadBuffer(0x80000)
		if err != nil {
			panic(err)
		}
		this.rtpConn.SetReadDeadline(time.Time{})
		// this.rtcpConn, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("0.0.0.0"), Port: 26969})
		// if err != nil {
		// 	panic(err)
		// }
	}



	transport := "RTP/AVP/TCP;unicast;interleaved=0-1"
	if(this.udp){
		transport = "RTP/AVP;unicast;client_port=26968-26969"
	}
	
	if status, message, err := this.SendRequest("SETUP", this.uri + "/" + this.track[0], map[string]string{"Transport": transport}); err != nil {
		return false, "Unable to read SETUP response; connection lost"
	} else if status == 200 {
			this.session = ParseSession(message)
			log.Printf("Session : %s", this.session)			
	}
	
	if len(this.track) > 1 {
		transport := "RTP/AVP/TCP;unicast;interleaved=2-3"
		if(this.udp){
			transport = "RTP/AVP;unicast;client_port=5002-5003"
		}

		if status, message, err := this.SendRequest("SETUP", this.uri + "/" + this.track[1], map[string]string{"Transport": transport}); err != nil {
			return false, "Unable to read SETUP response; connection lost"
		} else if status == 200 {
					this.session = ParseSession(message)
					log.Printf("Session : %s", this.session)
		}
	}
	

	// PHASE 4 PLAY		
	if status, _, err := this.SendRequest("PLAY", this.uri, map[string]string{"Range": "npt=0-"}); err != nil {
		return false, "Unable to read PLAY response; connection lost"
	} else if status == 200 {
		go this.RtspRtpLoop()		
		return true, "ok"
	}
	return false, "other error"
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
func (this *RtspClient) RtspRtpLoop() {
	defer func() {
		this.signals <- true
	}()
	header := make([]byte, 4)
	payload := make([]byte, 1600)
	
	timer := time.Now()
	for {
			if int(time.Now().Sub(timer).Seconds()) > 50 {				
				if _, _, err := this.SendRequest("OPTIONS", this.uri, nil); err != nil {
					return
				}
				timer = time.Now()
			}
			if this.udp {
				
				// log.Println("UDP")
				// n, _, err := this.rtcpConn.ReadFromUDP(payload)
				log.Println("UDP2 ")

				n, _, err := this.rtpConn.ReadFrom(payload)
				if err != nil {
					panic(fmt.Sprintf("error during read: %s", err))
				}
				this.received <- payload[:n]
				// Lecture des paquets RTCP
				n, addr, err := this.rtcpConn.ReadFrom(payload)
				if err != nil {
					log.Printf("Erreur de lecture RTCP : %v", err)
					continue
				}
				log.Printf("Paquet RTCP reçu de %s : %d octets", addr, n)				
				
			} else {

				
				this.socket.SetDeadline(time.Now().Add(50 * time.Second))
				// Read the 4-byte interleaved header
				if n, err := io.ReadFull(this.socket, header); err != nil || n != 4 {
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
				if n, err := io.ReadFull(this.socket, payload[:payloadLen]); err != nil || n != payloadLen {
					log.Println("Error reading RTP payload:", err)
					return
				}

				
    			
				// Send the complete RTP packet (header + payload) to the received channel
				rtpPacket := append(header, payload[:payloadLen]...)
				this.received <- rtpPacket
		}
	}
}

// unsafe!
func (this *RtspClient) SendBufer(buffer []byte) {
	// send all packets from the buffer
	payload := make([]byte, 4096)
	for {
		if len(buffer) < 4 {
			log.Fatal("buffer too small")
		}
		dataLength := (int)(buffer[2])<<8 + (int)(buffer[3])
		if dataLength > len(buffer)+4 {
			if n, err := io.ReadFull(this.socket, payload[:dataLength-len(buffer)+4]); err != nil {
				return
			} else {
				this.received <- append(buffer, payload[:n]...)
				return
			}
		} else {
			this.received <- buffer[:dataLength+4]
			buffer = buffer[dataLength+4:]
		}
	}
}

func (this *RtspClient) Connect() bool {
	d := &net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.Dial("tcp", this.host+":"+this.port)
	if err != nil {
		return false
	}
	this.socket = conn
	return true
}


func (this *RtspClient) SendRequest(requestType string, uri string, content map[string]string) (int, string, error) {
	this.cseq += 1
	message := requestType + " " + uri + " RTSP/1.0\r\nUser-Agent: rtsp-client\r\nAccept: application/sdp\r\nCSeq: " + strconv.Itoa(this.cseq) + "\r\n"
	if this.auth != "" {
		message += this.auth + "\r\n"
	}
	for k, v := range content {
		message += k + ": " + v + "\r\n"
	}

	if this.session != "" {
		message += "Session: " + this.session + "\r\n"
	}


	log.Println("***** SENDING *****")
	log.Println(message)
	if _, e := this.socket.Write([]byte(message + "\r\n")); e != nil {
		log.Println("socket write failed", e)
		return 0, "", e
	}	
	
	buffer := make([]byte, 4096)
	if nb, err := this.socket.Read(buffer); err != nil || nb <= 0 {
		log.Println("socket read failed", err)
		return 0, "", err
	} else {
		result := string(buffer[:nb])
		log.Println("***** RECEIVED *****")
		log.Println(result)

		// check authentication method if not set 
		if this.auth == "" {
			if strings.Contains(result, "Digest") {
				nonce := ParseDirective(result, "nonce")
				realm := ParseDirective(result, "realm")
				hs1 := GetMD5Hash(this.login + ":" + realm + ":" + this.password)
				hs2 := GetMD5Hash(requestType + ":" + this.uri)
				response := GetMD5Hash(hs1 + ":" + nonce + ":" + hs2)
				this.auth = `Authorization: Digest username="` + this.login + `", realm="` + realm + `", nonce="` + nonce + `", uri="` + this.uri + `", response="` + response + `"`
	
			} else if strings.Contains(result, "Basic") {
				this.auth = "\r\nAuthorization: Basic " + b64.StdEncoding.EncodeToString([]byte(this.login+":"+this.password))	
			}

			// resend request 

		}

		// extract status code : RTSP/1.0 200 OK convert to int
		statusCode, _ := strconv.Atoi(result[9:9+3])




		return statusCode, result, nil
	}

	return 0, "", errors.New("unknown error")


}

func (this *RtspClient) ParseUrl(rtsp_url string) bool {
	u, err := url.Parse(rtsp_url)
	if err != nil {
		return false
	}
	phost := strings.Split(u.Host, ":")
	this.host = phost[0]
	if len(phost) == 2 {
		this.port = phost[1]
	} else {
		this.port = "554"
	}
	this.login = u.User.Username()
	this.password, _ = u.User.Password()
	this.uri = "rtsp://" + this.host + ":" + this.port + u.Path
	if u.RawQuery != "" {
		this.uri += "?" + string(u.RawQuery)
	} 
	return true
}

func (this *RtspClient) Close() {
	if this.socket != nil {
		this.socket.Close()
	}

	if this.rtpConn != nil {
		this.rtpConn.Close()
	}
}

func ParseDirective(header, name string) string {
	start := strings.Index(header, name + `:"`)
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
func (this *RtspClient) ParseMedia(header string) []string {
	letters := []string{}
	mparsed := strings.Split(header, "\r\n")
	paste := ""
	for _, element := range mparsed {
		if strings.Contains(element, "a=control:") && !strings.Contains(element, "*") && strings.Contains(element, "tra") {
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
				this.videow = dims[0]
				this.videoh = dims[1]
			}
		}
	}
	return letters
}