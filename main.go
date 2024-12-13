package main

import (
    "log"
    "os"
    "os/signal"
    "syscall"
    "github.com/pion/webrtc/v4"
    "flag"
)

func main() {
    

    configFile := flag.String("config","config.yaml", "config file path")
    flag.Parse()
    config, err := loadConfig(*configFile)
    if err != nil {
        log.Fatalf("Error while loading config: %v", err)
    }

    // build proxy client
    webrtcProxy := NewWebRTCProxy(config.Device.URL)
    
    // build signaling 
    mqttClient := NewMqttClient(config.Device.ID, config.MQTT.Username, config.MQTT.Password, config.MQTT.URL)
    mqttClient.Name = config.MQTT.Name
    mqttClient.OnOfferReceived(func(source string, offer webrtc.SessionDescription) {
        answer, err :=webrtcProxy.HandleOffer(source, offer)
        if err != nil {
            log.Printf("[MQTT] - Error while handling SDP: %v", err) 
            mqttClient.SendError(source, err)
            mqttClient.Hangup(source)
        } else {
            mqttClient.SendAnswerSDP(source, answer)
        }


    })

    mqttClient.OnRemoteICECandidateReceived(func(source string, ice webrtc.ICECandidateInit) {
        webrtcProxy.AddICECandidate(ice)
    })
    
    mqttClient.OnHangupReceived(func() {
        webrtcProxy.Close()
    })

    webrtcProxy.OnLocalICECandidateReceived(func(source string, ice *webrtc.ICECandidate) {
        mqttClient.SendICECandidate(source, ice)
    })

    webrtcProxy.OnConnectionStateChanged(func(streaming bool) {
        mqttClient.streaming = streaming
        mqttClient.PublishDeviceInfo()
    })
    
    
    c := make(chan os.Signal, 1)
    signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
    <-c

    
    log.Println("Closing app ...")
    webrtcProxy.Close()
    mqttClient.Close()
}