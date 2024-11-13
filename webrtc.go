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
    demuxer *Demuxer

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
func NewWebRTCProxy(rtspUrl string) *WebRTCProxy {
    // log the URL to the console
    log.Printf("[WEBRTC] - url : %s", rtspUrl)
    
    // create a new object
    Obj := &WebRTCProxy{
        // create a new demuxer
        demuxer: NewDemuxer(),
        // set the RTSP URL
        rtspUrl: rtspUrl,
    }
    // return the object
    return Obj
}



// OnLocalICECandidateReceived sets a callback for when a local ICE candidate is received
//
// The callback is called with the source of the candidate and the candidate itself.
// The source is the identifier of the peer that sent the candidate.
func (this *WebRTCProxy) OnLocalICECandidateReceived(callback OnLocalICECandidateReceivedFunc) {
    this.onLocalICECandidateReceived = callback
}


// OnConnectionStateChanged sets a callback for when the connection state of the peer connection changes
//
// The callback is called with a boolean argument indicating whether the connection is established or not.
func (this *WebRTCProxy) OnConnectionStateChanged(callback OnConnectionStateChangedFunc) {
    this.onConnectionStateChanged = callback
}


// Close the WebRTC connection and the RTSP connection.
//
// This method can be called multiple times without any problems.
func (this *WebRTCProxy) Close() {
    // Close the WebRTC connection if it exists
    if this.peerConnection != nil {
        log.Println("[WEBRTC] - Closing WebRTC connection...")
        this.peerConnection.Close()
        this.peerConnection = nil
    }

    // Close the RTSP connection if it exists
    if this.rtspClient != nil {
        log.Println("[RTSP] - Disconnecting from RTSP...")
        this.rtspClient.Close()
        //this.rtspClient = nil
    }
}


// CreatePeerConnection creates a new PeerConnection and adds the video and audio tracks to it
//
// The method is called when the HandleOffer method is called and a new PeerConnection needs to be created.
func (this *WebRTCProxy) CreatePeerConnection(from string) error {
    // Create a new PeerConnection with the given configuration
    // The configuration contains ICE servers that are used to establish a connection between the peers
    config := webrtc.Configuration{ICEServers: []webrtc.ICEServer{
        {
            URLs: []string{"stun:stun1.l.google.com:19302", "stun:stun2.l.google.com:19302"},
        },
    }}
    // config := webrtc.Configuration{}
   
    var err error
    this.peerConnection, err = webrtc.NewPeerConnection(config)
    if err != nil {
        log.Printf("[WEBRTC] - Error while creating PeerConnection: %v", err)
        return err
    }

    // Set a callback for when an ICE candidate is received
    this.peerConnection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
        if candidate != nil {      
            this.onLocalICECandidateReceived(from, candidate)                  
        }
    })

    // Set a callback for when the connection state of the peer connection changes
    this.peerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
        log.Printf("[WEBRTC] - Connection state: %s", state.String())
        if state == webrtc.PeerConnectionStateFailed {
            log.Println("[WEBRTC] - Connection failed. Closing rtsp connection...")
            this.rtspClient.Close()
            this.onConnectionStateChanged(false)
            

        } else if state == webrtc.PeerConnectionStateConnected {
            go this.StartRTSPStream()
            this.onConnectionStateChanged(true)
            
        }
    })

    // Set a callback for when the signaling state of the peer connection changes
    this.peerConnection.OnSignalingStateChange(func(state webrtc.SignalingState) {
        log.Printf("[WEBRTC] - Signaling state: %s", state.String())
    })

    // Create a new video track and add it to the peer connection
    this.videoTrack, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "video", "rtsp-video")
    if err != nil {
        return  err
    }
    
    _, err = this.peerConnection.AddTrack(this.videoTrack)
    if err != nil {
        return err
    }
    
    // Create a new audio track and add it to the peer connection
    this.audioTrack, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMA}, "audio", "rtsp-audio")
    if err == nil {
        _, err = this.peerConnection.AddTrack(this.audioTrack)
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
func (this *WebRTCProxy) AddICECandidate(candidate webrtc.ICECandidateInit) error {
    // Check if the PeerConnection exists
    if this.peerConnection == nil {
        log.Println("[WEBRTC] - PeerConnection is null.")
        return errors.New("PeerConnection is null")
    }
    
    // Try to add the ICE candidate to the PeerConnection
    if err := this.peerConnection.AddICECandidate(candidate); err != nil {
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
func (this *WebRTCProxy) HandleOffer(from string, offer webrtc.SessionDescription) (*webrtc.SessionDescription, error) {
    if this.peerConnection != nil {
        log.Println("[WEBRTC] - PeerConnection already exists.")
        return nil, errors.New("PeerConnection already exists.")
    }

    if err := this.CreatePeerConnection(from); err != nil {
        return nil, err
    }
    log.Println("[WEBRTC] - PeerConnection OK")

    if err := this.peerConnection.SetRemoteDescription(offer); err != nil {
        return nil, err
    }
    log.Println("[WEBRTC] - SetRemoteDescription OK")

    answer, err := this.peerConnection.CreateAnswer(nil)
    if err != nil {
        log.Printf("[WEBRTC] - Error while creating answer: %v", err)
        return nil, err
    }
    log.Println("[WEBRTC] - CreateAnswer OK")

    if err := this.peerConnection.SetLocalDescription(answer); err != nil {
        log.Printf("[WEBRTC] - Error while setting local description: %v", err)
        this.Close()
        return nil, err
    }
    log.Println("[WEBRTC] - SetLocalDescription OK")

    return this.peerConnection.CurrentLocalDescription(), nil
}


// sendPacket sends a packet to the WebRTC peer.
//
// The payload is the content of the packet.
// The timestamp is the timestamp of the packet.
// The isVideo parameter indicates if the packet is a video or audio packet.
//
// The method returns an error if the packet cannot be sent.
func (this *WebRTCProxy) sendPacket(payload []byte, timestamp uint32, isVideo bool) error {
    // If the packet is a video packet and the video track is not nil, write the packet to the track.
    if isVideo && this.videoTrack != nil {
        return this.videoTrack.WriteSample(media.Sample{
            Data:     payload,
            Duration: time.Millisecond * time.Duration(timestamp),
        })
    }
    // If the packet is an audio packet and the audio track is not nil, write the packet to the track.
    if !isVideo && this.audioTrack != nil {
        return this.audioTrack.WriteSample(media.Sample{
            Data:     payload,
            Duration: time.Millisecond * time.Duration(timestamp),
        })
    }
    // If the packet is not a video or audio packet, return an error.
    return nil
}

// StartRTSPStream starts the RTSP stream.
//
// This method is used to start the RTSP stream and handle the incoming packets.
func (this *WebRTCProxy) StartRTSPStream() {
	log.Printf("[RTSP] - Connecting to RTSP %s...", this.rtspUrl)
    
    this.rtspClient = NewRtspClient()
	this.demuxer.reset()

	if err := this.rtspClient.Client(this.rtspUrl, false); err == nil {
		log.Println("[RTSP] - Connected")
		for {
			select {
			case data := <- this.rtspClient.received:
				if len(data) > 12 {
					//log.Printf("[RTSP] - packet [0]=%x type=%d - %d\n", data[0], data[1], len(data)) //
						payload, duration := this.demuxer.handlePacket(&data)
						if payload != nil {
							this.sendPacket(payload, duration, data[1] == 0 )
						} else {
                            log.Println("[RTSP] - no payload")
                        }					

				} else {
					log.Println("[RTSP] - Data too short to contain an RTP header")
				}
			case <-this.rtspClient.signals:
				log.Println("[RTSP] - exit signal by class rtsp")
			}
		}
		log.Println("[RTSP] - exit ")
	} else {
		log.Println("[RTSP] - error", err)
	}
}


