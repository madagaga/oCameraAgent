package main

import (
	"errors"
	"net"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

type WebRTCProxy struct {
	peerConnection *webrtc.PeerConnection
	videoTrack     *webrtc.TrackLocalStaticRTP
	audioTrack     *webrtc.TrackLocalStaticRTP

	rtspUrl    string
	rtspClient *RtspClient

	videoCodec string
	audioCodec string

	closeOnce sync.Once

	// pendingCandidates holds ICE candidates that arrived before SetRemoteDescription.
	mu                sync.Mutex
	pendingCandidates []webrtc.ICECandidateInit
	remoteDescSet     bool

	onLocalICECandidateReceived OnLocalICECandidateReceivedFunc
	onConnectionStateChanged    OnConnectionStateChangedFunc
}

type OnLocalICECandidateReceivedFunc func(source string, candidate *webrtc.ICECandidate)
type OnConnectionStateChangedFunc func(conencted bool)

// NewWebRTCProxy creates a new WebRTCProxy object.
//
// The created object is configured to connect to the specified RTSP URL
// and to handle the WebRTC peer connection.
//
// The object is not started automatically, the Start() method must be
// called to start the proxy.
func NewWebRTCProxy(device Device) *WebRTCProxy {
	// log the URL to the console
	Info("WEBRTC", "URL: %s", device.URL)

	// create a new object
	Obj := &WebRTCProxy{
		// set the RTSP URL
		rtspUrl:    device.URL,
		audioCodec: "audio/" + device.Audio,
		videoCodec: "video/" + device.Video,
	}

	// return the object
	return Obj
}

// OnLocalICECandidateReceived sets a callback for when a local ICE candidate is received
//
// The callback is called with the source of the candidate and the candidate itself.
// The source is the identifier of the peer that sent the candidate.
func (wp *WebRTCProxy) OnLocalICECandidateReceived(callback OnLocalICECandidateReceivedFunc) {
	wp.onLocalICECandidateReceived = callback
}

// OnConnectionStateChanged sets a callback for when the connection state of the peer connection changes
//
// The callback is called with a boolean argument indicating whether the connection is established or not.
func (wp *WebRTCProxy) OnConnectionStateChanged(callback OnConnectionStateChangedFunc) {
	wp.onConnectionStateChanged = callback
}

// Close terminates the WebRTC and RTSP connections. Safe to call from multiple goroutines;
// the actual teardown runs exactly once per session.
func (wp *WebRTCProxy) Close() {
	wp.closeOnce.Do(func() {
		if wp.peerConnection != nil {
			Info("WEBRTC", "Closing WebRTC connection...")
			wp.peerConnection.Close()
			wp.peerConnection = nil
		}

		if wp.rtspClient != nil {
			Info("RTSP", "Disconnecting from RTSP...")
			wp.rtspClient.Close()
			wp.rtspClient = nil
		}

		if wp.onConnectionStateChanged != nil {
			wp.onConnectionStateChanged(false)
		}
	})
}

// CreatePeerConnection creates a new PeerConnection and adds the video and audio tracks to it
//
// The method is called when the HandleOffer method is called and a new PeerConnection needs to be created.
func (wp *WebRTCProxy) CreatePeerConnection(from string) error {
	_ = from
	wp.closeOnce = sync.Once{} // reset for new session
	Info("WEBRTC", "Using standard ICE configuration with STUN servers")
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun1.l.google.com:19302", "stun:stun2.l.google.com:19302"},
			},
		},
	}

	var err error
	wp.peerConnection, err = webrtc.NewPeerConnection(config)
	if err != nil {
		Error("WEBRTC", "Error while creating PeerConnection: %v", err)
		return err
	}

	// Set a callback for when an ICE candidate is received
	wp.peerConnection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate != nil {
			Debug("WEBRTC", "ICE candidate received: %v", candidate)
			if wp.onLocalICECandidateReceived != nil {
				wp.onLocalICECandidateReceived(from, candidate)
			}
		}
	})

	// Set a callback for when the connection state of the peer connection changes
	wp.peerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		Info("WEBRTC", "Connection state: %s", state.String())
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateDisconnected {
			wp.Close()
		} else if state == webrtc.PeerConnectionStateConnected {
			wp.logSelectedICEPairNetwork()
			go wp.StartRTSPStream()
			if wp.onConnectionStateChanged != nil {
				wp.onConnectionStateChanged(true)
			}
		}
	})

	// Set a callback for when the signaling state of the peer connection changes
	wp.peerConnection.OnSignalingStateChange(func(state webrtc.SignalingState) {
		Debug("WEBRTC", "Signaling state: %s", state.String())
	})

	// Create a new video track and add it to the peer connection
	wp.videoTrack, err = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: wp.videoCodec}, "video", "rtsp-video")
	if err != nil {
		Error("WEBRTC", "Error creating video track: %v", err)
		return err
	}

	_, err = wp.peerConnection.AddTrack(wp.videoTrack)
	if err != nil {
		Error("WEBRTC", "Error adding video track: %v", err)
		return err
	}

	// Create a new audio track and add it to the peer connection
	wp.audioTrack, err = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: wp.audioCodec}, "audio", "rtsp-audio")
	if err == nil {
		_, err = wp.peerConnection.AddTrack(wp.audioTrack)
		if err != nil {
			Error("WEBRTC", "Error adding audio track: %v", err)
			return err
		}
	} else {
		Warn("WEBRTC", "Audio track not added: %v", err)
	}

	return nil
}

// AddICECandidate adds an ICE candidate, queuing it if SetRemoteDescription has not yet been called.
func (wp *WebRTCProxy) AddICECandidate(candidate webrtc.ICECandidateInit) error {
	wp.mu.Lock()
	defer wp.mu.Unlock()

	if wp.peerConnection == nil {
		Error("WEBRTC", "PeerConnection is null")
		return errors.New("PeerConnection is null")
	}

	if !wp.remoteDescSet {
		Debug("WEBRTC", "Remote description not set yet, queuing ICE candidate")
		wp.pendingCandidates = append(wp.pendingCandidates, candidate)
		return nil
	}

	if err := wp.peerConnection.AddICECandidate(candidate); err != nil {
		Error("WEBRTC", "Error while adding ICE candidate: %v", err)
		return err
	}

	Debug("WEBRTC", "ICE candidate added successfully")
	return nil
}

// HandleOffer handles an offer from a peer and returns an answer to the peer.
//
// If the PeerConnection already exists, an error is returned.
// If the PeerConnection does not exist, the method creates a new PeerConnection and sets the remote description.
// Then, it creates an answer and sets the local description.
// The method returns the answer to the caller.
func (wp *WebRTCProxy) HandleOffer(from string, offer webrtc.SessionDescription) (*webrtc.SessionDescription, error) {
	if wp.peerConnection != nil {
		Warn("WEBRTC", "PeerConnection already exists")
		return nil, errors.New("PeerConnection already exists")
	}

	// Reset ICE candidate queue for the new session.
	wp.mu.Lock()
	wp.pendingCandidates = wp.pendingCandidates[:0]
	wp.remoteDescSet = false
	wp.mu.Unlock()

	if err := wp.CreatePeerConnection(from); err != nil {
		Error("WEBRTC", "Error creating peer connection: %v", err)
		return nil, err
	}
	Info("WEBRTC", "PeerConnection created successfully")

	if err := wp.peerConnection.SetRemoteDescription(offer); err != nil {
		Error("WEBRTC", "Error setting remote description: %v", err)
		return nil, err
	}
	Debug("WEBRTC", "SetRemoteDescription successful")

	// Mark remote description as set and flush any queued ICE candidates.
	wp.mu.Lock()
	wp.remoteDescSet = true
	pending := wp.pendingCandidates
	wp.pendingCandidates = nil
	wp.mu.Unlock()

	for _, c := range pending {
		if err := wp.peerConnection.AddICECandidate(c); err != nil {
			Warn("WEBRTC", "Error adding queued ICE candidate: %v", err)
		}
	}
	if len(pending) > 0 {
		Debug("WEBRTC", "Flushed %d queued ICE candidates", len(pending))
	}

	answer, err := wp.peerConnection.CreateAnswer(nil)
	if err != nil {
		Error("WEBRTC", "Error while creating answer: %v", err)
		return nil, err
	}
	Debug("WEBRTC", "CreateAnswer successful")

	if err := wp.peerConnection.SetLocalDescription(answer); err != nil {
		Error("WEBRTC", "Error while setting local description: %v", err)
		wp.Close()
		return nil, err
	}
	Debug("WEBRTC", "SetLocalDescription successful")

	return wp.peerConnection.CurrentLocalDescription(), nil
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
	case 1, 3:
		// RTCP interleaved channels are ignored for now.
		return nil
	default:
		Debug("RTSP", "Ignoring unsupported interleaved channel %d", channel)
		return nil
	}
}

// StartRTSPStream starts the RTSP stream with automatic reconnection on failure.
func (wp *WebRTCProxy) StartRTSPStream() {
	const maxAttempts = 5

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			Warn("RTSP", "Reconnect attempt %d/%d in %v", attempt, maxAttempts, backoff)
			time.Sleep(backoff)
		}

		// Stop if the WebRTC session was closed while we were waiting.
		if wp.peerConnection == nil {
			return
		}

		Info("RTSP", "Connecting to RTSP %s...", wp.rtspUrl)
		wp.rtspClient = NewRtspClient()

		if err := wp.rtspClient.Client(wp.rtspUrl, false); err != nil {
			Error("RTSP", "Connection error: %v", err)
			continue
		}

		Info("RTSP", "Connected")
		wp.consumeRTPPackets()

		// consumeRTPPackets returned: either the RTSP session ended or WebRTC closed.
		if wp.peerConnection == nil {
			return
		}

		Warn("RTSP", "RTSP session ended, will reconnect")
	}

	Error("RTSP", "Max reconnect attempts reached, closing session")
	wp.Close()
}

// consumeRTPPackets forwards packets from the RTSP client to the WebRTC tracks
// until the RTSP session ends or a signal is received.
func (wp *WebRTCProxy) consumeRTPPackets() {
	packetCount := 0
	lastLogTime := time.Now()
	droppedCount := 0

	for {
		select {
		case data := <-wp.rtspClient.received:
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

		case <-wp.rtspClient.signals:
			Info("RTSP", "Exit signal received from RTSP client")
			return
		}
	}
}

func (wp *WebRTCProxy) logSelectedICEPairNetwork() {
	pair, err := wp.getSelectedICECandidatePair()
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

func (wp *WebRTCProxy) getSelectedICECandidatePair() (*webrtc.ICECandidatePair, error) {
	for _, sender := range wp.peerConnection.GetSenders() {
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

	if wp.peerConnection.SCTP() != nil &&
		wp.peerConnection.SCTP().Transport() != nil &&
		wp.peerConnection.SCTP().Transport().ICETransport() != nil {
		return wp.peerConnection.SCTP().Transport().ICETransport().GetSelectedCandidatePair()
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
