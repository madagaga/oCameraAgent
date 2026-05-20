package main

import (
	"errors"
	"net"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

type WebRTCProxy struct {
	peerConnection *webrtc.PeerConnection
	videoTrack     *webrtc.TrackLocalStaticSample
	audioTrack     *webrtc.TrackLocalStaticSample

	rtspUrl      string
	rtspClient   *RtspClient
	annexBParser *AnnexBParser

	videoCodec string
	audioCodec string

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
		// create a new annexBParser
		annexBParser: NewAnnexBParser(),
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

// Close the WebRTC connection and the RTSP connection.
//
// wp method can be called multiple times without any problems.
func (wp *WebRTCProxy) Close() {
	// Close the WebRTC connection if it exists
	if wp.peerConnection != nil {
		Info("WEBRTC", "Closing WebRTC connection...")
		wp.peerConnection.Close()
		wp.peerConnection = nil
	}

	// Close the RTSP connection if it exists
	if wp.rtspClient != nil {
		Info("RTSP", "Disconnecting from RTSP...")
		wp.rtspClient.Close()
		//wp.rtspClient = nil
	}
	if wp.onConnectionStateChanged != nil {
		wp.onConnectionStateChanged(false)
	}
}

// CreatePeerConnection creates a new PeerConnection and adds the video and audio tracks to it
//
// The method is called when the HandleOffer method is called and a new PeerConnection needs to be created.
func (wp *WebRTCProxy) CreatePeerConnection(from string) error {
	_ = from
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
			Info("WEBRTC", "Closing RTSP connection...")
			if wp.rtspClient != nil {
				wp.rtspClient.Close()
			}
			if wp.onConnectionStateChanged != nil {
				wp.onConnectionStateChanged(false)
			}
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
	wp.videoTrack, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: wp.videoCodec}, "video", "rtsp-video")
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
	wp.audioTrack, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: wp.audioCodec}, "audio", "rtsp-audio")
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

// AddICECandidate adds an ICE candidate to the existing PeerConnection.
// Returns an error if the PeerConnection is nil or if adding the candidate fails.
func (wp *WebRTCProxy) AddICECandidate(candidate webrtc.ICECandidateInit) error {
	// Check if the PeerConnection exists
	if wp.peerConnection == nil {
		Error("WEBRTC", "PeerConnection is null")
		return errors.New("PeerConnection is null")
	}

	// Try to add the ICE candidate to the PeerConnection
	if err := wp.peerConnection.AddICECandidate(candidate); err != nil {
		Error("WEBRTC", "Error while adding ICE candidate: %v", err)
		return err
	}

	// Successfully added the ICE candidate
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

// sendPacket sends a packet to the WebRTC peer.
//
// The payload is the content of the packet.
// The timestamp is the timestamp of the packet.
// The isVideo parameter indicates if the packet is a video or audio packet.
//
// The method returns an error if the packet cannot be sent.
func (wp *WebRTCProxy) sendPacket(payload []byte, timestamp uint32, isVideo bool) error {
	// If the packet is a video packet and the video track is not nil, write the packet to the track.
	if isVideo && wp.videoTrack != nil {
		return wp.videoTrack.WriteSample(media.Sample{
			Data:     payload,
			Duration: time.Millisecond * time.Duration(timestamp),
		})
	}
	// If the packet is an audio packet and the audio track is not nil, write the packet to the track.
	if !isVideo && wp.audioTrack != nil {
		return wp.audioTrack.WriteSample(media.Sample{
			Data:     payload,
			Duration: time.Millisecond * time.Duration(timestamp),
		})
	}
	// If the packet is not a video or audio packet, return an error.
	return nil
}

// StartRTSPStream starts the RTSP stream.
//
// wp method is used to start the RTSP stream and handle the incoming packets.
func (wp *WebRTCProxy) StartRTSPStream() {
	Info("RTSP", "Connecting to RTSP %s...", wp.rtspUrl)

	wp.rtspClient = NewRtspClient()
	wp.annexBParser.reset()

	if err := wp.rtspClient.Client(wp.rtspUrl, false); err == nil {
		Info("RTSP", "Connected")

		// Counters to reduce log frequency
		packetCount := 0
		lastLogTime := time.Now()
		noPayloadCount := 0

		for {
			select {
			case data := <-wp.rtspClient.received:
				if len(data) > 12 {
					// Process the packet
					payload, duration := wp.annexBParser.handlePacket(&data)

					if payload != nil {
						// Send the packet to the WebRTC client
						if err := wp.sendPacket(payload, duration, data[1] == 0); err != nil {
							// Log only in case of error
							Error("RTSP", "Error sending packet: %v", err)
						}

						// Increment packet counter
						packetCount++

						// Periodic logging (every 5 seconds) to reduce CPU usage
						if time.Since(lastLogTime) > 5*time.Second {
							Debug("RTSP", "Processed %d packets in last 5 seconds", packetCount)
							packetCount = 0
							lastLogTime = time.Now()

							// Reset the counter for packets without payload
							if noPayloadCount > 0 {
								Debug("RTSP", "%d packets had no payload", noPayloadCount)
								noPayloadCount = 0
							}
						}
					} else {
						// Count packets without payload instead of logging them individually
						noPayloadCount++
					}
				} else if len(data) > 0 {
					// Keep short-packet logs in debug mode to avoid noisy normal runtime.
					Debug("RTSP", "Packet too short to contain an RTP header")
				}
				// Return packet buffer to pool when it originates from RtspClient.
				rtpPacketPool.Put(data[:0])
			case <-wp.rtspClient.signals:
				Info("RTSP", "Exit signal received from RTSP client")
				return
			}
		}
	} else {
		Error("RTSP", "Connection error: %v", err)
		wp.Close()
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
