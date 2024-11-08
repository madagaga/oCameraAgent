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


func NewWebRTCProxy( rtspUrl string) *WebRTCProxy {
    log.Printf("[WEBRTC] - url : %s", rtspUrl)    
    Obj := &WebRTCProxy{
        demuxer: NewDemuxer(),        
        rtspUrl : rtspUrl,
    }
    return Obj
}



func (this *WebRTCProxy) OnLocalICECandidateReceived(callback OnLocalICECandidateReceivedFunc) {
    this.onLocalICECandidateReceived = callback
}

func (this *WebRTCProxy) OnConnectionStateChanged(callback OnConnectionStateChangedFunc) {
    this.onConnectionStateChanged = callback
}


func (this *WebRTCProxy) Close() {
    if this.peerConnection != nil {
        log.Println("[WEBRTC] - Closing WebRTC connection...")
        this.peerConnection.Close()
        this.peerConnection = nil        
    }

    if this.rtspClient != nil {
        log.Println("[RTSP] - Disconnecting from RTSP...")
        this.rtspClient.Close()
        //this.rtspClient = nil
    }
}


func (this *WebRTCProxy) CreatePeerConnection(from string) error {
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

    this.peerConnection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
        if candidate != nil {      
            this.onLocalICECandidateReceived(from, candidate)                  
        }
    })

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

    this.peerConnection.OnSignalingStateChange(func(state webrtc.SignalingState) {
        log.Printf("[WEBRTC] - Signaling state: %s", state.String())
    })

    
    this.videoTrack, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "video", "rtsp-video")
    if err != nil {
        return  err
    }
    
    _, err = this.peerConnection.AddTrack(this.videoTrack)
    if err != nil {
        return err
    }
    
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

func (this *WebRTCProxy) AddICECandidate(candidate webrtc.ICECandidateInit) error {
    
    if(this.peerConnection != nil) {        
    
        err := this.peerConnection.AddICECandidate(candidate)
        if err != nil {
            log.Printf("[WEBRTC] - Error while adding ICE candidate: %v", err)
            return err
        }
    }else {
        log.Println("[WEBRTC] - Peerconnection is null .")
    }
    return nil
}


func (this *WebRTCProxy) HandleOffer(from string, offer webrtc.SessionDescription) (*webrtc.SessionDescription, error) {
    if this.peerConnection != nil {
        log.Println("[WEBRTC] - PeerConnection already exists.")
        return nil, errors.New("PeerConnection already exists.")
    }
    
    err := this.CreatePeerConnection(from)
    if err != nil {
        return nil, err
    }
    log.Println("[WEBRTC] - PeerConnection OK")


    err = this.peerConnection.SetRemoteDescription(offer)
    if err != nil {
        return nil, err
    }
    log.Println("[WEBRTC] - SetRemoteDescription OK")


    answer, err := this.peerConnection.CreateAnswer(nil)
    if err != nil {        
        log.Printf("[WEBRTC] - Error while creating answer: %v", err)
        return nil, err
    }
    log.Println("[WEBRTC] - CreateAnswer OK")

    err = this.peerConnection.SetLocalDescription(answer)
    if err != nil {
        log.Printf("[WEBRTC] - Error while setting local description: %v", err)
        this.Close()
        return nil, err
    }
    log.Println("[WEBRTC] - SetLocalDescription OK")

    return this.peerConnection.CurrentLocalDescription(), nil
}


func (this *WebRTCProxy) sendPacket(payload []byte, timestamp uint32, isVideo bool) error {
    if isVideo {
        if this.videoTrack != nil {
            return this.videoTrack.WriteSample(media.Sample{Data: payload, Duration: time.Millisecond * time.Duration(timestamp)})
        }
    } else {
        if this.audioTrack != nil {
            return this.audioTrack.WriteSample(media.Sample{Data: payload, Duration: time.Millisecond * time.Duration(timestamp)})
        }
    }
    return nil
}

func (this *WebRTCProxy) StartRTSPStream() {
	log.Printf("[RTSP] - Connecting to RTSP %s...", this.rtspUrl)
	this.rtspClient = NewRtspClient()
	this.demuxer.reset()

	if status, message := this.rtspClient.Client(this.rtspUrl, false); status {
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
		log.Println("[RTSP] - error", message)
	}
}


