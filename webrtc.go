package main

import (
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

// disconnectGrace: time tolerated in `disconnected` state before declaring the connection lost (§ 7).
const disconnectGrace = 10 * time.Second

// SessionError carries a protocol error code (§ 6).
type SessionError struct {
	Code string
	Msg  string
}

func (e *SessionError) Error() string { return e.Msg }

func newSessionError(code, msg string) *SessionError { return &SessionError{Code: code, Msg: msg} }

// WebRTCProxy handles ONE session: a PeerConnection and its RTSP stream. A new object is
// created for each accepted offer.
type WebRTCProxy struct {
	rtspUrl    string
	videoCodec string
	audioCodec string
	stun       []string

	videoTrack *webrtc.TrackLocalStaticRTP
	audioTrack *webrtc.TrackLocalStaticRTP

	mu              sync.Mutex // protects the fields below
	pc              *webrtc.PeerConnection
	rtspClient      *RtspClient
	closed          bool
	started         bool // the RTSP stream has been started
	disconnectTimer *time.Timer

	done      chan struct{} // closed by Close()
	closeOnce sync.Once

	// onLocalICE receives each local candidate; nil signals the end of gathering.
	onLocalICE func(candidate *webrtc.ICECandidate)
	// onConnected is called when the WebRTC connection is established.
	onConnected func()
	// onEnd is called when the session cannot continue (ByeICEFailed, ByeSourceLost).
	onEnd func(reason string)
}

// NewWebRTCProxy creates the proxy of one session. stun: the agent's STUN servers (config ice.stun).
func NewWebRTCProxy(device Device, stun []string) *WebRTCProxy {
	Info("WEBRTC", "Session proxy created for %s", redactURL(device.URL))
	return &WebRTCProxy{
		rtspUrl:    device.URL,
		audioCodec: "audio/" + device.Audio,
		videoCodec: "video/" + device.Video,
		stun:       stun,
		done:       make(chan struct{}),
	}
}

// Close terminates the WebRTC and RTSP connections. Idempotent and safe from several goroutines.
func (wp *WebRTCProxy) Close() {
	wp.closeOnce.Do(func() {
		close(wp.done)

		wp.mu.Lock()
		wp.closed = true
		pc := wp.pc
		client := wp.rtspClient
		wp.pc = nil
		wp.rtspClient = nil
		if wp.disconnectTimer != nil {
			wp.disconnectTimer.Stop()
		}
		wp.mu.Unlock()

		if pc != nil {
			Info("WEBRTC", "Closing WebRTC connection...")
			pc.Close()
		}
		if client != nil {
			Info("RTSP", "Disconnecting from RTSP...")
			client.Close()
		}
	})
}

func (wp *WebRTCProxy) isClosed() bool {
	wp.mu.Lock()
	defer wp.mu.Unlock()
	return wp.closed
}

// HandleOffer creates the PeerConnection, applies the offer and returns the answer. Errors are
// *SessionError (protocol codes). On error, the proxy is closed.
func (wp *WebRTCProxy) HandleOffer(offer webrtc.SessionDescription, wantAudio bool) (*webrtc.SessionDescription, error) {
	videoName := strings.ToLower(strings.TrimPrefix(wp.videoCodec, "video/"))
	lowerSDP := strings.ToLower(offer.SDP)
	if !strings.Contains(lowerSDP, " "+videoName+"/") {
		return nil, newSessionError(ErrUnsupportedCodec, "the offer does not contain video codec "+videoName)
	}
	addAudio := wantAudio && strings.Contains(lowerSDP, "m=audio")

	answer, err := wp.createAnswer(offer, addAudio)
	if err != nil {
		wp.Close()
		return nil, err
	}
	return answer, nil
}

func (wp *WebRTCProxy) createAnswer(offer webrtc.SessionDescription, addAudio bool) (*webrtc.SessionDescription, error) {
	config := webrtc.Configuration{}
	if len(wp.stun) > 0 {
		config.ICEServers = []webrtc.ICEServer{{URLs: wp.stun}}
	}

	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		Error("WEBRTC", "Error while creating PeerConnection: %v", err)
		return nil, newSessionError(ErrInternal, "cannot create the PeerConnection")
	}

	wp.mu.Lock()
	if wp.closed {
		wp.mu.Unlock()
		pc.Close()
		return nil, newSessionError(ErrInternal, "session closed")
	}
	wp.pc = pc
	wp.mu.Unlock()

	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate != nil {
			Debug("WEBRTC", "Local ICE candidate: %v", candidate)
		}
		if wp.onLocalICE != nil {
			wp.onLocalICE(candidate)
		}
	})
	pc.OnConnectionStateChange(wp.handleConnectionState)
	pc.OnSignalingStateChange(func(state webrtc.SignalingState) {
		Debug("WEBRTC", "Signaling state: %s", state.String())
	})

	wp.videoTrack, err = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: wp.videoCodec}, "video", "rtsp-video")
	if err != nil {
		Error("WEBRTC", "Error creating video track: %v", err)
		return nil, newSessionError(ErrUnsupportedCodec, "unsupported video codec")
	}
	if _, err = pc.AddTrack(wp.videoTrack); err != nil {
		Error("WEBRTC", "Error adding video track: %v", err)
		return nil, newSessionError(ErrInternal, "cannot add the video track")
	}

	if addAudio {
		wp.audioTrack, err = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: wp.audioCodec}, "audio", "rtsp-audio")
		if err != nil {
			Warn("WEBRTC", "Audio track not added: %v", err)
			wp.audioTrack = nil
		} else if _, err = pc.AddTrack(wp.audioTrack); err != nil {
			Error("WEBRTC", "Error adding audio track: %v", err)
			return nil, newSessionError(ErrInternal, "cannot add the audio track")
		}
	}

	if err := pc.SetRemoteDescription(offer); err != nil {
		Error("WEBRTC", "Error setting remote description: %v", err)
		return nil, newSessionError(ErrBadRequest, "offre SDP invalide")
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		Error("WEBRTC", "Error while creating answer: %v", err)
		return nil, newSessionError(ErrInternal, "cannot create the answer")
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		Error("WEBRTC", "Error while setting local description: %v", err)
		return nil, newSessionError(ErrInternal, "cannot apply the answer")
	}

	return pc.CurrentLocalDescription(), nil
}

func (wp *WebRTCProxy) handleConnectionState(state webrtc.PeerConnectionState) {
	Info("WEBRTC", "Connection state: %s", state.String())

	wp.mu.Lock()
	if wp.closed {
		wp.mu.Unlock()
		return
	}
	pc := wp.pc
	if state != webrtc.PeerConnectionStateDisconnected && wp.disconnectTimer != nil {
		wp.disconnectTimer.Stop()
		wp.disconnectTimer = nil
	}
	wp.mu.Unlock()

	switch state {
	case webrtc.PeerConnectionStateFailed:
		wp.end(ByeICEFailed)

	case webrtc.PeerConnectionStateDisconnected:
		wp.mu.Lock()
		if wp.disconnectTimer == nil && !wp.closed {
			wp.disconnectTimer = time.AfterFunc(disconnectGrace, func() { wp.end(ByeICEFailed) })
		}
		wp.mu.Unlock()

	case webrtc.PeerConnectionStateConnected:
		wp.mu.Lock()
		first := !wp.started
		wp.started = true
		wp.mu.Unlock()
		if !first {
			return
		}
		if pc != nil {
			wp.logSelectedICEPairNetwork(pc)
		}
		go wp.StartRTSPStream()
		if wp.onConnected != nil {
			wp.onConnected()
		}
	}
}

func (wp *WebRTCProxy) end(reason string) {
	if wp.isClosed() {
		return
	}
	if wp.onEnd != nil {
		wp.onEnd(reason)
	} else {
		wp.Close()
	}
}

// AddICECandidate ajoute un candidat distant.
func (wp *WebRTCProxy) AddICECandidate(candidate webrtc.ICECandidateInit) error {
	wp.mu.Lock()
	pc := wp.pc
	wp.mu.Unlock()
	if pc == nil {
		return errors.New("PeerConnection is null")
	}
	if err := pc.AddICECandidate(candidate); err != nil {
		return err
	}
	Debug("WEBRTC", "Remote ICE candidate added")
	return nil
}

// sendRTPPacket forwards an RTP packet extracted from RTSP interleaved TCP.
//
// The RTSP client keeps the camera packetization intact; Pion rewrites only the
// SSRC/PayloadType negotiated for the WebRTC sender.
func (wp *WebRTCProxy) sendRTPPacket(data []byte) error {
	if len(data) <= 4 {
		return nil
	}

	channel := data[1]
	packetBytes := data[4:]

	switch channel {
	case 0:
		if wp.videoTrack == nil {
			return nil
		}
		_, err := wp.videoTrack.Write(packetBytes)
		return err
	case 2:
		if wp.audioTrack == nil {
			return nil
		}
		_, err := wp.audioTrack.Write(packetBytes)
		return err
	default:
		return nil
	}
}

// StartRTSPStream starts the RTSP stream with automatic reconnection on failure.
// When attempts are exhausted, the session ends with reason `source_lost`.
func (wp *WebRTCProxy) StartRTSPStream() {
	const maxAttempts = 5

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			Warn("RTSP", "Reconnect attempt %d/%d in %v", attempt, maxAttempts, backoff)
			select {
			case <-time.After(backoff):
			case <-wp.done:
				return
			}
		}

		Info("RTSP", "Connecting to RTSP %s...", redactURL(wp.rtspUrl))
		client := NewRtspClient()

		wp.mu.Lock()
		if wp.closed {
			wp.mu.Unlock()
			return
		}
		wp.rtspClient = client
		wp.mu.Unlock()

		if err := client.Client(wp.rtspUrl, false); err != nil {
			Error("RTSP", "Connection error: %v", err)
			client.Close()
			continue
		}

		Info("RTSP", "Connected")
		wp.consumeRTPPackets(client)

		if wp.isClosed() {
			return
		}
		client.Close()
		Warn("RTSP", "RTSP session ended, will reconnect")
	}

	Error("RTSP", "Max reconnect attempts reached, ending session")
	wp.end(ByeSourceLost)
}

// consumeRTPPackets forwards packets from the RTSP client to the WebRTC tracks
// until the RTSP session ends or the session is closed.
func (wp *WebRTCProxy) consumeRTPPackets(client *RtspClient) {
	packetCount := 0
	lastLogTime := time.Now()
	droppedCount := 0

	for {
		select {
		case data := <-client.received:
			if len(data) > 12 {
				if data[1] == 0 || data[1] == 2 {
					if err := wp.sendRTPPacket(data); err != nil {
						Error("RTSP", "Error sending RTP packet: %v", err)
					} else {
						packetCount++
					}

					if time.Since(lastLogTime) > 5*time.Second {
						Debug("RTSP", "Forwarded %d RTP packets in last 5 seconds", packetCount)
						packetCount = 0
						lastLogTime = time.Now()
						if droppedCount > 0 {
							Debug("RTSP", "%d packets were ignored in last 5 seconds", droppedCount)
							droppedCount = 0
						}
					}
				} else {
					droppedCount++
				}
			} else if len(data) > 0 {
				Debug("RTSP", "Packet too short to contain an RTP header")
				droppedCount++
			}
			rtpPacketPool.Put(data[:0])

		case <-client.signals:
			Info("RTSP", "Exit signal received from RTSP client")
			return

		case <-wp.done:
			return
		}
	}
}

// redactURL masks the credentials of an RTSP URL before logging.
func redactURL(raw string) string {
	scheme := strings.Index(raw, "://")
	at := strings.LastIndex(raw, "@")
	if scheme < 0 || at < scheme {
		return raw
	}
	return raw[:scheme+3] + "***@" + raw[at+1:]
}

func (wp *WebRTCProxy) logSelectedICEPairNetwork(pc *webrtc.PeerConnection) {
	pair, err := wp.getSelectedICECandidatePair(pc)
	if err != nil {
		Warn("WEBRTC", "Unable to read selected ICE pair: %v", err)
		return
	}
	if pair == nil || pair.Local == nil || pair.Remote == nil {
		Warn("WEBRTC", "Selected ICE pair is not available yet")
		return
	}

	localIP := net.ParseIP(pair.Local.Address)
	remoteIP := net.ParseIP(pair.Remote.Address)
	if localIP == nil || remoteIP == nil {
		Info(
			"WEBRTC",
			"ICE selected pair local=%s(%s) remote=%s(%s) same_network=unknown",
			pair.Local.Address,
			pair.Local.Typ.String(),
			pair.Remote.Address,
			pair.Remote.Typ.String(),
		)
		return
	}

	sameNetwork := pair.Local.Typ == webrtc.ICECandidateTypeHost &&
		pair.Remote.Typ == webrtc.ICECandidateTypeHost &&
		isPrivateOrLinkLocalIP(remoteIP) &&
		isInLocalSubnet(remoteIP)

	Info(
		"WEBRTC",
		"ICE selected pair local=%s(%s) remote=%s(%s) same_network=%t",
		pair.Local.Address,
		pair.Local.Typ.String(),
		pair.Remote.Address,
		pair.Remote.Typ.String(),
		sameNetwork,
	)
}

func (wp *WebRTCProxy) getSelectedICECandidatePair(pc *webrtc.PeerConnection) (*webrtc.ICECandidatePair, error) {
	for _, sender := range pc.GetSenders() {
		if sender == nil || sender.Transport() == nil || sender.Transport().ICETransport() == nil {
			continue
		}
		pair, err := sender.Transport().ICETransport().GetSelectedCandidatePair()
		if err != nil {
			return nil, err
		}
		if pair != nil {
			return pair, nil
		}
	}

	if pc.SCTP() != nil &&
		pc.SCTP().Transport() != nil &&
		pc.SCTP().Transport().ICETransport() != nil {
		return pc.SCTP().Transport().ICETransport().GetSelectedCandidatePair()
	}

	return nil, nil
}

func isPrivateOrLinkLocalIP(ip net.IP) bool {
	if ip4 := ip.To4(); ip4 != nil {
		switch {
		case ip4[0] == 10:
			return true
		case ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31:
			return true
		case ip4[0] == 192 && ip4[1] == 168:
			return true
		case ip4[0] == 169 && ip4[1] == 254:
			return true
		default:
			return false
		}
	}

	return ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

func isInLocalSubnet(remoteIP net.IP) bool {
	ifaces, err := net.Interfaces()
	if err != nil {
		return false
	}

	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet == nil || ipNet.IP == nil {
				continue
			}
			if (ipNet.IP.To4() == nil) != (remoteIP.To4() == nil) {
				continue
			}
			if ipNet.Contains(remoteIP) {
				return true
			}
		}
	}

	return false
}
