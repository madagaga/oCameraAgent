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
		received: make(chan []byte, 5000), // réduit de 100000 à 5000 pour économiser la mémoire
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
		// Récupérer les panics pour éviter les crashs
		if r := recover(); r != nil {
			log.Printf("[RTSP] - Recovered from panic in RtspRtpLoop: %v", r)
		}
		// Signaler la fin de la boucle
		rc.signals <- true
	}()
	
	header := make([]byte, 4)
	payload := make([]byte, 1600)
	
	timer := time.Now()
	keepaliveInterval := 50 * time.Second
	reconnectAttempts := 0
	maxReconnectAttempts := 5
	
	for {
		// Envoyer une requête OPTIONS périodiquement pour maintenir la connexion active
		if time.Since(timer) > keepaliveInterval {
			if _, _, err := rc.SendRequest("OPTIONS", rc.uri, nil); err != nil {
				log.Printf("[RTSP] - Keepalive failed: %v", err)
				
				// Tentative de reconnexion avec backoff exponentiel
				reconnectAttempts++
				if reconnectAttempts > maxReconnectAttempts {
					log.Printf("[RTSP] - Max reconnect attempts reached (%d), giving up", maxReconnectAttempts)
					return
				}
				
				// Attendre avant de réessayer (backoff exponentiel)
				backoffTime := time.Duration(1<<uint(reconnectAttempts-1)) * time.Second
				if backoffTime > 30*time.Second {
					backoffTime = 30 * time.Second // Plafonner à 30 secondes
				}
				
				log.Printf("[RTSP] - Reconnect attempt %d/%d in %v", reconnectAttempts, maxReconnectAttempts, backoffTime)
				time.Sleep(backoffTime)
				continue
			}
			
			// Réinitialiser le timer et le compteur de tentatives après un keepalive réussi
			timer = time.Now()
			reconnectAttempts = 0
		}
		
		// Lecture des paquets TCP
		if err := rc.handleTCPPackets(header, payload); err != nil {
			log.Printf("[RTSP] - TCP read error: %v", err)
			return
		}
	}
}

// Nouvelle méthode pour gérer les paquets TCP
func (rc *RtspClient) handleTCPPackets(header []byte, payload []byte) error {
	// Définir un timeout pour la lecture
	rc.socket.SetDeadline(time.Now().Add(1 * time.Second))
	
	// Lire l'en-tête interleaved
	n, err := io.ReadFull(rc.socket, header)
	if err != nil {
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			// Timeout de lecture - c'est normal, on continue
			return nil
		}
		return fmt.Errorf("failed to read interleaved header: %v", err)
	}
	
	if n != 4 {
		return fmt.Errorf("incomplete header read: %d bytes", n)
	}
	
	// Vérifier le marqueur RTP (0x24 en hex, ou 36 en décimal)
	if header[0] != 36 {
		log.Println("[RTSP] - Unexpected start byte; synchronization lost")
		// Tenter de resynchroniser en lisant octet par octet jusqu'à trouver 0x24
		return nil
	}
	
	// Extraire la longueur du payload depuis l'en-tête
	payloadLen := int(header[2])<<8 + int(header[3])
	if payloadLen > len(payload) {
		return fmt.Errorf("payload length too large: %d > %d", payloadLen, len(payload))
	}
	
	if payloadLen < 12 {
		log.Println("[RTSP] - Invalid payload length; discarding packet")
		return nil
	}
	
	// Lire le payload RTP
	n, err = io.ReadFull(rc.socket, payload[:payloadLen])
	if err != nil {
		return fmt.Errorf("error reading RTP payload: %v", err)
	}
	
	if n != payloadLen {
		return fmt.Errorf("incomplete payload read: %d/%d bytes", n, payloadLen)
	}
	
	// Envoyer le paquet complet (en-tête + payload) au canal
	rtpPacket := append(header, payload[:payloadLen]...)
	select {
	case rc.received <- rtpPacket:
		// Paquet envoyé avec succès
	default:
		// Canal plein, on ignore ce paquet pour éviter de bloquer
		log.Println("[RTSP] - Channel full, dropping packet")
	}
	
	return nil
}


func (rc *RtspClient) Connect() bool {
	// Timeout plus long pour les réseaux lents ou instables
	d := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.Dial("tcp", rc.host+":"+rc.port)
	if err != nil {
		log.Printf("[RTSP] - Connection error: %v", err)
		return false
	}
	
	// Définir un timeout de lecture/écriture pour éviter les blocages
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(30 * time.Second)
		tcpConn.SetNoDelay(true) // Désactiver l'algorithme de Nagle pour réduire la latence
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


	// Réduction des logs pour diminuer la charge CPU
	if strings.Contains(message, "OPTIONS") {
		// Ne pas logger les requêtes OPTIONS qui sont fréquentes
		log.Println("[RTSP] - Sending OPTIONS request")
	} else {
		log.Printf("[RTSP] - Sending %s request", requestType)
	}
	
	if _, e := rc.socket.Write([]byte(message + "\r\n")); e != nil {
		log.Println("[RTSP] - Socket write failed:", e)
		return 0, "", e
	}	
	
	buffer := make([]byte, 4096)
	if nb, err := rc.socket.Read(buffer); err != nil || nb <= 0 {
		log.Println("[RTSP] - Socket read failed:", err)
		return 0, "", err
	} else {
		result := string(buffer[:nb])
		
		// Réduction des logs - ne pas afficher le contenu complet des réponses
		if strings.Contains(message, "OPTIONS") {
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
	// Fermer proprement la connexion RTSP
	if rc.socket != nil {
		// Envoyer TEARDOWN pour informer le serveur RTSP
		if rc.session != "" {
			// Ignorer les erreurs car on va fermer la connexion de toute façon
			rc.SendRequest("TEARDOWN", rc.uri, nil)
		}
		
		// Fermer la socket TCP
		rc.socket.Close()
		rc.socket = nil
	}
	
	// Vider les canaux pour éviter les fuites de mémoire
	// Vider le canal received sans bloquer
	select {
	case <-rc.received:
		// Vider un message
	default:
		// Canal déjà vide
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
