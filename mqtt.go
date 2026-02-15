package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/pion/webrtc/v4"
)

type MqttClient struct {
	ClientID                     string
	Username                     string
	Password                     string
	URL                          string
	Name                         string
	mqttClient                   pahomqtt.Client
	onOfferReceived              OnOfferReceivedFunc
	onRemoteICECandidateReceived OnRemoteICECandidateReceivedFunc
	onHangupReceived             OnHangupReceivedFunc
	streaming                    bool
	online                       bool
}

type SignalingMessage struct {
	Source string          `json:"source"`
	Type   string          `json:"type"`
	Data   json.RawMessage `json:"data,omitempty"`
}

const (
	SDPMessage    = "sdp"
	ICEMessage    = "ice"
	DeviceMessage = "device"
	HangupMessage = "hangup"
	QueryMessage  = "query"
	ErrorMessage  = "error"
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
		ClientID:  clientId,
		Username:  username,
		Password:  password,
		URL:       url,
		streaming: false,
		online:    true,
	}

	Info("MQTT", "Connecting to %s", url)

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
			Error("MQTT", "Connection lost: %v", err)

		}).
		SetOnConnectHandler(func(client pahomqtt.Client) {
			Info("MQTT", "Connection established")
			Obj.SubscribeToTopics(Obj.ClientID)
			Obj.SubscribeToTopics("devices")
			Obj.PublishDeviceInfo()
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
		Fatal("MQTT", "Connection error: %v", token.Error())
	} else {
		Info("MQTT", "Connected")
	}

	return Obj

}

// OnOfferReceived sets a callback for when a SDP offer is received from another client.
//
// The callback is called with the source of the offer and the offer itself.
// The source is the identifier of the peer that sent the offer.
func (mqtt *MqttClient) OnOfferReceived(callback OnOfferReceivedFunc) {
	mqtt.onOfferReceived = callback
}

// OnRemoteICECandidateReceived sets a callback for when a remote ICE candidate is received from another client.
//
// The callback is called with the source of the candidate and the candidate itself.
// The source is the identifier of the peer that sent the candidate.
func (mqtt *MqttClient) OnRemoteICECandidateReceived(callback OnRemoteICECandidateReceivedFunc) {
	mqtt.onRemoteICECandidateReceived = callback
}

// OnHangupReceived sets a callback for when a hangup request is received from another client.
//
// The callback is called with no arguments.
func (mqtt *MqttClient) OnHangupReceived(callback OnHangupReceivedFunc) {
	mqtt.onHangupReceived = callback
}

// Close disconnects the MQTT client and publishes a device info message indicating that the device is offline.
//
// The method can be called multiple times without any problems.
func (mqtt *MqttClient) Close() {
	// publish device info message indicating that the device is offline
	mqtt.online = false
	mqtt.streaming = false
	mqtt.PublishDeviceInfo()

	// disconnect from the MQTT broker
	mqtt.mqttClient.Disconnect(250)
	Info("MQTT", "Closed")
}

// PublishDeviceInfo sends a device info message to the MQTT broker.
//
// The message contains the device id, name, online status, and streaming status.
// The message is sent to the "devices" topic and the retained flag is set to true if the device is offline.
func (mqtt *MqttClient) PublishDeviceInfo() {
	// create device info message
	payload := SignalingMessage{
		Type: DeviceMessage,
		Data: []byte(fmt.Sprintf(`{"id": "%s","name": "%s","online": %t, "streaming": %t, "type": "sender"}`, mqtt.ClientID, mqtt.Name, mqtt.online, mqtt.streaming)),
	}

	// log message
	Info("MQTT", "Sending device info %s - %s", mqtt.Username, mqtt.ClientID)

	// send message to the MQTT broker
	mqtt.Send("devices", payload, !mqtt.online)
}

// SubscribeToTopics subscribes the MQTT client to a given topic and sets up a message handler.
//
// The message handler processes incoming messages by calling HandleMQTTMessage.
func (mqtt *MqttClient) SubscribeToTopics(topic string) {
	// Subscribe to the specified topic with QoS level 1
	mqtt.mqttClient.Subscribe("/"+topic, 1, func(mqttClient pahomqtt.Client, msg pahomqtt.Message) {
		// Handle the received MQTT message
		mqtt.HandleMQTTMessage(msg)
	})

	// Log the subscription action
	Info("MQTT", "Subscribed to %s", topic)
}

func (mqtt *MqttClient) HandleMQTTMessage(msg pahomqtt.Message) {
	Debug("MQTT", "Message received on %s", msg.Topic())

	// decode message
	var signalingMsg SignalingMessage
	err := json.Unmarshal(msg.Payload(), &signalingMsg)
	if err != nil {
		Error("MQTT", "Error while parsing JSON: %v", err)
		return
	}

	// ignore echo message
	if signalingMsg.Source == mqtt.ClientID {
		return
	}

	switch signalingMsg.Type {
	case HangupMessage:
		Info("MQTT", "Hangup request from %s", signalingMsg.Source)
		if mqtt.onHangupReceived != nil {
			mqtt.onHangupReceived()
		} else {
			Warn("MQTT", "Hangup callback not set")
		}
	case SDPMessage:
		var sdp webrtc.SessionDescription
		err := json.Unmarshal(signalingMsg.Data, &sdp)
		if err != nil {
			Error("MQTT", "Error while parsing SDP: %v", err)
			return
		}
		if sdp.Type == webrtc.SDPTypeOffer {
			Info("MQTT", "SDP offer received from %s", signalingMsg.Source)
			if mqtt.onOfferReceived != nil {
				mqtt.onOfferReceived(signalingMsg.Source, sdp)
			} else {
				Warn("MQTT", "Offer callback not set")
			}
		} else {
			Warn("MQTT", "Unhandled SDP type: %s", sdp.Type)
		}

	case ICEMessage:
		var ice webrtc.ICECandidateInit
		err := json.Unmarshal(signalingMsg.Data, &ice)
		if err != nil {
			Error("MQTT", "Error while parsing ICE: %v", err)
			return
		}
		Debug("MQTT", "ICE candidate received from %s", signalingMsg.Source)
		if mqtt.onRemoteICECandidateReceived != nil {
			mqtt.onRemoteICECandidateReceived(signalingMsg.Source, ice)
		} else {
			Warn("MQTT", "Remote ICE callback not set")
		}
	case QueryMessage:
		Debug("MQTT", "Query request from %s", signalingMsg.Source)
		mqtt.PublishDeviceInfo()
	default:
		Warn("MQTT", "Unhandled message: %s : %s", signalingMsg.Type, msg.Payload())
	}
}

// SendAnswerSDP sends an answer SDP to the given peer.
// The answer is converted to JSON and sent as a message of type SDP.
func (mqtt *MqttClient) SendAnswerSDP(to string, answer *webrtc.SessionDescription) {
	// Marshal the answer to JSON
	data, err := json.Marshal(answer)
	if err != nil {
		Error("MQTT", "Error while encoding SDP: %v", err)
		return
	}

	// Create a SignalingMessage with the answer and send it to the peer
	payload := SignalingMessage{
		Type: SDPMessage,
		Data: data,
	}
	mqtt.Send(to, payload, false)
}

// SendICECandidate sends an ICE candidate to the given peer.
// The candidate is converted to JSON and sent as a message of type ICE.
func (mqtt *MqttClient) SendICECandidate(to string, ice *webrtc.ICECandidate) {
	// Marshal the candidate to JSON
	data, err := json.Marshal(ice.ToJSON())
	if err != nil {
		Error("MQTT", "Error while encoding ICE: %v", err)
		return
	}

	// Create a SignalingMessage with the candidate and send it to the peer
	payload := SignalingMessage{
		Type: ICEMessage,
		Data: data,
	}
	mqtt.Send(to, payload, false)
}

// Hangup sends a hangup request to the given peer.
// The hangup request is sent as a message of type Hangup.
func (mqtt *MqttClient) Hangup(to string) {
	// Create a SignalingMessage of type Hangup and send it to the peer
	Info("MQTT", "Sending hangup to %s", to)
	mqtt.Send(to, SignalingMessage{Source: to, Type: HangupMessage}, false)
}

// SendError sends an error message to the given peer.
// The error is converted to JSON and sent as a message of type Error.
func (mqtt *MqttClient) SendError(to string, err error) {
	// Create a SignalingMessage with the error and send it to the peer
	errorBody, marshalErr := json.Marshal(map[string]string{"message": err.Error()})
	if marshalErr != nil {
		errorBody = json.RawMessage(`{"message":"unknown error"}`)
	}

	payload := SignalingMessage{
		Type: ErrorMessage,
		Data: errorBody,
	}

	// Send the error message to the peer
	Error("MQTT", "Sending error to %s: %v", to, err)
	mqtt.Send(to, payload, false)
}

// Send sends a message to the specified channel on the MQTT broker.
//
// The payload is marshaled to JSON and published to the specified topic.
// The retained flag specifies whether the message should be retained by the broker.
func (mqtt *MqttClient) Send(channel string, payload SignalingMessage, retained bool) {
	// override source
	payload.Source = mqtt.ClientID

	// marshal the payload to JSON
	jsonData, err := json.Marshal(payload)
	if err != nil {
		Error("MQTT", "Error while converting payload: %v", err)
		return
	}

	// construct the topic string
	topic := "/" + channel

	// publish the message to the MQTT broker
	token := mqtt.mqttClient.Publish(topic, 1, retained, jsonData)
	go token.Wait()

	// log the publish action
	Debug("MQTT", "Data sent to %s: %s", topic, string(jsonData))
}
