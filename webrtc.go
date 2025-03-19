package main

import (
    "log"
    "github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
    "time"
    "errors"
)


type WebRTCProxy struct {
    peerConnection *webrtc.PeerConnection
    videoTrack *webrtc.TrackLocalStaticSample
    audioTrack *webrtc.TrackLocalStaticSample

    rtspUrl string
    rtspClient *RtspClient
    annexBParser *AnnexBParser

    videoCodec string
    audioCodec string

    onLocalICECandidateReceived OnLocalICECandidateReceivedFunc
    onConnectionStateChanged OnConnectionStateChangedFunc


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
    log.Printf("[WEBRTC] - url : %s", device.URL)
    
    // create a new object
    Obj := &WebRTCProxy{
        // create a new annexBParser
        annexBParser: NewAnnexBParser(),
        // set the RTSP URL
        rtspUrl: device.URL,
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
        log.Println("[WEBRTC] - Closing WebRTC connection...")
        wp.peerConnection.Close()
        wp.peerConnection = nil
    }

    // Close the RTSP connection if it exists
    if wp.rtspClient != nil {
        log.Println("[RTSP] - Disconnecting from RTSP...")
        wp.rtspClient.Close()
        //wp.rtspClient = nil
    }
    wp.onConnectionStateChanged(false)
}


// CreatePeerConnection creates a new PeerConnection and adds the video and audio tracks to it
//
// The method is called when the HandleOffer method is called and a new PeerConnection needs to be created.
func (wp *WebRTCProxy) CreatePeerConnection(from string) error {
    // Create a new PeerConnection with the given configuration
    // The configuration contains ICE servers that are used to establish a connection between the peers
    config := webrtc.Configuration{ICEServers: []webrtc.ICEServer{
        {
            URLs: []string{"stun:stun1.l.google.com:19302", "stun:stun2.l.google.com:19302"},
        },
    }}
    // config := webrtc.Configuration{}
   
    var err error
    wp.peerConnection, err = webrtc.NewPeerConnection(config)
    if err != nil {
        log.Printf("[WEBRTC] - Error while creating PeerConnection: %v", err)
        return err
    }

    // Set a callback for when an ICE candidate is received
    wp.peerConnection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
        if candidate != nil {      
            wp.onLocalICECandidateReceived(from, candidate)                  
        }
    })

    // Set a callback for when the connection state of the peer connection changes
    wp.peerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
        log.Printf("[WEBRTC] - Connection state: %s", state.String())
        if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateDisconnected{
            log.Println("[WEBRTC] - Closing rtsp connection...")
            wp.rtspClient.Close()
            wp.onConnectionStateChanged(false)
        } else if state == webrtc.PeerConnectionStateConnected {
            go wp.StartRTSPStream()
            wp.onConnectionStateChanged(true)
            
        }
    })

    // Set a callback for when the signaling state of the peer connection changes
    wp.peerConnection.OnSignalingStateChange(func(state webrtc.SignalingState) {
        log.Printf("[WEBRTC] - Signaling state: %s", state.String())
    })

    // Create a new video track and add it to the peer connection
    wp.videoTrack, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: wp.videoCodec}, "video", "rtsp-video")
    if err != nil {
        return  err
    }
    
    _, err = wp.peerConnection.AddTrack(wp.videoTrack)
    if err != nil {
        return err
    }
    
    // Create a new audio track and add it to the peer connection
    wp.audioTrack, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: wp.audioCodec}, "audio", "rtsp-audio")
    if err == nil {
        _, err = wp.peerConnection.AddTrack(wp.audioTrack)
        if err != nil {
            return  err
        }
    } else {
        log.Println("[WEBRTC] - Audio track not added.")
    }

    return nil
}


// AddICECandidate adds an ICE candidate to the existing PeerConnection.
// Returns an error if the PeerConnection is nil or if adding the candidate fails.
func (wp *WebRTCProxy) AddICECandidate(candidate webrtc.ICECandidateInit) error {
    // Check if the PeerConnection exists
    if wp.peerConnection == nil {
        log.Println("[WEBRTC] - PeerConnection is null.")
        return errors.New("PeerConnection is null")
    }
    
    // Try to add the ICE candidate to the PeerConnection
    if err := wp.peerConnection.AddICECandidate(candidate); err != nil {
        log.Printf("[WEBRTC] - Error while adding ICE candidate: %v", err)
        return err
    }
    
    // Successfully added the ICE candidate
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
        log.Println("[WEBRTC] - PeerConnection already exists.")
        return nil, errors.New("PeerConnection already exists.")
    }

    if err := wp.CreatePeerConnection(from); err != nil {
        return nil, err
    }
    log.Println("[WEBRTC] - PeerConnection OK")

    if err := wp.peerConnection.SetRemoteDescription(offer); err != nil {
        return nil, err
    }
    log.Println("[WEBRTC] - SetRemoteDescription OK")

    answer, err := wp.peerConnection.CreateAnswer(nil)
    if err != nil {
        log.Printf("[WEBRTC] - Error while creating answer: %v", err)
        return nil, err
    }
    log.Println("[WEBRTC] - CreateAnswer OK")

    if err := wp.peerConnection.SetLocalDescription(answer); err != nil {
        log.Printf("[WEBRTC] - Error while setting local description: %v", err)
        wp.Close()
        return nil, err
    }
    log.Println("[WEBRTC] - SetLocalDescription OK")

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
	log.Printf("[RTSP] - Connecting to RTSP %s...", wp.rtspUrl)
    
    wp.rtspClient = NewRtspClient()
	wp.annexBParser.reset()

	if err := wp.rtspClient.Client(wp.rtspUrl, false); err == nil {
		log.Println("[RTSP] - Connected")
		for {
			select {
			case data := <- wp.rtspClient.received:
				if len(data) > 12 {
					//log.Printf("[RTSP] - packet [0]=%x type=%d - %d\n", data[0], data[1], len(data)) //
						payload, duration := wp.annexBParser.handlePacket(&data)
						if payload != nil {
							wp.sendPacket(payload, duration, data[1] == 0 )
						} else {
                            log.Println("[RTSP] - no payload")
                        }					

				} else {
					log.Println("[RTSP] - Data too short to contain an RTP header")
				}
			case <-wp.rtspClient.signals:
				log.Println("[RTSP] - exit signal by class rtsp")
			}
		}
		log.Println("[RTSP] - exit ")
	} else {
		log.Println("[RTSP] - ", err)
        wp.Close()
	}
}


