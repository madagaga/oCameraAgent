package main

import (
	"encoding/json"
	"regexp"
	"time"
)

// MQTT signaling protocol version 1 (see MQTT_PROTOCOL.md).

const (
	ProtocolVersion = 1
	AgentName       = "ocamera-go"
	AgentVersion    = "1.0.0"

	// tolerance on the ts field (§ 4)
	maxClockSkew = 30 * time.Second
)

// Message types (§ 4)
const (
	TypeOffer  = "offer"
	TypeAnswer = "answer"
	TypeICE    = "ice"
	TypeBye    = "bye"
	TypeError  = "error"
	TypeCmd    = "cmd"
	TypeRes    = "res"
)

// Error codes (§ 6)
const (
	ErrBusy              = "busy"
	ErrBadRequest        = "bad_request"
	ErrUnsupportedCodec  = "unsupported_codec"
	ErrSourceUnavailable = "source_unavailable"
	ErrStale             = "stale"
	ErrInternal          = "internal"
	ErrUnsupported       = "unsupported" // reply to an unknown command
)

// Reasons of a bye (§ 6)
const (
	ByeUser       = "user"
	ByeTimeout    = "timeout"
	ByeICEFailed  = "ice_failed"
	ByeSourceLost = "source_lost"
	ByeShutdown   = "shutdown"
)

var sidPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,32}$`)

// Envelope is the envelope of `in` messages (§ 4).
type Envelope struct {
	V    int             `json:"v"`
	TS   int64           `json:"ts"`
	SID  string          `json:"sid,omitempty"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

type OfferData struct {
	SDP  string `json:"sdp"`
	Want *struct {
		Stream string `json:"stream"`
		Audio  *bool  `json:"audio"`
	} `json:"want"`
}

type AnswerData struct {
	SDP string `json:"sdp"`
}

type ICECandidate struct {
	Candidate     string  `json:"candidate"`
	SDPMid        *string `json:"sdpMid"`
	SDPMLineIndex *uint16 `json:"sdpMLineIndex"`
}

type ICEData struct {
	Candidates []ICECandidate `json:"candidates"`
	End        bool           `json:"end"`
}

type ByeData struct {
	Reason string `json:"reason,omitempty"`
}

type ErrorData struct {
	Code      string `json:"code"`
	Message   string `json:"message,omitempty"`
	Retryable bool   `json:"retryable"`
}

type CmdData struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type ResError struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

type ResData struct {
	ID    string    `json:"id"`
	OK    bool      `json:"ok"`
	Error *ResError `json:"error,omitempty"`
}

// State is the retained state of a camera (§ 5).
type State struct {
	V      int        `json:"v"`
	TS     int64      `json:"ts"`
	Name   string     `json:"name"`
	Online bool       `json:"online"`
	Busy   bool       `json:"busy"`
	Viewer *string    `json:"viewer"`
	Kind   string     `json:"kind"`
	Caps   *StateCaps `json:"caps,omitempty"`
	Agent  *StateInfo `json:"agent,omitempty"`
}

type StateCaps struct {
	Video   []string `json:"video"`
	Audio   []string `json:"audio"`
	Streams []string `json:"streams"`
	Cmds    []string `json:"cmds"`
}

type StateInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// clockSynced tells whether the local clock looks synchronized (§ 4: year >= 2024).
func clockSynced(now time.Time) bool { return now.Year() >= 2024 }

// tsAcceptable applies the ts tolerance check; skipped while the clock is not synchronized.
func tsAcceptable(ts int64, now time.Time) bool {
	if !clockSynced(now) {
		return true
	}
	d := now.Sub(time.Unix(ts, 0))
	if d < 0 {
		d = -d
	}
	return d <= maxClockSkew
}
