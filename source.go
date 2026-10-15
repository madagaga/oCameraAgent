package main

import (
	"sync"
	"time"
)

const (
	defaultProbeInterval = 30 * time.Second
	minProbeInterval     = 5 * time.Second
	// number of consecutive failed probes before an online source is declared offline
	probeFailThreshold = 3
)

// Codecs a WebRTC session can carry, in order of preference.
var (
	supportedVideo = []string{"h264", "h265", "vp8", "vp9", "av1"}
	supportedAudio = []string{"pcmu", "pcma", "opus"}
)

// SourceMonitor watches the RTSP source: it detects what the camera announces (codecs) and whether
// it is reachable. An unreachable source makes the whole camera offline (the agent and the camera
// are one and the same for viewers).
type SourceMonitor struct {
	url      string
	interval time.Duration
	probe    func(url string) (*SourceInfo, error) // replaceable in tests

	// onChange is called (without lock held) when reachability or detected codecs change.
	onChange func()

	mu       sync.Mutex
	online   bool
	failures int
	info     *SourceInfo // last successful probe

	stop chan struct{}
	once sync.Once
}

func NewSourceMonitor(url string, interval time.Duration) *SourceMonitor {
	if interval < minProbeInterval {
		interval = defaultProbeInterval
	}
	return &SourceMonitor{url: url, interval: interval, probe: ProbeSource, stop: make(chan struct{})}
}

// Online tells whether the source is currently considered reachable.
func (m *SourceMonitor) Online() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.online
}

// Info returns the codecs announced by the last successful probe (nil if none yet).
func (m *SourceMonitor) Info() *SourceInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.info
}

// ProbeNow probes the source immediately and updates the state. The first probe (nothing known yet)
// declares the source offline right away on failure; afterwards offline needs several failures.
func (m *SourceMonitor) ProbeNow() bool {
	info, err := m.probe(m.url)

	m.mu.Lock()
	wasOnline := m.online
	changed := false
	if err == nil {
		m.failures = 0
		m.online = true
		if !sameInfo(m.info, info) {
			m.info = info
			changed = true
		}
		if !wasOnline {
			changed = true
			Info("SOURCE", "Source reachable (video=%v audio=%v)", info.Video, info.Audio)
		}
	} else {
		m.failures++
		Warn("SOURCE", "Probe failed (%d/%d): %v", m.failures, probeFailThreshold, err)
		if m.info == nil || m.failures >= probeFailThreshold {
			if m.online {
				Warn("SOURCE", "Source unreachable: camera is now offline")
				changed = true
			}
			m.online = false
		}
	}
	online := m.online
	m.mu.Unlock()

	if changed && m.onChange != nil {
		m.onChange()
	}
	return online
}

// MarkDown declares the source offline immediately (e.g. the stream was lost during a session).
func (m *SourceMonitor) MarkDown() {
	m.mu.Lock()
	changed := m.online
	m.online = false
	m.failures = probeFailThreshold
	m.mu.Unlock()
	if changed {
		Warn("SOURCE", "Source lost: camera is now offline")
		if m.onChange != nil {
			m.onChange()
		}
	}
}

// Run probes periodically until Stop. `paused` (optional) returns true while probing must be skipped,
// e.g. during an active session where the stream itself is the signal.
func (m *SourceMonitor) Run(paused func() bool) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if paused != nil && paused() {
				continue
			}
			m.ProbeNow()
		case <-m.stop:
			return
		}
	}
}

func (m *SourceMonitor) Stop() { m.once.Do(func() { close(m.stop) }) }

func sameInfo(a, b *SourceInfo) bool {
	if a == nil || b == nil {
		return a == b
	}
	return equalStrings(a.Video, b.Video) && equalStrings(a.Audio, b.Audio)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// firstSupported returns the first announced codec that WebRTC can carry ("" if none).
func firstSupported(announced, supported []string) string {
	for _, a := range announced {
		for _, s := range supported {
			if a == s {
				return a
			}
		}
	}
	return ""
}

// filterSupported keeps the announced codecs WebRTC can carry, preserving the announced order
// (never nil, so it serializes as an empty JSON list).
func filterSupported(announced, supported []string) []string {
	out := []string{}
	for _, a := range announced {
		for _, s := range supported {
			if a == s {
				out = append(out, a)
				break
			}
		}
	}
	return out
}
