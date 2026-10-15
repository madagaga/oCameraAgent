# oCamera

**Watch your IP cameras from anywhere, directly. No cloud, no account, no port forwarding.**

oCamera connects your phone straight to your RTSP camera over an encrypted peer-to-peer WebRTC link.
Your video never touches a vendor server: it goes from the camera to your phone, and nowhere else.

```
 Camera (RTSP) ─▶ oCamera agent ◀── MQTT broker (signaling only) ──▶ oCamera app (Flutter)
                       ╚═══════════ WebRTC, encrypted, peer-to-peer ═══════════╝
```

## Why oCamera

- **Your video stays yours.** No vendor cloud, no "P2P service" run by a third party. The stream is
  end-to-end encrypted (WebRTC DTLS-SRTP) between the agent and your phone.
- **Nothing to expose.** The agent only makes outgoing connections. No port forwarding, no dynamic DNS,
  no VPN to maintain, nothing listening on your network.
- **Light.** A single static Go binary (about 10 MB for ARM and MIPS), and in the order of 13 to 16 MB of RAM
  in use on x64 (figures vary with the target architecture). It runs on the
  box next to the camera: a router, a Raspberry Pi, a NAS, a small x86 board, an old ARM or MIPS device.
- **Zero configuration of codecs.** The agent asks the camera what it offers and adapts. No transcoding,
  so almost no CPU: it simply forwards the packets.
- **Knows when a camera is down.** Cameras appear online or offline in the app in real time.
- **Open source.** Read it, build it, self-host it.

## What you need

1. A camera that speaks **RTSP** (most IP cameras do).
2. A small always-on machine to run the **agent** (Windows, Linux x86/ARM/MIPS...).
3. An **MQTT broker** reachable by both the agent and the phone. This is the one piece of infrastructure
   oCamera relies on, and it only carries a few small messages to set up each connection (no video).
4. The **oCamera app** on your phone.

### Where to get an MQTT broker

You can run your own or use a free hosted one. The agent and the app speak MQTT 3.1.1 over TLS with a
login and password.

| Option | Notes |
|---|---|
| **Self-hosted [Mosquitto](https://mosquitto.org)** | Free and open source. Full control, including per-account ACLs (recommended, see `MQTT_PROTOCOL.md`, annex A). |
| **[HiveMQ Cloud](https://www.hivemq.com/mqtt-cloud-broker/)** | Free hosted tier, TLS included. |
| **[EMQX Cloud](https://www.emqx.com/en/cloud)** | Free serverless tier, TLS included. |
| Other brokers (EMQX, NanoMQ, VerneMQ...) | Anything that supports MQTT 3.1.1, TLS and password authentication. |

Free tiers and their limits change: check the provider's current offer. Public test brokers
(`test.mosquitto.org`, `broker.emqx.io`...) are fine to experiment, but anyone can read them: do not use
them for real cameras. For the strongest isolation between users, a broker that enforces per-account topic
ACLs (such as your own Mosquitto) is the best choice.

STUN (to find a path through your router) uses free public servers by default, and no media goes through
them. If your network blocks direct connections (strict corporate NAT, some mobile carriers), a TURN server
can be added on the app side.

## Quick start

1. Create an account for the camera and one for the phone on your broker.
2. Download or build the agent (`TECHNICAL.md`), then copy `config.yaml.template` to `config.yaml`:

   ```yaml
   mqtt:
     url: "ssl://your-broker:8883"
     username: "cam-entree"
     password: "..."
   device:
     name: "Entrance"
     url: "rtsp://user:pass@192.168.1.20:554/stream"
   ```
3. Run it: `./oCameraAgent -config config.yaml`
4. Open the oCamera app, enter your broker address and your phone account, and your camera appears.

All options are documented in [`TECHNICAL.md`](TECHNICAL.md).

## The oCamera app

A [Flutter](https://github.com/madagaga/oCamera) client for Android. It signs in to your broker,
lists your cameras with their live online/offline status, and opens the video directly from the camera
over WebRTC. It is open source too.

## oCamera vs go2rtc

[go2rtc](https://github.com/AlexxIT/go2rtc) is an excellent, much more general streaming server. oCamera is
deliberately narrower. Pick the one that fits your need.

| | **oCamera** | **go2rtc** |
|---|---|---|
| Goal | Reach your camera remotely, privately, with nothing exposed | Universal stream hub: many sources and many outputs |
| Remote access | Outgoing connections only, through an MQTT broker used for signaling | Typically needs a reachable endpoint (forwarded port, VPN or tunnel) |
| Setup on the camera side | Agent + config file (URL, broker, account) | Config file, plus network exposure for remote use |
| Footprint | Single static binary (about 10 MB), roughly 13-16 MB RAM on x64 | Single binary, larger feature set |
| Sources / outputs | RTSP in, WebRTC out | Many protocols in and out (RTSP, HLS, MSE, WebRTC...), optional transcoding with ffmpeg |
| Viewers | One at a time per camera | Many simultaneous consumers |
| Client | Dedicated Flutter app (Android) | Web UI, integrations (e.g. Home Assistant), any compatible player |
| Extra infrastructure | An MQTT broker (can be free or self-hosted) | None required for local use |

**Choose oCamera** if you want a private, no-exposure way to watch your cameras from your phone, on
modest hardware. **Choose go2rtc** if you need to convert between protocols, share one camera with many
viewers, transcode, or integrate with a home automation hub. They also work together: the agent only needs
an RTSP URL, which go2rtc can provide.

## Honest limits

- One viewer per camera at a time; a second viewer is told the camera is busy.
- The agent forwards the camera's codecs as they are (no transcoding): the phone must be able to decode
  what the camera sends (H.264 works everywhere).
- It needs an MQTT broker. If you want something that works with no infrastructure at all, this is not it.
- No recording, no motion detection: oCamera is for live viewing.

## Documentation

- [`TECHNICAL.md`](TECHNICAL.md): configuration, behavior, sessions, errors, source layout
- `MQTT_PROTOCOL.md` (project root): the signaling protocol and the Mosquitto ACL setup

## Build

```bash
go build -o oCameraAgent .
```

Static builds for ARM, ARM64, MIPS and x64: see `build-musl.ps1`.
