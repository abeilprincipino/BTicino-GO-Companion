# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

BTicino GO Companion: a single static Go binary (`cmd/companion`) that runs **on** a rooted BTicino
Classe 300X / 100X indoor unit (Linux, ARMv7) and exposes it over HTTP/WebSocket, RTSP, WebRTC and
HomeKit. Its main consumer is the Home Assistant integration (`BTicino-GO-Integration`, separate repo).
Deeper references: [docs/architecture.md](docs/architecture.md), [docs/build-and-install.md](docs/build-and-install.md),
design specs in `docs/superpowers/specs/`, investigations in `docs/analysis/`.

## Commands

The dev host is Windows; the target is linux/arm. Module path is `bticino-go-companion`, Go 1.26.

```sh
# Build the device binary (auto-updater disabled because BuildReleaseRepo is empty)
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -buildvcs=false -trimpath \
  -ldflags "-s -w -X bticino-go-companion/internal/system.BuildVersion=v0.0.0-local \
  -X bticino-go-companion/internal/system.BuildGitSHA=$(git rev-parse --short=7 HEAD) \
  -X bticino-go-companion/internal/system.BuildReleaseRepo=" -o dist/companion ./cmd/companion

# Type-check the whole tree, tests included, for the real target (works on Windows)
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go vet ./...

# Tests (CI runs go vet ./... and go test ./... on ubuntu-latest)
go test ./internal/signaling/...                 # single package
go test ./internal/core -run TestProjector       # single test
docker build --target test .                     # full suite in Linux

# Lint (config in .golangci.yml: wsl_v5, godot, funlen, sloglint, gosec, revive...)
golangci-lint run ./...

# Installer/uninstaller logic tests (POSIX sh)
sh scripts/tests/run.sh

# Web UI on the workstation, without an intercom (default :8081)
go run ./cmd/webui-dev
```

**Tests on Windows:** `internal/media` does not compile on Windows (`syscall.Setpgid`/`Kill` in
`audiobridge.go`), and that also breaks the tests of `homekit`, `openwebnet`, `webui`, `diagnostics`;
`config`, `storage`, `logging` and `system/update` fail for environmental reasons. Packages like
`internal/signaling` and `internal/core` run natively. For media changes, verify with
`GOOS=linux go build ./internal/media/` (or the `go vet` above), say the tests were not run, and leave
execution to CI — do not cross-compile test binaries to run them under WSL.

Deploying to a device: see the one-liners in `docs/build-and-install.md` §0. Always transfer `gst/` and
`scripts/` with `git -c core.autocrlf=false archive HEAD gst scripts | ssh ...`, never `scp -r` (Windows
checkouts lose symlinks/exec bits in `gst/lib`). Quick redeploy = copy the binary to
`/home/bticino/cfg/extra/companion/companion.new` and `/etc/init.d/companion restart` (the watchdog
restarts the old binary if you replace it any other way). Device log: `/tmp/companion.log`.

## Architecture

`internal/app/run.go` is the single composition root: `newRuntime` builds every collaborator and wires
them through interfaces (e.g. `api.Server.Set*`, see `internal/api/interfaces.go`), `run` starts the
goroutines. Packages below `app` must not call back up into edge packages; `config`, `core`,
`openwebnet` and `api` are sink layers.

**State flow.** OpenWebNet frames (multicast listener) and SIP events become `core.Event`s.
`core.Projector` is the only state machine and rejects invalid transitions. `newEventApplier` in
`run.go` is the single publication point: Projector → `homekit.Sync` + API WebSocket broadcast.

**Device protocols.**
- `openwebnet`: commands on :20000, AV control on :30007 (`AVClient`, `*7*300` frames), bus frame parsing/mapping.
- `signaling`: SIP via the device's local Flexisip (sipgo). Outbound INVITE for on-demand view;
  inbound calls arrive because the installer adds `companion@<domain>` to Flexisip's `alluser` group.

**How media actually flows (non-obvious).** The SIP call carries no media: the SDP advertises
placeholder ports 65000/65002 with `inline:dummykey` (`signaling/sdp.go`). The usable stream is
**clear RTP requested separately over OpenWebNet AV** (`*7*300` to :30007) towards the local
`RTPReceiver` ports. A ring "ringing preview" borrows the inbound call (`signaling.Manager.AdoptRingingCall`),
asks for video only, and on answer adds audio via `SourceSession.Answered` without restarting video.

**Media pipeline (`internal/media`).**
- `StreamCoordinator`: the hardware allows one A/V session, so every consumer (WebRTC, RTSP, HomeKit,
  snapshot, ring clip) takes a lease; it also reports stream state back to the Projector.
- `SourceSession`: SIP + AV + receivers for one physical stream; `app.ringHolder` holds one open
  during the ring so snapshot/clip/preview share it.
- `fanout` → consumers, with keyframe cache/replay for late joiners; `snapshot` decodes the first IDR
  with a `gst-launch` pipeline; `ringclip` records the ring.
- `audiobridge`: intercom Speex ↔ Opus via `gst-launch` processes using the bundled GStreamer in `gst/`.

**GStreamer on the device** is 1.12.4: no partial caps filter right after a caps-less `fdsrc`; always
capture `gst-launch` stderr and exit status in the warning log.

## Conventions that matter

- **Config compatibility:** `config.yaml` is loaded with `KnownFields(true)` and the Beta channel can
  downgrade, so every new persisted key must be omitted while at its default (`omitempty`). Old
  configs must load with unchanged behaviour. `config.Store.Snapshot()` returns a copy; mutate only
  via `Update(func(*Config) error)`.
- Tests use hand-written fakes, no mocking framework.
- Logging is `log/slog` (enforced by `sloglint`).
- Line endings are LF everywhere (`.gitattributes`); the scripts run on the intercom's BusyBox sh.

## Branches and releases

`develop` is the default branch and the PR target; betas (`vX.Y.Z-beta.N`) are tagged on `develop`.
`main` only receives already-shipped changes via a merge-commit PR from `develop`, and official tags
go on `main`. The Release workflow can create the tag itself (Actions → Release → Run workflow).
PR descriptions should state whether the change was tried on a real intercom and which model.
