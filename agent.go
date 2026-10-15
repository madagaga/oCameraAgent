package main

import (
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

const (
	connectTimeout = 30 * time.Second // no WebRTC connection after the answer (§ 7)
	iceBatchWindow = 100 * time.Millisecond
)

// Session is the active session of a camera (state `busy(sid, viewer)`).
type Session struct {
	sid     string
	viewer  string
	proxy   *WebRTCProxy
	started time.Time

	answer []byte // last answer, replayed on a duplicate offer

	mu           sync.Mutex
	connectTimer *time.Timer

	// batching of local ICE candidates (§ 6)
	iceMu      sync.Mutex
	iceBuf     []ICECandidate
	iceEnd     bool
	iceTimer   *time.Timer
	iceReady   bool // the answer has been sent: candidates may be sent
	iceStopped bool
}

// Agent implements the session rules of § 7: `idle` or `busy(sid, viewer)`.
type Agent struct {
	cfg    *Config
	mqtt   *MqttClient
	source *SourceMonitor

	mu   sync.Mutex
	sess *Session // nil = idle
}

func NewAgent(cfg *Config) (*Agent, error) {
	a := &Agent{cfg: cfg}
	a.source = NewSourceMonitor(cfg.Device.URL, time.Duration(cfg.Device.ProbeInterval)*time.Second)
	m, err := NewMqttClient(cfg.MQTT, a.state)
	if err != nil {
		return nil, err
	}
	m.OnEnvelope(a.handleEnvelope)
	a.mqtt = m
	return a, nil
}

// Run probes the source once (so the first published state is accurate), connects to the broker and
// starts the periodic source monitoring.
func (a *Agent) Run() error {
	a.source.ProbeNow()
	a.source.onChange = func() { a.mqtt.PublishState(true) }
	if err := a.mqtt.Connect(); err != nil {
		return err
	}
	// the stream itself is the signal during a session: no probing then
	go a.source.Run(func() bool { return a.current() != nil })
	return nil
}

// codecs returns the codecs to use, detected from the camera announcement (video is empty when
// nothing usable is known, audio is empty when the camera has no audio WebRTC can carry).
func (a *Agent) codecs() (video, audio string) {
	info := a.source.Info()
	if info == nil {
		return "", ""
	}
	return firstSupported(info.Video, supportedVideo), firstSupported(info.Audio, supportedAudio)
}

// state builds the published state (§ 5).
func (a *Agent) state(online bool) State {
	st := State{
		V:    ProtocolVersion,
		TS:   time.Now().Unix(),
		Name: a.cfg.Device.Name,
		// the agent and its camera are one entity for viewers: unreachable source = offline
		Online: online && a.source.Online(),
		Kind:   "relay",
		Agent:  &StateInfo{Name: AgentName, Version: AgentVersion},
	}
	// caps are only published once the camera announced something (viewers keep the last known ones)
	if info := a.source.Info(); info != nil {
		st.Caps = &StateCaps{
			Video:   filterSupported(info.Video, supportedVideo),
			Audio:   filterSupported(info.Audio, supportedAudio),
			Streams: []string{"main"},
			Cmds:    []string{},
		}
	}
	if online {
		a.mu.Lock()
		if a.sess != nil {
			v := a.sess.viewer
			st.Busy = true
			st.Viewer = &v
		}
		a.mu.Unlock()
	}
	return st
}

func (a *Agent) handleEnvelope(from string, env Envelope, stale bool) {
	if stale {
		if env.Type == TypeOffer {
			a.sendError(from, env.SID, ErrStale, "clock out of sync (ts outside tolerance)", true)
		}
		return
	}

	switch env.Type {
	case TypeOffer:
		a.handleOffer(from, env)
	case TypeICE:
		a.handleICE(from, env)
	case TypeBye:
		a.handleBye(from, env)
	case TypeCmd:
		a.handleCmd(from, env)
	default:
		Warn("AGENT", "Ignored message type: %q (from %s)", env.Type, from)
	}
}

func (a *Agent) current() *Session {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sess
}

func (a *Agent) handleOffer(from string, env Envelope) {
	if !sidPattern.MatchString(env.SID) {
		a.sendError(from, "", ErrBadRequest, "invalid sid", false)
		return
	}

	// Camera busy: idempotent duplicate, or refusal without touching the active session.
	if cur := a.current(); cur != nil {
		if cur.sid == env.SID && cur.viewer == from {
			Info("AGENT", "Duplicate offer from %s (sid=%s)", from, env.SID)
			if cur.answer != nil {
				a.mqtt.publish("ocam/cli/"+from+"/in/"+a.mqtt.id, false, cur.answer)
			}
			return
		}
		Warn("AGENT", "Offer from %s refused: session active with %s", from, cur.viewer)
		a.sendError(from, env.SID, ErrBusy, "a session is already active", true)
		return
	}

	var data OfferData
	if err := json.Unmarshal(env.Data, &data); err != nil || data.SDP == "" {
		a.sendError(from, env.SID, ErrBadRequest, "invalid offer", false)
		return
	}
	wantAudio := true
	if data.Want != nil {
		if data.Want.Audio != nil {
			wantAudio = *data.Want.Audio
		}
		if data.Want.Stream != "" && data.Want.Stream != "main" {
			a.sendError(from, env.SID, ErrBadRequest, "requested stream does not exist: "+data.Want.Stream, false)
			return
		}
	}

	// A camera believed offline may just be back: probe before refusing.
	if !a.source.Online() && !a.source.ProbeNow() {
		a.sendError(from, env.SID, ErrSourceUnavailable, "camera unreachable", true)
		return
	}
	video, audio := a.codecs()
	if video == "" {
		a.sendError(from, env.SID, ErrUnsupportedCodec, "the camera announces no video codec WebRTC can carry", false)
		return
	}

	s := &Session{sid: env.SID, viewer: from, started: time.Now()}
	proxy := NewWebRTCProxy(a.cfg.Device.URL, video, audio, a.cfg.ICE.Stun)
	s.proxy = proxy
	proxy.onLocalICE = func(c *webrtc.ICECandidate) { s.queueICE(a, c) }
	proxy.onConnected = func() { a.onConnected(s) }
	proxy.onEnd = func(reason string) { a.endSession(s, reason, true) }

	answer, err := proxy.HandleOffer(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: data.SDP}, wantAudio)
	if err != nil {
		var se *SessionError
		if !errors.As(err, &se) {
			se = newSessionError(ErrInternal, err.Error())
		}
		a.sendError(from, env.SID, se.Code, se.Msg, se.Code == ErrInternal)
		return
	}

	payload, err := a.envelope(TypeAnswer, env.SID, AnswerData{SDP: answer.SDP})
	if err != nil {
		proxy.Close()
		a.sendError(from, env.SID, ErrInternal, "cannot encode the answer", true)
		return
	}
	s.answer = payload

	a.mu.Lock()
	a.sess = s
	a.mu.Unlock()

	Info("AGENT", "Session started: sid=%s viewer=%s", s.sid, s.viewer)

	a.mqtt.publish("ocam/cli/"+from+"/in/"+a.mqtt.id, false, payload)
	a.mqtt.PublishState(true)
	s.startICE(a)

	s.mu.Lock()
	s.connectTimer = time.AfterFunc(connectTimeout, func() {
		Warn("AGENT", "No WebRTC connection within %s (sid=%s)", connectTimeout, s.sid)
		a.endSession(s, ByeTimeout, true)
	})
	s.mu.Unlock()
}

func (a *Agent) handleICE(from string, env Envelope) {
	cur := a.current()
	if cur == nil || cur.sid != env.SID || cur.viewer != from {
		Debug("AGENT", "ice ignored (sid=%s, from %s)", env.SID, from)
		return
	}
	var data ICEData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		Warn("AGENT", "invalid ice from %s: %v", from, err)
		return
	}
	for _, c := range data.Candidates {
		if c.Candidate == "" {
			continue
		}
		init := webrtc.ICECandidateInit{Candidate: c.Candidate, SDPMid: c.SDPMid, SDPMLineIndex: c.SDPMLineIndex}
		if err := cur.proxy.AddICECandidate(init); err != nil {
			Warn("AGENT", "Remote candidate rejected: %v", err)
		}
	}
}

func (a *Agent) handleBye(from string, env Envelope) {
	cur := a.current()
	if cur == nil || cur.sid != env.SID || cur.viewer != from {
		Debug("AGENT", "bye ignored (sid=%s, from %s)", env.SID, from)
		return
	}
	var data ByeData
	_ = json.Unmarshal(env.Data, &data)
	Info("AGENT", "bye from %s (reason=%q)", from, data.Reason)
	a.endSession(cur, ByeUser, false)
}

// handleCmd: the RTSP agent has no commands (empty caps.cmds): everything gets `unsupported`.
func (a *Agent) handleCmd(from string, env Envelope) {
	var data CmdData
	if err := json.Unmarshal(env.Data, &data); err != nil || data.ID == "" {
		Warn("AGENT", "invalid cmd from %s", from)
		return
	}
	a.mqtt.Send(from, TypeRes, "", ResData{
		ID:    data.ID,
		OK:    false,
		Error: &ResError{Code: ErrUnsupported, Message: "unsupported command: " + data.Name},
	})
}

func (a *Agent) onConnected(s *Session) {
	s.mu.Lock()
	if s.connectTimer != nil {
		s.connectTimer.Stop()
		s.connectTimer = nil
	}
	s.mu.Unlock()
	Info("AGENT", "WebRTC connection established (sid=%s)", s.sid)
}

// endSession ends session s (if it is still the active one): closes the
// PeerConnection and RTSP, sends `bye` if requested, republishes the state and logs the end.
func (a *Agent) endSession(s *Session, reason string, sendBye bool) {
	a.mu.Lock()
	if a.sess != s {
		a.mu.Unlock()
		return
	}
	a.sess = nil
	a.mu.Unlock()

	s.mu.Lock()
	if s.connectTimer != nil {
		s.connectTimer.Stop()
		s.connectTimer = nil
	}
	s.mu.Unlock()
	s.stopICE()
	s.proxy.Close()

	if reason == ByeSourceLost {
		a.source.MarkDown()
	}

	if sendBye {
		a.mqtt.Send(s.viewer, TypeBye, s.sid, ByeData{Reason: reason})
	}
	Info("AGENT", "Session ended: sid=%s viewer=%s reason=%s duration=%s",
		s.sid, s.viewer, reason, time.Since(s.started).Round(time.Second))
	a.mqtt.PublishState(true)
}

// Shutdown closes the current session, if any (bye `shutdown`), then publishes the offline state.
func (a *Agent) Shutdown() {
	a.source.Stop()
	if s := a.current(); s != nil {
		a.mu.Lock()
		a.sess = nil
		a.mu.Unlock()
		s.stopICE()
		s.proxy.Close()
		a.mqtt.Send(s.viewer, TypeBye, s.sid, ByeData{Reason: ByeShutdown})
		Info("AGENT", "Session ended: sid=%s viewer=%s reason=%s duration=%s",
			s.sid, s.viewer, ByeShutdown, time.Since(s.started).Round(time.Second))
	}
	a.mqtt.Close()
}

func (a *Agent) envelope(typ, sid string, data any) ([]byte, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Envelope{V: ProtocolVersion, TS: time.Now().Unix(), SID: sid, Type: typ, Data: raw})
}

func (a *Agent) sendError(to, sid, code, msg string, retryable bool) {
	Warn("AGENT", "error %s -> %s : %s", code, to, msg)
	a.mqtt.Send(to, TypeError, sid, ErrorData{Code: code, Message: msg, Retryable: retryable})
}

// --- ICE candidate batching ---

// startICE allows candidates to be sent (after the answer) and sends those already collected.
func (s *Session) startICE(a *Agent) {
	s.iceMu.Lock()
	s.iceReady = true
	pending := len(s.iceBuf) > 0 || s.iceEnd
	s.iceMu.Unlock()
	if pending {
		s.flushICE(a)
	}
}

func (s *Session) queueICE(a *Agent, c *webrtc.ICECandidate) {
	s.iceMu.Lock()
	if s.iceStopped {
		s.iceMu.Unlock()
		return
	}
	if c == nil {
		s.iceEnd = true
	} else {
		j := c.ToJSON()
		s.iceBuf = append(s.iceBuf, ICECandidate{Candidate: j.Candidate, SDPMid: j.SDPMid, SDPMLineIndex: j.SDPMLineIndex})
	}
	if !s.iceReady {
		s.iceMu.Unlock()
		return
	}
	if c == nil {
		s.iceMu.Unlock()
		s.flushICE(a)
		return
	}
	if s.iceTimer == nil {
		s.iceTimer = time.AfterFunc(iceBatchWindow, func() { s.flushICE(a) })
	}
	s.iceMu.Unlock()
}

func (s *Session) flushICE(a *Agent) {
	s.iceMu.Lock()
	if s.iceStopped || (len(s.iceBuf) == 0 && !s.iceEnd) {
		s.iceMu.Unlock()
		return
	}
	batch := s.iceBuf
	end := s.iceEnd
	s.iceBuf = nil
	s.iceEnd = false
	if s.iceTimer != nil {
		s.iceTimer.Stop()
		s.iceTimer = nil
	}
	s.iceMu.Unlock()

	if batch == nil {
		batch = []ICECandidate{}
	}
	a.mqtt.Send(s.viewer, TypeICE, s.sid, ICEData{Candidates: batch, End: end})
}

func (s *Session) stopICE() {
	s.iceMu.Lock()
	s.iceStopped = true
	if s.iceTimer != nil {
		s.iceTimer.Stop()
		s.iceTimer = nil
	}
	s.iceMu.Unlock()
}
