package main

import (
    "fmt"
    "log"
    "crypto/tls"
    pahomqtt "github.com/eclipse/paho.mqtt.golang"
	"encoding/json"
    "github.com/pion/webrtc/v4"
)

type MqttClient struct {    
    ClientID string 
    Username string 
    Password string 
    URL      string
    Name string
    mqttClient pahomqtt.Client
    onOfferReceived OnOfferReceivedFunc
    onRemoteICECandidateReceived OnRemoteICECandidateReceivedFunc
    onHangupReceived OnHangupReceivedFunc
}


type SignalingMessage struct {
    Source string          `json:"source"`
    Type   string `json:"type"`
    Data   json.RawMessage `json:"data,omitempty"`
}

const (    
    SDP    = "sdp"
    ICE    = "ice"
    Device  = "device"
    Hangup  = "hangup"
    Query   = "query"
    Error   = "error"
)

type OnOfferReceivedFunc func(source string, sdp webrtc.SessionDescription)
type OnRemoteICECandidateReceivedFunc func(source string, candidate webrtc.ICECandidateInit)
type OnHangupReceivedFunc func()


func NewMqttClient(clientId string, username string, password string, url string) *MqttClient {

    Obj := &MqttClient{
        ClientID: clientId,
        Username: username,
        Password: password,
        URL:      url,
    }
    log.Printf("[MQTT] - Connecting to %s", url)

    // needed for embedded linux 
    tlsconfig := &tls.Config{
		
		// ClientAuth = whether to request cert from server.
		// Since the server is set up for SSL, this happens
		// anyways.
		ClientAuth: tls.NoClientCert,
		// ClientCAs = certs used to validate client cert.
		ClientCAs: nil,
		// InsecureSkipVerify = verify that cert contents
		// match server. IP matches what is in cert etc.
		InsecureSkipVerify: true,		
	}


    mqttOptions := pahomqtt.NewClientOptions().
        AddBroker(Obj.URL).
        SetClientID(Obj.ClientID).
        SetAutoReconnect(true).
        SetConnectionLostHandler(func(client pahomqtt.Client, err error) {
            log.Printf("[MQTT] - Connection lost: %v", err)
            
        }).
        SetOnConnectHandler(func(client pahomqtt.Client) {
            log.Println("[MQTT] - Connection established")
            Obj.SubscribeToTopics(Obj.ClientID)
            Obj.SubscribeToTopics("devices")
            Obj.PublishDeviceInfo(true, false)
        }).
        SetMessageChannelDepth(255).
        SetTLSConfig(tlsconfig).
        SetCleanSession(false)

    if Obj.Username != "" && Obj.Password != "" {
        mqttOptions.SetUsername(Obj.Username)
        mqttOptions.SetPassword(Obj.Password)
    }

    Obj.mqttClient = pahomqtt.NewClient(mqttOptions)
    if token := Obj.mqttClient.Connect(); token.Wait() && token.Error() != nil {
        log.Fatalf("[MQTT] - Connection error: %v", token.Error())
    } else {
        log.Println("[MQTT] - Connected")
    }

    return Obj
    
}

func (this *MqttClient) OnOfferReceived(callback OnOfferReceivedFunc) {
    this.onOfferReceived = callback
}

func (this *MqttClient) OnRemoteICECandidateReceived (callback OnRemoteICECandidateReceivedFunc) {
    this.onRemoteICECandidateReceived = callback
}

func (this *MqttClient) OnHangupReceived (callback OnHangupReceivedFunc) {
    this.onHangupReceived = callback
}

func (this *MqttClient) Close (){
    this.PublishDeviceInfo(false, false)
    this.mqttClient.Disconnect(250)
    log.Println("[MQTT] - Closed")

}


func (this *MqttClient) PublishDeviceInfo(online bool, streaming bool) { 
    payload := SignalingMessage{        
        Type:   Device,
        Data:    []byte(fmt.Sprintf(`{"id": "%s","name": "%s","online": %t, "streaming": %t, "type": "sender"}`, this.ClientID, this.Name, online, streaming)),
    }  
    log.Printf("[MQTT] - Sending device info %s - %s ", this.Username, this.ClientID)
    this.Send("devices", payload, !online)
}

func (this *MqttClient) SubscribeToTopics(topic string) {    
    
    this.mqttClient.Subscribe("/" + topic , 1, func(mqttClient pahomqtt.Client, msg pahomqtt.Message) {        
        this.HandleMQTTMessage( msg)
    })
    log.Printf("[MQTT] - Subscribed to %s",  topic)
}

func (this *MqttClient) HandleMQTTMessage( msg pahomqtt.Message) {
    //log.Printf("[MQTT] - Message received on %s: %s", msg.Topic(), msg.Payload())

    // decode message
    var signalingMsg SignalingMessage
    err := json.Unmarshal(msg.Payload(), &signalingMsg)
    if err != nil {
        log.Printf("[MQTT] - Error while parsing JSON: %v", err)
        return
    }

    // ignore echo message 
    if signalingMsg.Source == this.ClientID {
        return
    }

    switch signalingMsg.Type {
        case Hangup:
            log.Printf("[MQTT] - Hangup request from %s", signalingMsg.Source)
            this.onHangupReceived()
        case SDP:
            var sdp webrtc.SessionDescription
            err := json.Unmarshal(signalingMsg.Data, &sdp)
            if err != nil {
                log.Printf("[MQTT] - Error while parsing SDP: %v", err)
                return
            }
            if sdp.Type == webrtc.SDPTypeOffer {
                log.Printf("[MQTT] - SDP offer received from %s", signalingMsg.Source)
                this.onOfferReceived(signalingMsg.Source, sdp)
            } else {
                log.Printf("[MQTT] - Unhandled SDP type: %s", sdp.Type)
            }

        case ICE:
            var ice webrtc.ICECandidateInit
            err := json.Unmarshal(signalingMsg.Data, &ice)
            if err != nil {
                log.Printf("[MQTT] - Error while parsing ICE: %v", err)
                return
            }
            log.Printf("[MQTT] - ICE candidate received from %s", signalingMsg.Source)
            this.onRemoteICECandidateReceived(signalingMsg.Source, ice)
        case Query:
            log.Printf("[MQTT] - Query request from %s", signalingMsg.Source)
            this.PublishDeviceInfo(true, false)
        default:
            log.Printf("[MQTT] - Unhandled message: %s : %s", signalingMsg.Type, msg.Payload())
        }
}


func (this *MqttClient) SendAnswerSDP(to string, answer *webrtc.SessionDescription) {
    
    data, err := json.Marshal(answer)
    if err != nil {
        log.Printf("[MQTT] - Error while encoding SDP: %v", err)
        return
    }

    //RTCSessionDescription
    payload := SignalingMessage{        
        Type:   SDP,
        Data:   data,
    }        
    this.Send(to, payload, false)
}

func (this *MqttClient) SendICECandidate(to string, ice *webrtc.ICECandidate) {
        
    data, err := json.Marshal(ice.ToJSON())
    if err != nil {
        log.Printf("[MQTT] - Error while encoding ICE: %v", err)
        return
    }    
    payload := SignalingMessage{        
        Type:   ICE,
        Data:   data,
    }  
    this.Send(to, payload, false)
}

func (this *MqttClient) Hangup(to string) {
    this.Send(to, SignalingMessage{Source: to, Type: Hangup}, false)    
}

func (this *MqttClient) SendError(to string, err error) {
    // send error 
    payload := SignalingMessage{        
        Type:   Error,
        Data:   json.RawMessage(`"` + err.Error() + `"`),
    }     
    
    this.Send(to, payload, false)
}


func (this *MqttClient) Send(channel string, payload SignalingMessage, retained bool) {
    // override source 
    payload.Source = this.ClientID

    jsonData, err := json.Marshal(payload)
    if err != nil {
        fmt.Println("[MQTT] - Error while converting payload :", err)
        return
    }
    topic := "/" + channel  
    token := this.mqttClient.Publish(topic, 0, retained, jsonData)
    go token.Wait()
    log.Printf("[MQTT] - Data sent to %s", topic)
    log.Printf(string(jsonData))
}
