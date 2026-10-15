package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"
)

// OnEnvelopeFunc is called for each valid `in` message (correct JSON, supported v, acceptable ts
// except for a stale offer, flagged by stale=true). `from` is the last topic level.
type OnEnvelopeFunc func(from string, env Envelope, stale bool)

type MqttClient struct {
	id       string
	cfg      MQTT
	client   pahomqtt.Client
	onEnv    OnEnvelopeFunc
	stateFn  func(online bool) State
	aclCheck sync.Once

	topicIn    string // ocam/dev/{id}/in/
	topicState string // ocam/dev/{id}/state
}

// NewMqttClient prepares the client (without connecting). `stateFn` provides the camera's current state:
// it is used to publish `state` and to build the Last Will.
func NewMqttClient(cfg MQTT, stateFn func(online bool) State) (*MqttClient, error) {
	m := &MqttClient{
		id:         strings.TrimSpace(cfg.Username),
		cfg:        cfg,
		stateFn:    stateFn,
		topicIn:    "ocam/dev/" + cfg.Username + "/in/",
		topicState: "ocam/dev/" + cfg.Username + "/state",
	}

	tlsConfig, err := buildTLSConfig(cfg)
	if err != nil {
		return nil, err
	}
	if !usesTLS(cfg.URL) {
		Warn("MQTT", "Connection WITHOUT TLS: the protocol requires TLS (§ 9), use for local tests only")
	}

	willPayload, err := json.Marshal(stateFn(false))
	if err != nil {
		return nil, err
	}

	opts := pahomqtt.NewClientOptions().
		AddBroker(cfg.URL).
		SetProtocolVersion(4). // MQTT 3.1.1
		SetClientID(m.id).
		SetUsername(cfg.Username).
		SetPassword(cfg.Password).
		SetCleanSession(true).
		SetKeepAlive(30*time.Second).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(5*time.Second).
		SetMaxReconnectInterval(30*time.Second).
		SetWill(m.topicState, string(willPayload), 1, true).
		SetMessageChannelDepth(255).
		SetTLSConfig(tlsConfig).
		SetConnectionLostHandler(func(_ pahomqtt.Client, err error) {
			Error("MQTT", "Connection lost: %v", err)
		}).
		SetReconnectingHandler(func(_ pahomqtt.Client, _ *pahomqtt.ClientOptions) {
			Info("MQTT", "Reconnecting...")
		}).
		SetOnConnectHandler(m.onConnect)

	m.client = pahomqtt.NewClient(opts)
	return m, nil
}

func usesTLS(url string) bool {
	u := strings.ToLower(url)
	return strings.HasPrefix(u, "ssl://") || strings.HasPrefix(u, "tls://") ||
		strings.HasPrefix(u, "tcps://") || strings.HasPrefix(u, "wss://") || strings.HasPrefix(u, "mqtts://")
}

// buildTLSConfig validates the broker certificate (no InsecureSkipVerify). A custom CA
// (mqtt.ca) replaces the system store, useful on embedded systems without an up-to-date CA bundle.
func buildTLSConfig(cfg MQTT) (*tls.Config, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CA == "" {
		return tlsConfig, nil
	}
	pem, err := os.ReadFile(cfg.CA)
	if err != nil {
		return nil, fmt.Errorf("cannot read mqtt.ca: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("mqtt.ca: no valid PEM certificate in %s", cfg.CA)
	}
	tlsConfig.RootCAs = pool
	return tlsConfig, nil
}

// OnEnvelope registers the receiver of `in` messages.
func (m *MqttClient) OnEnvelope(cb OnEnvelopeFunc) { m.onEnv = cb }

// Connect blocks until the first connection (automatic retries).
func (m *MqttClient) Connect() error {
	Info("MQTT", "Connecting to %s as %s", m.cfg.URL, m.id)
	token := m.client.Connect()
	token.Wait()
	return token.Error()
}

func (m *MqttClient) onConnect(client pahomqtt.Client) {
	Info("MQTT", "Connection established")

	topic := m.topicIn + "#"
	client.Subscribe(topic, 1, func(_ pahomqtt.Client, msg pahomqtt.Message) {
		m.handleMessage(msg)
	})
	Info("MQTT", "Subscribed to %s", topic)

	// On every (re)connection: republish the exact state (§ 7).
	m.PublishState(true)

	m.aclCheck.Do(func() { go m.selfTestACL() })
}

// selfTestACL tries a subscription forbidden to a camera (§ 9): if accepted, the ACL is not enforced.
func (m *MqttClient) selfTestACL() {
	const probe = "ocam/dev/+/state"
	token := m.client.Subscribe(probe, 0, func(pahomqtt.Client, pahomqtt.Message) {})
	if !token.WaitTimeout(10 * time.Second) {
		Warn("MQTT", "ACL self-test: no answer from the broker")
		return
	}
	if token.Error() != nil {
		Info("MQTT", "ACL self-test: subscription refused (%v)", token.Error())
		return
	}
	granted := true
	if st, ok := token.(*pahomqtt.SubscribeToken); ok {
		if code, found := st.Result()[probe]; found && code == 0x80 {
			granted = false
		}
	}
	if granted {
		Warn("MQTT", "permissive broker: ACL is not enforced (subscription to %s accepted)", probe)
		m.client.Unsubscribe(probe)
	} else {
		Info("MQTT", "ACL self-test: subscription to %s refused, ACL active", probe)
	}
}

func (m *MqttClient) handleMessage(msg pahomqtt.Message) {
	topic := msg.Topic()
	if !strings.HasPrefix(topic, m.topicIn) {
		return
	}
	from := topic[len(m.topicIn):]
	// The sender is the last topic level: a single level, valid identifier.
	if !idPattern.MatchString(from) {
		Warn("MQTT", "Message ignored: invalid sender in topic %q", topic)
		return
	}

	var env Envelope
	if err := json.Unmarshal(msg.Payload(), &env); err != nil {
		Warn("MQTT", "Message from %s ignored: invalid JSON (%v)", from, err)
		return
	}
	if env.V > ProtocolVersion {
		Warn("MQTT", "Message from %s ignored: unsupported version %d", from, env.V)
		return
	}

	stale := !tsAcceptable(env.TS, time.Now())
	if stale {
		Warn("MQTT", "Message %q from %s outside ts tolerance (%d): clock problem?", env.Type, from, env.TS)
	}
	Debug("MQTT", "Message %q received from %s (sid=%s)", env.Type, from, env.SID)

	if m.onEnv != nil {
		m.onEnv(from, env, stale)
	}
}

// PublishState publishes the retained state (QoS 1).
func (m *MqttClient) PublishState(online bool) {
	st := m.stateFn(online)
	data, err := json.Marshal(st)
	if err != nil {
		Error("MQTT", "Error while encoding state: %v", err)
		return
	}
	Debug("MQTT", "State: %s", data)
	m.publish(m.topicState, true, data)
}

// Send publishes a message to the mailbox of viewer `to`: ocam/cli/{to}/in/{self}.
func (m *MqttClient) Send(to, typ, sid string, data any) {
	var raw json.RawMessage
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			Error("MQTT", "Error while encoding %s data: %v", typ, err)
			return
		}
		raw = b
	}
	env := Envelope{V: ProtocolVersion, TS: time.Now().Unix(), SID: sid, Type: typ, Data: raw}
	payload, err := json.Marshal(env)
	if err != nil {
		Error("MQTT", "Error while encoding %s message: %v", typ, err)
		return
	}
	m.publish("ocam/cli/"+to+"/in/"+m.id, false, payload)
}

func (m *MqttClient) publish(topic string, retained bool, payload []byte) {
	token := m.client.Publish(topic, 1, retained, payload)
	go func() {
		if token.Wait() && token.Error() != nil {
			Warn("MQTT", "Publish on %s failed: %v", topic, token.Error())
		}
	}()
}

// Close publishes the offline state (retained) then disconnects; a clean disconnect does not trigger
// the Last Will.
func (m *MqttClient) Close() {
	if m.client.IsConnectionOpen() {
		token := m.client.Publish(m.topicState, 1, true, mustJSON(m.stateFn(false)))
		token.WaitTimeout(2 * time.Second)
	}
	m.client.Disconnect(250)
	Info("MQTT", "Closed")
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
