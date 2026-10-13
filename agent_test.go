package main

import (
	"bufio"
	"bytes"
	"net"
	"testing"
	"time"
)

func TestTsAcceptable(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		ts   int64
		want bool
	}{
		{now.Unix(), true},
		{now.Add(-29 * time.Second).Unix(), true},
		{now.Add(31 * time.Second).Unix(), false},
		{now.Add(-2 * time.Minute).Unix(), false},
	}
	for _, c := range cases {
		if got := tsAcceptable(c.ts, now); got != c.want {
			t.Errorf("ts=%d: got %v want %v", c.ts, got, c.want)
		}
	}
	// clock not synchronized: check skipped
	if !tsAcceptable(0, time.Date(1970, 1, 2, 0, 0, 0, 0, time.UTC)) {
		t.Error("the ts check must be skipped when the clock is not synchronized")
	}
}

func TestRedactURL(t *testing.T) {
	if got := redactURL("rtsp://admin:secret@10.0.0.1:554/s"); got != "rtsp://***@10.0.0.1:554/s" {
		t.Errorf("got %q", got)
	}
	if got := redactURL("rtsp://10.0.0.1/s"); got != "rtsp://10.0.0.1/s" {
		t.Errorf("got %q", got)
	}
}

func TestConfigValidate(t *testing.T) {
	ok := Config{
		MQTT:   MQTT{URL: "ssl://b:8883", Username: "cam-1", Password: "x"},
		Device: Device{URL: "rtsp://h/s", Video: "H264", Audio: "PCMU"},
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	for _, bad := range []string{"ab", "cam/1", "cam+1", "a-very-long-identifier-exceeding-32-chars", ""} {
		c := ok
		c.MQTT.Username = bad
		if c.Validate() == nil {
			t.Errorf("identifier %q accepted", bad)
		}
	}
}

// A stray frame before a valid one must not make the stream drift (resynchronization),
// and an RTCP / too short frame must be fully consumed.
func TestHandleTCPPacketsResync(t *testing.T) {
	rtp := make([]byte, 20)
	rtp[0] = 0x80
	frame := func(ch byte, payload []byte) []byte {
		return append([]byte{0x24, ch, byte(len(payload) >> 8), byte(len(payload))}, payload...)
	}

	var stream bytes.Buffer
	stream.Write([]byte{0x00, 0x11, 0x22})   // stray bytes
	stream.Write(frame(1, make([]byte, 30))) // RTCP: ignored
	stream.Write(frame(0, make([]byte, 5)))  // too short: ignored
	stream.Write(frame(0, rtp))              // valid
	stream.Write(frame(2, rtp))              // valid (audio)

	client, server := net.Pipe()
	go func() { server.Write(stream.Bytes()) }()

	rc := NewRtspClient()
	rc.socket = client
	rc.reader = bufio.NewReader(client)
	rc.payload = make([]byte, 8)

	got := 0
	deadline := time.After(3 * time.Second)
	for got < 2 {
		select {
		case <-deadline:
			t.Fatalf("only %d packets received", got)
		default:
		}
		if err := rc.handleTCPPackets(); err != nil {
			t.Fatal(err)
		}
		for len(rc.received) > 0 {
			p := <-rc.received
			if len(p) != 24 || p[0] != 0x24 {
				t.Fatalf("unexpected packet: %v", p)
			}
			got++
		}
	}
	if len(rc.payload) < 20 {
		t.Error("the grown buffer must be kept")
	}
}
