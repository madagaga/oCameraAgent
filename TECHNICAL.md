# oCamera agent - technical documentation

Reference for how the agent works and how to configure it. For the overview, see `README.md`.
Signaling uses the oCamera protocol version 1 (`MQTT_PROTOCOL.md` at the root of the project).

The agent relays an RTSP camera stream to a WebRTC viewer as a pure pass-through (no decoding or
transcoding). The video and audio never go through the broker: MQTT only carries the SDP/ICE exchange,
the camera presence and small commands. Media flows peer-to-peer over WebRTC.

## Build

```bash
go build -o oCameraAgent .
```

For embedded targets (static musl build), use `build-musl.ps1` (`-Update` refreshes the dependencies first).

## Run

```bash
./oCameraAgent -config config.yaml [-debug]
```

| Flag | Description |
|---|---|
| `-config` | Path of the configuration file (default `config.yaml`) |
| `-debug` | Enable debug logs |

## Configuration

See `config.yaml.template`.

| Key | Required | Description |
|---|---|---|
| `mqtt.url` | yes | Broker URL, e.g. `ssl://broker:8883` |
| `mqtt.username` | yes | **Camera identifier** (3-32 chars among `[A-Za-z0-9_-]`); also used as MQTT client id |
| `mqtt.password` | yes | Password of the camera account |
| `mqtt.ca` | no | PEM file of a custom CA used to validate the broker (defaults to the system store) |
| `device.url` | yes | Source RTSP URL (credentials may be embedded) |
| `device.name` | no | Display name shown by viewers (defaults to the identifier) |
| `device.probe_interval` | no | Seconds between source probes (default 30, minimum 5) |
| `ice.stun` | no | List of STUN servers (defaults to Google public STUN). No TURN is needed on the agent side |

Notes:
- The broker certificate is always validated (no insecure mode). Plain `tcp://` URLs work but log a
  warning and are meant for local tests only.
- Passwords and RTSP credentials are never logged.

## Behavior

### Identity and topics

The camera identifier is its MQTT username. The agent uses:

| Topic | Direction | Content |
|---|---|---|
| `ocam/dev/{id}/state` | published, retained | Camera state (`online`, `busy`, `viewer`, `caps`, ...) |
| `ocam/dev/{id}/in/{viewer}` | subscribed | Messages from a viewer: `offer`, `ice`, `bye`, `cmd` |
| `ocam/cli/{viewer}/in/{id}` | published | Messages to a viewer: `answer`, `ice`, `bye`, `error`, `res` |

The sender of a message is the last level of its topic. Every message is a JSON envelope
`{v, ts, sid, type, data}`; unknown fields and types are ignored, and messages whose `ts` is more than
30 s away from the local clock are dropped (an `offer` gets `error stale`). The `ts` check is skipped
while the clock is not synchronized.

### Source detection and camera status

The agent is a pure pass-through: RTP packets are forwarded as they come from the camera, without decoding or transcoding. The codecs are therefore **detected, not configured**.

- At startup the agent opens a short RTSP session (`OPTIONS` + `DESCRIBE`, no `SETUP`/`PLAY`) and reads the codecs the camera announces. The first video codec WebRTC can carry (`h264`, `h265`, `vp8`, `vp9`, `av1`) is used, and likewise for audio (`pcmu`, `pcma`, `opus`). A camera without such an audio track (e.g. AAC) simply streams without audio.
- The detected codecs are published in `caps.video` / `caps.audio` of `state`.
- The source is probed again every `device.probe_interval` seconds while idle (no probing during a session: the stream itself is the signal). The camera is declared **offline** (`online: false`) after 3 consecutive failed probes, and back online after one success; the codecs are refreshed on every success.
- The agent and its camera are one entity for viewers: an unreachable source means `online: false`.
- An `offer` received while the camera is offline triggers an immediate probe; if it still fails, the answer is `error source_unavailable`. If the camera announces no video codec WebRTC can carry, the answer is `error unsupported_codec`.
- Losing the stream during a session (`source_lost`) marks the camera offline immediately.

### Presence

- `state` is published (retained) on every (re)connection and on every change.
- The MQTT Last Will publishes `online: false, busy: false` if the agent disappears.
- On a clean shutdown the agent ends the active session (`bye`, reason `shutdown`) and publishes the
  offline state itself.
- Losing the MQTT connection does **not** interrupt an established WebRTC session; the exact state is
  republished when the connection comes back.

### Sessions

A camera serves **one session at a time** (`idle` or `busy(sid, viewer)`).

- `offer` while idle: the agent creates the PeerConnection, answers, becomes busy and republishes `state`.
  The RTSP stream is opened only once the WebRTC connection is established.
- Duplicate `offer` (same `sid` and viewer): the previous `answer` is sent again.
- `offer` from anyone else while busy: `error busy`; the active session is left untouched.
- `ice` and `bye` are accepted only from the viewer and `sid` of the active session.
- ICE candidates are sent in batches (about 100 ms window) after the answer, with `end: true` after the last one.
  Both trickle and non-trickle viewers are supported.

A session ends on:

| Cause | `bye` reason sent to the viewer |
|---|---|
| Viewer sends `bye` | (none) |
| No WebRTC connection 30 s after the answer | `timeout` |
| WebRTC `failed`, or `disconnected` for more than 10 s | `ice_failed` |
| RTSP source lost after 5 reconnection attempts | `source_lost` |
| Agent shutdown | `shutdown` |

Session start (`sid`, viewer) and end (reason, duration) are always logged.

### Errors

`error` messages carry a code and end the session; they are never followed by a `bye`:
`busy`, `bad_request` (invalid offer or unknown stream), `source_unavailable` (camera unreachable), `unsupported_codec` (no usable video codec announced by the camera, or the offer lacks it), `stale`, `internal`.

Audio is added only if the viewer asks for it (`want.audio` not `false`) and the offer contains an audio
section.

### Commands

The agent exposes no command (`caps.cmds` is empty): every `cmd` is answered by a `res` with
`ok: false` and code `unsupported`.

### Security self-test

At startup the agent tries to subscribe to `ocam/dev/+/state`, which a camera account must not be allowed
to read. If the broker accepts it, a warning is logged (`permissive broker: ACL is not enforced`) and the
agent keeps running. See annex A of the protocol for the Mosquitto ACL.

## Source layout

| File | Role |
|---|---|
| `main.go` | Entry point, signal handling |
| `config.go` | Configuration loading and validation |
| `protocol.go` | Envelope, message types, error codes, state structures |
| `source.go` | Source probing, codec detection, offline detection |
| `mqtt.go` | MQTT connection, topics, Last Will, publishing, ACL self-test |
| `agent.go` | Session state machine, ICE batching |
| `webrtc.go` | PeerConnection and RTP relay of one session |
| `rtsp_client.go` | RTSP client (TCP interleaved) |

## Tests

```bash
go test .
```
