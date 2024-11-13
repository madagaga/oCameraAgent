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


// NewMqttClient creates a new MQTT client object.
//
// The created object is configured to connect to the specified MQTT broker
// with the specified client ID and credentials.
//
// The object is not started automatically, the Connect() method must be
// called to establish the connection.
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

// OnOfferReceived sets a callback for when a SDP offer is received from another client.
//
// The callback is called with the source of the offer and the offer itself.
// The source is the identifier of the peer that sent the offer.
func (this *MqttClient) OnOfferReceived(callback OnOfferReceivedFunc) {
    this.onOfferReceived = callback
}

// OnRemoteICECandidateReceived sets a callback for when a remote ICE candidate is received from another client.
//
// The callback is called with the source of the candidate and the candidate itself.
// The source is the identifier of the peer that sent the candidate.
func (this *MqttClient) OnRemoteICECandidateReceived (callback OnRemoteICECandidateReceivedFunc) {
    this.onRemoteICECandidateReceived = callback
}

// OnHangupReceived sets a callback for when a hangup request is received from another client.
//
// The callback is called with no arguments.
func (this *MqttClient) OnHangupReceived (callback OnHangupReceivedFunc) {
    this.onHangupReceived = callback
}

// Close disconnects the MQTT client and publishes a device info message indicating that the device is offline.
//
// The method can be called multiple times without any problems.
func (this *MqttClient) Close() {
    // publish device info message indicating that the device is offline
    this.PublishDeviceInfo(false, false)

    // disconnect from the MQTT broker
    this.mqttClient.Disconnect(250)
    log.Println("[MQTT] - Closed")
}


// PublishDeviceInfo sends a device info message to the MQTT broker.
//
// The message contains the device id, name, online status, and streaming status.
// The message is sent to the "devices" topic and the retained flag is set to true if the device is offline.
func (this *MqttClient) PublishDeviceInfo(online bool, streaming bool) {
    // create device info message
    payload := SignalingMessage{
        Type:   Device,
        Data:    []byte(fmt.Sprintf(`{"id": "%s","name": "%s","online": %t, "streaming": %t, "type": "sender"}`, this.ClientID, this.Name, online, streaming)),
    }

    // log message
    log.Printf("[MQTT] - Sending device info %s - %s ", this.Username, this.ClientID)

    // send message to the MQTT broker
    this.Send("devices", payload, !online)
}

// SubscribeToTopics subscribes the MQTT client to a given topic and sets up a message handler.
//
// The message handler processes incoming messages by calling HandleMQTTMessage.
func (this *MqttClient) SubscribeToTopics(topic string) {
    // Subscribe to the specified topic with QoS level 1
    this.mqttClient.Subscribe("/" + topic, 1, func(mqttClient pahomqtt.Client, msg pahomqtt.Message) {
        // Handle the received MQTT message
        this.HandleMQTTMessage(msg)
    })
    
    // Log the subscription action
    log.Printf("[MQTT] - Subscribed to %s", topic)
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


// SendAnswerSDP sends an answer SDP to the given peer.
// The answer is converted to JSON and sent as a message of type SDP.
func (this *MqttClient) SendAnswerSDP(to string, answer *webrtc.SessionDescription) {
    // Marshal the answer to JSON
    data, err := json.Marshal(answer)
    if err != nil {
        log.Printf("[MQTT] - Error while encoding SDP: %v", err)
        return
    }

    // Create a SignalingMessage with the answer and send it to the peer
    payload := SignalingMessage{
        Type:   SDP,
        Data:   data,
    }
    this.Send(to, payload, false)
}

// SendICECandidate sends an ICE candidate to the given peer.
// The candidate is converted to JSON and sent as a message of type ICE.
func (this *MqttClient) SendICECandidate(to string, ice *webrtc.ICECandidate) {
	// Marshal the candidate to JSON
	data, err := json.Marshal(ice.ToJSON())
	if err != nil {
		log.Printf("[MQTT] - Error while encoding ICE: %v", err)
		return
	}

	// Create a SignalingMessage with the candidate and send it to the peer
	payload := SignalingMessage{
		Type:   ICE,
		Data:   data,
	}
	this.Send(to, payload, false)
}

// Hangup sends a hangup request to the given peer.
// The hangup request is sent as a message of type Hangup.
func (this *MqttClient) Hangup(to string) {
    // Create a SignalingMessage of type Hangup and send it to the peer
    this.Send(to, SignalingMessage{Source: to, Type: Hangup}, false)
}

// SendError sends an error message to the given peer.
// The error is converted to JSON and sent as a message of type Error.
func (this *MqttClient) SendError(to string, err error) {
    // Create a SignalingMessage with the error and send it to the peer
    payload := SignalingMessage{
        Type:   Error,
        Data:   json.RawMessage(`"` + err.Error() + `"`),
    }
    
    // Send the error message to the peer
    this.Send(to, payload, false)
}


// Send sends a message to the specified channel on the MQTT broker.
//
// The payload is marshaled to JSON and published to the specified topic.
// The retained flag specifies whether the message should be retained by the broker.
func (this *MqttClient) Send(channel string, payload SignalingMessage, retained bool) {
    // override source 
    payload.Source = this.ClientID

    // marshal the payload to JSON
    jsonData, err := json.Marshal(payload)
    if err != nil {
        fmt.Println("[MQTT] - Error while converting payload :", err)
        return
    }

    // construct the topic string
    topic := "/" + channel  

    // publish the message to the MQTT broker
    token := this.mqttClient.Publish(topic, 0, retained, jsonData)
    go token.Wait()

    // log the publish action
    log.Printf("[MQTT] - Data sent to %s", topic)
    log.Printf("%s", string(jsonData))
}
