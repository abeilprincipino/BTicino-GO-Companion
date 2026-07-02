# BTicino GO Companion

[![GitHub Release](https://img.shields.io/github/v/release/r0bb10/BTicino-GO-Companion)](https://github.com/r0bb10/BTicino-GO-Companion/releases/latest)
[![Release](https://github.com/r0bb10/BTicino-GO-Companion/actions/workflows/release.yaml/badge.svg)](https://github.com/r0bb10/BTicino-GO-Companion/actions/workflows/release.yaml)
[![GitHub Issues](https://img.shields.io/github/issues/r0bb10/BTicino-GO-Companion)](https://github.com/r0bb10/BTicino-GO-Companion/issues)
[![Top Language](https://img.shields.io/github/languages/top/r0bb10/BTicino-GO-Companion)](https://go.dev/)
[![API](https://img.shields.io/badge/API-v2-blue)](internal/adapters/http/v2/router.go)
[![Home Assistant](https://img.shields.io/badge/Home%20Assistant-local_push-blue)](https://github.com/r0bb10/BTicino-GO-Integration)

Native Go companion service for BTicino Classe 300X and Classe 100X intercoms.

The companion runs directly on the intercom and exposes a local, authenticated API for state, events, controls, media, diagnostics, and lifecycle management. It is designed to work together with [BTicino GO Integration](https://github.com/r0bb10/BTicino-GO-Integration), the Home Assistant custom integration that discovers, pairs with, and consumes this service as a local push hub.

This project takes inspiration from the excellent research done in [`slyoldfox/c300x-controller`](https://github.com/slyoldfox/c300x-controller). The goal here is a native Go service with Go-native protocol handling, explicit API contracts, and integrated media/control services that can be installed as a small init-managed daemon on the intercom.

## Table of Contents

- [What It Is](#what-it-is)
- [Feature Overview](#feature-overview)
- [How It Works](#how-it-works)
- [Home Assistant Integration](#home-assistant-integration)
- [API Surface](#api-surface)
- [Media Pipeline](#media-pipeline)
- [Authentication and Discovery](#authentication-and-discovery)
- [Configuration](#configuration)
- [Installation](#installation)
- [Updates and Rollback](#updates-and-rollback)
- [Development](#development)
- [Project Status](#project-status)

## What It Is

BTicino GO Companion is the on-device service.

It runs on the BTicino unit and talks to the local device services:

- OpenWebNet TCP command socket on `127.0.0.1:20000`.
- OpenWebNet/syslog multicast events on `239.255.76.67:7667`.
- SIP/flexisip for incoming calls and camera stream negotiation.
- RTP media ports used by the intercom audio/video stream.
- Local voicemail files stored by the device.
- Local init/system services such as `dropbear`.

BTicino GO Integration is the Home Assistant side.

It discovers the companion over mDNS, pairs with the claim-code flow, consumes `/api/v2/...`, creates native HA entities, subscribes to server-sent events, and exposes camera, WebRTC, switches, sensors, buttons, update entities, and events inside Home Assistant.

## Feature Overview

### Core Service

- Single native Go daemon built for Linux ARMv7.
- HTTP API on `0.0.0.0:8080` by default.
- Versioned `/api/v2` contract.
- Health endpoint with runtime readiness.
- Structured state projection for call, stream, audio, voicemail, entrypoint, and diagnostic status.
- Server-sent events with replay by `last_event_id`.
- OpenWebNet trace capture and trace SSE stream for debugging.
- Config persistence in `/home/bticino/cfg/extra/companion/config.json` by default.
- Optional `-config` flag for a custom config file.

### OpenWebNet

- Native OpenWebNet command client.
- Optional HMAC-style OpenWebNet authentication support when the command socket requires it.
- Door/gate unlock pulse handling with open and close frames.
- Stream start command fallback for audio/video RTP ports.
- Ringer mute/unmute control.
- Mute status bootstrap probe.
- Voicemail enable/disable control.
- Voicemail status bootstrap probe.
- Diagnostic frame queries for IP, netmask, MAC, firmware, hardware, kernel, and distribution.
- TCP command trace records for transmitted and received frames.
- UDP multicast listener for OpenWebNet/ASWM events.
- Frame parsing, event mapping, deduplication, and typed event validation.

### Events and State

- `ring.started`, `ring.ended`.
- `ring.floor.started`, `ring.floor.ended`.
- `call.incoming`, `call.answered`, `call.ended`, `call.view_requested`.
- `stream.started`, `stream.stopped`.
- `unlock.pulse.started`, `unlock.pulse.ended`, `unlock.triggered`.
- `audio.muted`, `audio.unmuted`.
- `voicemail.enabled`, `voicemail.disabled`.
- `heartbeat` events for liveness.
- Invalid events are converted to `event.invalid` instead of silently corrupting state.

### Entrypoints

- Configurable entrypoints instead of a hardcoded single doorbell path.
- Each entrypoint can expose unlock, ring, and stream capabilities independently.
- Default entrypoint is `main`, label `Main Gate`, device address `20`.
- RTSP route metadata is generated per entrypoint and exposed through `/api/v2/entrypoints`.
- Stream lifecycle prevents unsafe switching while another entrypoint has active readers.

### SIP

- Native SIP stack using [`emiago/sipgo`](https://github.com/emiago/sipgo).
- SIP server and client in the companion process.
- Registration loop with refresh before expiry.
- Incoming INVITE handling with ringing state.
- Call answer and hangup API controls.
- Outgoing stream INVITE for camera viewing.
- Busy handling when another call/stream is active.
- SDP generation and parsing helpers.
- Automatic SIP profile discovery/fallback logic from local device files.

### RTSP and RTP Media

- Native RTSP server using [`bluenviron/gortsplib`](https://github.com/bluenviron/gortsplib).
- Default RTSP listener on `:8554`.
- H.264 video RTP ingest.
- Speex intercom audio RTP ingest.
- Reader-driven stream start/stop.
- Watchdog for idle RTSP readers.
- Static stream exposed for known entrypoint routes.
- RTP packet fan-out into native WebRTC sessions.
- Return audio handling for two-way audio paths.
- Snapshot mirror support for JPEG capture.

### Native WebRTC

- Native WebRTC service using [`pion/webrtc`](https://github.com/pion/webrtc).
- Default ICE UDP port `8555`.
- Interface filtering selects the preferred outbound network interface.
- H.264 video track from the intercom RTP stream.
- Opus audio track for browser/Home Assistant WebRTC consumers.
- WebRTC offer/answer endpoint.
- Trickle ICE candidate endpoint.
- Explicit session close endpoint.
- Pending ICE candidate queue for candidates received before the session exists.
- WebRTC session lifecycle joins/leaves the same stream lifecycle used by RTSP readers.
- Browser microphone return audio is accepted as Opus RTP and bridged back toward the intercom audio path.

### Audio Bridge

- Integrated GStreamer-managed bridge for Speex and Opus conversion.
- Downlink path converts intercom Speex RTP to Opus RTP for WebRTC.
- Uplink path converts WebRTC Opus RTP back to Speex RTP for the intercom.
- Uses the device GStreamer where appropriate and the bundled GStreamer runtime where needed.
- Managed process lifecycle with start/stop guards and restart protection.
- Default bridge ports are internal localhost UDP ports; the bundled bridge runtime lives under the companion data directory.

### Snapshots

- Snapshot capture endpoint per stream-capable entrypoint.
- Latest snapshot endpoint serving the last captured JPEG.
- Snapshot capture can start the stream temporarily and stop it again afterwards.
- Uses a snapshot RTP mirror plus a GStreamer JPEG pipeline.
- Stores snapshots under `/home/bticino/cfg/extra/companion/media/snapshots` by default.
- Captures a snapshot automatically when a stream starts, when possible.

### Voicemail

- Voicemail enable/disable control on supported models.
- Voicemail status in state.
- Voicemail message listing from the device message directory.
- Safe serving of voicemail assets with path traversal protection.
- C100X handling disables voicemail control paths where not supported.

### System Controls

- Optional reboot endpoint.
- Exposed service control list, defaulting to `dropbear`.
- Service status endpoint.
- Service restart endpoint.
- System update status/check/apply/rollback endpoints.
- Update exposure and rollback are config gated.

### Diagnostics

- Device model detection.
- Firmware, hardware, kernel, and distribution metadata.
- Network IP, netmask, MAC, and Wi-Fi signal refresh loop.
- Runtime readiness for SIP, OpenWebNet, and control subsystems.
- Health endpoint combines state boot time and runtime status.

### Installation and Operations

- Release workflow builds a static ARMv7 binary with embedded version metadata.
- Release bundle includes the companion binary, init script, installer, and GStreamer runtime.
- Single-pass installer downloads `companion.tar.gz`, verifies SHA256 from the GitHub release digest, installs files, registers init service, patches firewall rules, starts the service, and runs post-install checks.
- Init service opens required runtime ports at start and removes those runtime rules on stop.
- Installer persists firewall openings for HTTP, RTSP, mDNS, and WebRTC ICE.
- Previous binary is kept as `companion.previous` to support rollback.

## How It Works

The companion is organized as a set of internal services connected by typed events.

1. The app loads or creates config.
2. Device metadata is detected locally and enriched through OpenWebNet diagnostic frames.
3. Auth state is initialized from the same persisted config file.
4. mDNS advertisement starts so Home Assistant can discover the device.
5. SIP, OpenWebNet, RTSP, WebRTC, diagnostics, updater, state projection, trace, and event brokers are wired together.
6. OpenWebNet multicast frames and API/SIP/media transitions are normalized into typed events.
7. The state projector applies those events into a canonical snapshot.
8. Home Assistant reads snapshots and stays current through SSE.

The important design point is that the companion does not treat media, controls, and events as unrelated endpoints. A stream reader, a WebRTC session, a SIP call, a snapshot capture, and an API command all pass through the same lifecycle and state projection. That is why Home Assistant can show consistent `call_state`, `stream_active`, `active_entrypoint`, audio mute, voicemail, and runtime availability.

## Home Assistant Integration

The companion is intended to be used with [BTicino GO Integration](https://github.com/r0bb10/BTicino-GO-Integration).

The integration provides the Home Assistant side:

- Zeroconf discovery for `_bticomp._tcp.local.`.
- Config flow and pairing with the companion claim code.
- Local push operation through `/api/v2/events` SSE.
- Polling fallback through `/api/v2/health`, `/api/v2/state`, `/api/v2/entrypoints`, and `/api/v2/capabilities`.
- Native HA entities for sensors, binary sensors, switches, buttons, cameras, events, and updates.
- RTSP camera stream source generated from companion entrypoint metadata.
- Native HA WebRTC camera handling through companion offer/candidate/close endpoints.
- OpenWebNet trace relay into Home Assistant events.
- Service calls for answering, hanging up, muting, unmuting, voicemail, unlocking, rebooting, and refresh.
- Claim recovery/repair flow when stored credentials are no longer valid.

The split is intentional:

- The companion owns device-local protocols, media, authentication, and state.
- The integration owns Home Assistant entities, UI/config flow, service registration, and HA runtime behavior.

## API Surface

Public bootstrap endpoints:

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/api/v2/health` | Liveness/readiness payload. |
| `POST` | `/api/v2/pair/challenge` | Start claim challenge. |
| `POST` | `/api/v2/pair/claim` | Claim device and receive bearer token. |
| `GET` | `/api/v2/auth/status` | Check claim status. Requires auth after claim. |

Protected read endpoints:

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/api/v2/capabilities` | API capabilities and system control exposure. |
| `GET` | `/api/v2/entrypoints` | Entrypoints plus RTSP route metadata. |
| `GET` | `/api/v2/state` | Canonical state snapshot. |
| `GET` | `/api/v2/events` | SSE event stream with replay support. |
| `GET` | `/api/v2/trace/openwebnet` | OpenWebNet trace replay. |
| `GET` | `/api/v2/trace/openwebnet/stream` | OpenWebNet trace SSE stream. |
| `GET` | `/api/v2/voicemail/messages` | Voicemail message list. |
| `GET` | `/api/v2/voicemail/messages/{message_id}/{asset}` | Voicemail asset. |
| `GET` | `/api/v2/entrypoints/{id}/snapshot/latest.jpg` | Latest JPEG snapshot. |

Protected control endpoints:

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/api/v2/control/call/answer` | Answer incoming call. |
| `POST` | `/api/v2/control/call/hangup` | Hang up or reject call. |
| `POST` | `/api/v2/control/audio/mute` | Mute ringer/audio path. |
| `POST` | `/api/v2/control/audio/unmute` | Unmute ringer/audio path. |
| `POST` | `/api/v2/control/voicemail/enable` | Enable voicemail. |
| `POST` | `/api/v2/control/voicemail/disable` | Disable voicemail. |
| `POST` | `/api/v2/control/entrypoints/{id}/unlock` | Unlock an entrypoint. |
| `POST` | `/api/v2/control/entrypoints/{id}/stream/start` | Manually start an entrypoint stream. |
| `POST` | `/api/v2/control/entrypoints/{id}/stream/stop` | Stop a manually held stream. |
| `POST` | `/api/v2/control/entrypoints/{id}/snapshot` | Capture a JPEG snapshot. |
| `POST` | `/api/v2/control/system/reboot` | Reboot the device when enabled. |
| `GET` | `/api/v2/control/system/services` | List exposed services. |
| `GET` | `/api/v2/control/system/services/{name}/status` | Service status. |
| `POST` | `/api/v2/control/system/services/{name}/restart` | Restart exposed service. |
| `GET` | `/api/v2/control/system/update/status` | Update status. |
| `POST` | `/api/v2/control/system/update/check` | Check release or supplied artifact metadata. |
| `POST` | `/api/v2/control/system/update/apply` | Apply available or supplied update. |
| `POST` | `/api/v2/control/system/update/rollback` | Roll back when allowed and available. |
| `POST` | `/api/v2/webrtc/offer` | Create WebRTC answer for an entrypoint. |
| `POST` | `/api/v2/webrtc/candidate` | Add remote ICE candidate. |
| `POST` | `/api/v2/webrtc/close` | Close WebRTC session. |

Protected auth/admin endpoints:

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/api/v2/auth/rotate` | Rotate bearer token. |
| `POST` | `/api/v2/auth/revoke` | Revoke current key id and replace token. |
| `POST` | `/api/v2/admin/issue-repair-code` | Issue temporary repair code. |
| `POST` | `/api/v2/admin/reset-claim` | Reset claim state using repair code. |

## Media Pipeline

The companion handles media locally instead of depending on a separate Node.js/WebRTC bundle.

RTSP path:

1. A Home Assistant camera or RTSP client opens an entrypoint path on port `8554`.
2. The RTSP server joins the stream lifecycle as a reader.
3. The media service starts the intercom stream through SIP or OpenWebNet fallback.
4. Intercom RTP video/audio is ingested on configured local ports.
5. H.264 and audio packets are published to RTSP readers.
6. When readers leave and there is no manual hold, the stream is stopped.

WebRTC path:

1. Home Assistant sends a WebRTC offer to `/api/v2/webrtc/offer`.
2. The companion joins the same stream lifecycle as a reader.
3. Pion creates H.264 and Opus tracks.
4. RTP packets from the RTSP/media ingest are written into WebRTC tracks.
5. ICE uses UDP port `8555` and the selected outbound interface.
6. Browser microphone audio is received as Opus RTP, bridged to Speex, and forwarded toward the intercom return-audio path.

Snapshot path:

1. A snapshot request starts the stream if needed.
2. A temporary RTP mirror waits for H.264 SPS/PPS/IDR frames.
3. GStreamer decodes one frame and writes a JPEG.
4. The latest image is atomically published for Home Assistant camera thumbnails.

Bidirectional audio path:

1. The intercom sends audio as Speex RTP.
2. The companion feeds that RTP into a managed GStreamer downlink bridge.
3. The downlink bridge converts Speex to raw PCM/L16, then to Opus RTP for WebRTC consumers.
4. Browser or Home Assistant microphone audio arrives back at the companion as Opus RTP.
5. The managed uplink bridge converts Opus to raw PCM/L16, then back to Speex RTP.
6. The companion forwards the Speex return audio toward the intercom return-audio endpoint.
7. The bridge uses the device GStreamer where suitable and the bundled GStreamer runtime for the parts that need newer or additional plugins.

## Authentication and Discovery

The companion starts unclaimed unless a persisted claim exists.

- A human claim code is generated on first config creation.
- The claim code is persisted in `config.json`.
- Pairing uses a challenge ID, nonce, and claim code.
- After claim, the API returns a bearer token and key id.
- Protected endpoints require `Authorization: Bearer <token>`.
- Tokens can be rotated or revoked/replaced.
- Repair code flow can reset claim state when Home Assistant credentials need recovery.

mDNS advertisement uses `_bticomp._tcp` by default and publishes TXT metadata:

- `api=v2`
- `scheme=http`
- `model=...`
- `fw=...`
- `name=...`
- `device_id=...`
- `needs_claim=true|false`

Home Assistant uses this to discover the companion and start the config flow.

## Configuration

Default config path:

```sh
/home/bticino/cfg/extra/companion/config.json
```

The persisted schema stores:

- System control exposure and update settings.
- Exposed init services.
- Companion model and auth state.
- Entrypoint definitions.
- Audio mute control exposure.
- Voicemail message path and toggle exposure.
- OpenWebNet command password when needed.

Default runtime values include:

| Setting | Default |
| --- | --- |
| HTTP API | `0.0.0.0:8080` |
| Data dir | `/home/bticino/cfg/extra/companion` |
| mDNS service | `_bticomp._tcp` |
| OpenWebNet multicast | `239.255.76.67:7667` |
| OpenWebNet command | `127.0.0.1:20000` |
| SIP listen | `0.0.0.0:5070` |
| RTSP listen | `:8554` |
| WebRTC ICE UDP | `8555` |
| RTP audio ingest | `5000` |
| RTP video ingest | `5007` |
| Voicemail messages | `/home/bticino/cfg/extra/47/messages` |

Example config generated on a new install:

```json
{
  "schema_version": 2,
  "system": {
    "control": {
      "reboot": {
        "enabled": true
      },
      "update": {
        "enabled": true,
        "exposed": false,
        "allow_rollback": false,
        "manifest_path": "",
        "release_api": "https://api.github.com",
        "release_repo": "r0bb10/BTicino-GO-Companion",
        "release_asset": "companion.tar.gz",
        "service_script": "/etc/init.d/companion",
        "health_timeout_sec": 8
      }
    },
    "services": {
      "dropbear": {
        "enabled": true,
        "exposed": true
      }
    },
    "future": {}
  },
  "companion": {
    "info": {
      "model": "C300X"
    },
    "auth": {
      "device_id": "c300x_aabbccddeeff",
      "claimed": false,
      "claim_code": "1234-5678",
      "bearer_token": "",
      "key_id": ""
    },
    "config": {
      "entrypoints": [
        {
          "id": "main",
          "label": "Main Gate",
          "devaddr": "20",
          "has_stream": true,
          "has_unlock": true,
          "has_ring": true
        }
      ],
      "audio": {
        "enabled": true,
        "exposed": true
      },
      "voicemail": {
        "messages_dir": "/home/bticino/cfg/extra/47/messages",
        "enabled": true,
        "exposed": true
      }
    }
  }
}
```

`claim_code`, `device_id`, model, and runtime metadata are generated or detected on the device. For `C100X`, voicemail is normalized to disabled and not exposed. `openwebnet_command_password` is omitted unless configured.

### C100X AV endpoint & debug logging

On the C100X the SIP INVITE alone does not direct RTP to the companion: the
stream destination must be pushed to the intercom's AV server (`bt_ipcamera`,
TCP `127.0.0.1:30007`) with `*7*300#<ip>#<port>#<quality>*##` add-stream
frames. The companion does this automatically when the device model is
`C100X`. While a call is ringing or active the SIP INVITE is skipped entirely
(it would be answered `486 Busy Here`); a 486 reply is likewise treated as
"call in progress" and the companion falls through to AV commands only.

`config.json` keys (under `companion.config`):

| Key | Default | Meaning |
|---|---|---|
| `media.av_endpoint_enabled` | auto (C100X only) | Force the AV endpoint backend on/off |
| `media.av_endpoint_host` | `127.0.0.1` | AV server host |
| `media.av_endpoint_port` | `30007` | AV server port |
| `media.av_high_res_video` | `false` | Request high-res video (`#0`) instead of low-res (`#1`) |
| `debug.log_enabled` | `false` | Mirror logs to a file (the init script discards stdout/stderr) |
| `debug.log_path` | `/tmp/companion-debug.log` | Log file path (tmpfs by default; rotated once at 5 MB) |

## Installation

The release bundle is intended to be installed directly on the intercom.

Prerequisites:

- A rooted BTicino Classe 300X or Classe 100X reachable over SSH.
- Rooting is outside the scope of this project; one documented method for Classe 300X devices is available at [`fquinto/bticinoClasse300x`](https://github.com/fquinto/bticinoClasse300x).
- For SIP, RTSP, WebRTC, and two-way audio, the intercom should already have a working BTicino Door Entry mobile app setup. Pairing the official app provisions the Flexisip users and routes used by the device media stack.
- If `/etc/flexisip/users/users.db.txt` is empty on a stock or unpaired device, the companion can still expose non-media controls where available, but SIP/media features need valid Flexisip identity and routing first.

Official Door Entry apps:

| App | Android | iOS |
| --- | --- | --- |
| Classe100X | <a href="https://play.google.com/store/apps/details?id=com.legrandgroup.c100x"><img alt="Get it on Google Play" height="40" src="https://play.google.com/intl/en_us/badges/static/images/badges/en_badge_web_generic.png"></a> | <a href="https://apps.apple.com/it/app/door-entry-classe100x/id1260500637"><img alt="Download on the App Store" height="40" src="https://developer.apple.com/assets/elements/badges/download-on-the-app-store.svg"></a> |
| Classe300X | <a href="https://play.google.com/store/apps/details?id=com.legrandgroup.c300x"><img alt="Get it on Google Play" height="40" src="https://play.google.com/intl/en_us/badges/static/images/badges/en_badge_web_generic.png"></a> | <a href="https://apps.apple.com/it/app/door-entry-classe300x/id1067299253"><img alt="Download on the App Store" height="40" src="https://developer.apple.com/assets/elements/badges/download-on-the-app-store.svg"></a> |

Download and run the installer:

```sh
wget -qO- "https://raw.githubusercontent.com/r0bb10/BTicino-GO-Companion/main/scripts/install.sh" | sh
```

The installer will:

- Download the latest `companion.tar.gz` release bundle.
- Verify the bundle SHA256 using the GitHub release asset digest.
- Install the binary to `/home/bticino/cfg/extra/companion/companion`.
- Install the bundled GStreamer runtime to `/home/bticino/cfg/extra/companion/gst`.
- Remount `/` read-write when system files need to be changed, then restore it read-only before finishing.
- Install `/etc/init.d/companion`.
- Register boot symlinks for runlevels `2`, `3`, `4`, and `5`.
- Persist firewall access for TCP `8080` and `8554`.
- Persist firewall access for UDP `5353` and `8555`.
- Start the service.
- Check the binary, init script, boot symlink, service status, GStreamer runtime, health endpoint, and read-only root filesystem.

Local install from a built binary is also supported:

```sh
sh scripts/install.sh ./dist/companion
```

Service commands:

```sh
/etc/init.d/companion start
/etc/init.d/companion stop
/etc/init.d/companion restart
/etc/init.d/companion status
```

Health check:

```sh
wget -qO- http://127.0.0.1:8080/api/v2/health
```

After first start, inspect the generated config for the claim code used by Home Assistant pairing:

```sh
grep claim_code /home/bticino/cfg/extra/companion/config.json
```

## Updates and Rollback

The companion has two update paths.

Installer update:

- Run `scripts/install.sh` again.
- The current binary is copied to `companion.previous`.
- The new release bundle replaces `companion`.
- The service is restarted and health-checked.

API-managed update:

- The companion can discover updates from a configured local manifest or from GitHub latest release metadata.
- A background update check runs periodically, currently every 3 hours after startup.
- `/api/v2/control/system/update/check` reads a local manifest or latest GitHub release metadata.
- `/api/v2/control/system/update/apply` downloads or uses an artifact, verifies SHA256 when provided, extracts the companion binary, stores the previous binary, activates the candidate, restarts the service, and performs a health window check.
- `/api/v2/control/system/update/rollback` restores `companion.previous` when rollback is allowed and possible.

Home Assistant can expose this through the BTicino GO Integration update entity. When update control is enabled and exposed in the companion config, Home Assistant can show the installed version, detect the latest available release, and trigger the companion to self-update through the same API-managed flow.

Update controls are config gated. They can be enabled internally without exposing them to Home Assistant until desired.

## Project Status

This README is a technical map of what the companion currently implements.

The project is still evolving, but the implemented direction is clear:

- Keep the device side small and native.
- Prefer Go libraries for protocols and runtime services.
- Keep Home Assistant integration local, push-first, and entity-native.
- Avoid webhook registration loops where the companion can publish canonical state/events directly.
- Treat OpenWebNet, SIP, RTP, RTSP, WebRTC, snapshots, and controls as one coordinated local service instead of separate scripts.

Credits and prior art: the BTicino community work around `c300x-controller` made much of this exploration possible.