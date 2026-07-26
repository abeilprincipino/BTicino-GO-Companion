# BTicino GO Companion — Architecture

A single Go binary (`cmd/companion`) that runs **on** a BTicino indoor unit (Classe 300X / 100X)
and exposes the intercom to modern smart-home ecosystems. It speaks the device's native
OpenWebNet + SIP protocols on one side, and HTTP/WebSocket, RTSP/WebRTC and HomeKit
Accessory Protocol on the other.

Generated from the codebase knowledge graph (2068 nodes / 8830 edges) plus
[internal/app/run.go](../internal/app/run.go), which is the single wiring point.

---

## 1. System context

```mermaid
graph LR
    subgraph clients["Consumers"]
        HA["Home Assistant<br/>(BTicinoGO-Integration)"]
        HK["Apple Home<br/>(iOS / HomePod)"]
        BROWSER["Browser<br/>(admin)"]
        RTSPC["RTSP clients<br/>(NVR, VLC)"]
    end

    subgraph device["BTicino indoor unit (Linux, ARM)"]
        COMP["companion<br/>single Go binary"]
        FLEXISIP["Flexisip<br/>local SIP proxy"]
        GST["GStreamer bundle<br/>(gst/)"]
        CFG[("config.yaml<br/>/home/bticino/cfg/extra/companion")]
    end

    subgraph bus["SCS / OpenWebNet bus"]
        EP["Entrypoints<br/>(door stations)"]
    end

    GH["GitHub Releases API"]

    HA -->|"HTTP + WS<br/>:8080 /api/v3, bearer"| COMP
    BROWSER -->|"HTTP :80<br/>/webui/api, session cookie"| COMP
    HK -->|"HAP :51826<br/>+ mDNS"| COMP
    RTSPC -->|"RTSP :8554"| COMP
    HA -.->|"WebRTC<br/>ICE :8555"| COMP

    COMP -->|"OpenWebNet cmd :20000<br/>events mcast :7667"| EP
    COMP -->|"A/V setup :30007"| EP
    COMP <-->|"SIP :5060"| FLEXISIP
    FLEXISIP <-->|"RTP audio/video"| EP
    COMP -->|"transcode / snapshot"| GST
    COMP <--> CFG
    COMP -->|"update check"| GH
```

**Key idea:** the companion does not replace the intercom stack — it *joins* it. It places a SIP
call through the device's own Flexisip proxy to pull the door-station A/V stream, and drives
door locks / audio / voicemail over OpenWebNet.

---

## 2. Package structure

Layers are derived from actual call direction (`boundaries` + `layers` in the graph).
Everything below `internal/`; module path `bticino-go-companion`.

```mermaid
graph TD
    MAIN["cmd/companion<br/>main"] --> APP["app<br/>composition root"]

    subgraph edge["Edge — inbound protocol surfaces"]
        API["api<br/>:8080 /api/v3 + WS"]
        WEBUI["webui<br/>:80 /webui/api + static"]
        HOMEKIT["homekit<br/>HAP bridge :51826"]
        MEDIART["media/rtsp_server<br/>:8554"]
        MEDIAWR["media/webrtc<br/>ICE :8555"]
        MDNS["discovery<br/>mDNS advertise"]
    end

    subgraph domain["Domain"]
        CORE["core<br/>events + Projector (state machine)"]
        DIAG["diagnostics<br/>periodic bus probes"]
    end

    subgraph media_layer["Media pipeline"]
        COORD["media/stream_coordinator<br/>lease arbitration"]
        SRC["media/source_session"]
        RTP["media/rtp_receiver<br/>:5007 video / :5000 audio"]
        BRIDGE["media/audiobridge<br/>Speex &lt;-&gt; Opus via GStreamer"]
        SNAP["media/snapshot"]
        DIST["media/distributor"]
    end

    subgraph protocol["Device protocols"]
        OWN["openwebnet<br/>Control / Listener / AVClient / Trace"]
        SIG["signaling<br/>SIP dialer, SDP, Manager"]
    end

    subgraph platform["Platform / shared"]
        CONFIG["config<br/>Store + Snapshot"]
        AUTH["auth<br/>pairing + bearer"]
        SYSTEM["system<br/>runtime, reboot, updater, metadata"]
        STORAGE["storage"]
        LOGGING["logging"]
        HTTPUTIL["httputil"]
    end

    APP --> API & WEBUI & HOMEKIT & MDNS & DIAG & COORD & OWN & SIG & SYSTEM

    WEBUI --> API
    WEBUI --> HOMEKIT
    WEBUI --> AUTH
    API --> AUTH
    API --> CORE
    API --> MEDIAWR
    API --> SNAP
    API --> SYSTEM

    HOMEKIT --> COORD
    HOMEKIT --> SNAP
    HOMEKIT --> OWN

    MEDIART --> COORD
    MEDIAWR --> COORD
    COORD --> SRC
    SRC --> RTP & SIG & OWN
    SRC --> BRIDGE
    COORD --> DIST
    SNAP --> COORD

    DIAG --> OWN
    OWN --> CORE

    API & WEBUI & HOMEKIT & COORD & AUTH & SYSTEM & OWN --> CONFIG

    classDef core fill:#1f6feb22,stroke:#1f6feb
    class CONFIG,OWN,CORE,COORD core
```

### Structural hotspots (highest fan-in)

| Symbol | Fan-in | Role |
|---|---|---|
| `openwebnet.Client.Unlock` | 93 | door-lock command path — every consumer converges here |
| `webui.server.New` | 79 | admin server constructor |
| `config.Default` | 29 | config baseline, used everywhere incl. tests |
| `homekit.lifecycle.ConfigStore.Snapshot` | 26 | copy-on-read config access |
| `api.writeError` / `writeOK` / `webui.writeJSON` | 20–22 | HTTP response helpers |

`config` (52 inbound, 0 outbound), `core`, `openwebnet` and `api` are true sink layers —
they never call back up into edge packages. That's the invariant to preserve.

---

## 3. Composition root

`app.newRuntime` builds every collaborator once and wires them by interface; `app.run` then
starts the goroutines. Construction order matters — it encodes the dependency DAG.

```mermaid
graph LR
    A["openConfig<br/>detect metadata, create/open config.yaml"] --> B["newRuntime"]
    B --> B1["signaling.NewStreamDialer<br/>(discovers Flexisip domain)"]
    B --> B2["openwebnet.NewTrace<br/>+ NewControl"]
    B --> B3["media.NewSnapshotManager"]
    B --> B4["media.NewRTSPServer<br/>sourceFactory = newBridgeSource"]
    B --> B5["media.NewWebRTCService<br/>(shares Coordinator)"]
    B --> B6["system.NewRuntimeControl<br/>+ NewUpdater"]
    B --> B7["auth.NewStore"]
    B --> B8["homekit.NewManager<br/>SetControllers/Coordinator/Snapshots"]
    B --> B9["core.NewProjector<br/>+ api.NewServer"]
    B --> B10["diagnostics.New"]

    B --> C["run: listeners :8080 / :80"]
    C --> C1["webui.New<br/>SetFrames/HomeKit/Diagnostics/Update"]
    C --> C2["newEventApplier<br/>projector -> homekit.Sync + api.Broadcast"]
    C --> C3["openwebnet.NewListener<br/>frame + message observers"]
    C --> C4["goroutines: listener, homekit.Run,<br/>mdns.Run, checkUpdates, voicemail debounce"]
    C4 --> D["serve()<br/>blocks until ctx cancel"]
```

The single most important line is `newEventApplier`
([run.go:405](../internal/app/run.go#L405)): **all** state change funnels through
`Projector.Apply`, then fans out to HomeKit characteristics and the API WebSocket. There is one
source of truth for state, and one place it is published from.

---

## 4. Runtime flow — a doorbell ring becomes a video stream

```mermaid
sequenceDiagram
    participant EP as Door station
    participant L as openwebnet.Listener
    participant P as core.Projector
    participant HK as homekit.Manager
    participant WS as api WebSocket
    participant CO as StreamCoordinator
    participant SS as SourceSession
    participant SIP as signaling / Flexisip
    participant AB as AudioBridge (GStreamer)
    participant C as Consumer (RTSP / WebRTC / HAP)

    EP-->>L: OpenWebNet frame (mcast :7667)
    L->>P: core.Event (applyEvent)
    P->>HK: Sync(snapshot)
    P->>WS: BroadcastState + BroadcastEvent
    HK-->>C: doorbell characteristic fires

    C->>CO: request stream (lease)
    CO->>SS: Start (via sourceFactory)
    SS->>SIP: INVITE — negotiate SDP (:65000 audio / :65002 video)
    SS->>EP: AVClient start (:30007, high/low res)
    EP-->>SS: RTP video :5007 / audio :5000
    SS->>AB: intercom Speex
    AB-->>C: Opus (WebRTC) / back to bus on talk-back
    SS-->>CO: StreamSnapshot (owner, leaseID)
    CO->>P: StreamStateObserver -> PreviewStarted/Stopped
    Note over CO: single physical source,<br/>arbitrated by lease across all consumers
```

`StreamCoordinator` exists because the hardware allows **one** A/V session at a time. RTSP
clients, the WebRTC preview, HomeKit camera streams and snapshot capture all contend for the
same lease; the coordinator serialises them and reports ownership back into the projector.

---

## 5. Inbound surfaces

| Surface | Bind | Auth | Consumer |
|---|---|---|---|
| Companion API v3 | `:8080` `/api/v3/*` | bearer token, pairing flow (`auth.Store`) | Home Assistant integration |
| Admin Web UI | `:80` `/webui/api/*` + static `/` | username + password hash, session cookie | browser |
| HomeKit (HAP) | `:51826` | HomeKit PIN + SetupID, `hap.FsStore` | Apple Home |
| RTSP | `:8554` | — (LAN) | NVR / players |
| WebRTC | ICE `:8555` | via API bearer (`/api/v3/webrtc/ws`) | HA live preview |
| mDNS | multicast | — | HA discovery (device ID, model, pairing state, instance ID) |

### `/api/v3` — [internal/api/api.go:76-102](../internal/api/api.go#L76-L102)

Unauthenticated: `GET health`, `GET auth/status`, `POST pair/challenge`, `POST pair/claim`,
`POST auth/recover`. Everything else goes through `handleProtected` (bearer):

| Group | Endpoints |
|---|---|
| Auth | `POST auth/rotate`, `POST auth/revoke` |
| State | `GET state`, `GET diagnostics` |
| System | `GET system/update/status`, `POST system/update/{check,stage,install}`, `POST system/reboot`, `POST system/services/{name}/restart` |
| Control | `POST entrypoints/{id}/unlock`, `POST audio/{mute,unmute}`, `POST voicemail/{enable,disable,refresh}` |
| Streams | `GET ws`, `GET webrtc/ws`, `GET entrypoints/{id}/snapshot/latest.jpg` |

Unmatched `/api/v3/` paths hit an explicit `notFound` rather than falling through to static.

### `/webui/api` — [internal/webui/server.go:136-161](../internal/webui/server.go#L136-L161)

`session`, `login`, `admin/{logout,account,restart,reboot}`, `bootstrap/account`,
`config/homeassistant{,/recovery-code}`, `config/entrypoints` (GET/PUT),
`config/homekit{,/qr,/reset}` (GET/PUT/POST), `management/system` (GET/PUT),
`management/diagnostics`, `management/update{,/check}`,
`logs/companion{,/level}`, `logs/openwebnet`. All but `session`/`login`/`bootstrap/account`
are wrapped in `requireReady`.

> **Note:** `get_architecture`'s `routes` aspect reports these as `/api/v2/*` paths that exist
> nowhere in the tree — its route extractor is unreliable for this project. Read the mux
> registrations directly.

---

## 6. Notable design decisions

- **Copy-on-read config.** `config.Store.Snapshot()` returns a value copy; nothing holds a
  pointer into live config. Mutation goes through `Update(func(*Config) error)`.
- **Event sourcing at the edge, projection in the middle.** OpenWebNet frames are parsed into
  `core.Event` values; `Projector` is the only state machine and rejects invalid transitions
  (`ErrInvalidTransition`) rather than silently accepting them.
- **Interface-injected collaborators.** `api.Server` receives capabilities via `SetEntrypoints`,
  `SetAudio`, `SetVoicemail`, `SetWebRTC`, `SetSnapshot`, `SetRuntime`, `SetUpdate` — which is
  why `internal/api/interfaces.go` exists and why the package tests need no device.
- **Self-update in-process.** `system.Updater` polls GitHub releases (20s, then 3h; exponential
  backoff to 1h on failure), stages the artifact and restarts the `companion` service through
  `RuntimeControl`.
- **Debounced voicemail refresh.** An `aswm` ACK frame schedules a single refresh 250ms later
  with drain-on-wake, avoiding a burst of bus queries.
- **Tests colocated and heavy.** 37 of 90 Go files are `_test.go` (588 `TESTS` edges in the
  graph); almost every package ships hand-written fakes rather than a mocking framework.

---

## 7. Related repositories

- **BTicinoGO-Integration** — the Home Assistant custom integration consuming `/api/v2`
  (branch `dev/card` holds the modernized card).
- **c300x-controller** — earlier controller for the same hardware.
