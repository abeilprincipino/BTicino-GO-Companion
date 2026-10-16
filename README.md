# BTicino GO Companion

Native Go companion service for BTicino Classe 300X and Classe 100X intercoms.

The companion runs directly on the intercom and exposes a local, authenticated API and WebUI for
state, events, controls, media, diagnostics, and lifecycle management. It is designed to work
together with [BTicino GO Integration](https://github.com/abeilprincipino/BTicino-GO-Integration), the Home
Assistant custom integration that discovers and consumes this service as a local push hub.

For architecture details see [docs/architecture.md](docs/architecture.md). For building from source
and installing a local build see [docs/build-and-install.md](docs/build-and-install.md).

## Table of Contents

- [Prerequisites](#prerequisites)
- [Installation](#installation)
- [What the Installer Does](#what-the-installer-does)
- [Installing a Local Build](#installing-a-local-build)
- [Installer Options](#installer-options)
- [After Installation](#after-installation)
- [Answering Calls from Home Assistant](#answering-calls-from-home-assistant)
- [Service Commands](#service-commands)
- [Updates and Rollback](#updates-and-rollback)
- [Uninstall](#uninstall)

## Prerequisites

- A rooted BTicino Classe 300X or Classe 100X reachable over SSH as `root`.
- Rooting is outside the scope of this project; one documented method for Classe 300X devices is
  available at [`fquinto/bticinoClasse300x`](https://github.com/fquinto/bticinoClasse300x).
- Internet access from the intercom to `github.com` (for the release download), or a local build
  copied over SSH.
- About 20 MB free in `/tmp` and in `/home/bticino/cfg/extra`.
- For SIP, RTSP, WebRTC, and two-way audio, the intercom should already have a working BTicino
  Door Entry mobile app setup. Pairing the official app provisions the Flexisip users and routes
  used by the device media stack.
- If `/etc/flexisip/users/users.db.txt` is empty on a stock or unpaired device, the companion can
  still expose non-media controls where available, but SIP/media features need valid Flexisip
  identity and routing first.

Official Door Entry apps:

| App | Android | iOS |
| --- | --- | --- |
| Classe100X | <a href="https://play.google.com/store/apps/details?id=com.legrandgroup.c100x"><img alt="Get it on Google Play" height="40" src="https://play.google.com/intl/en_us/badges/static/images/badges/en_badge_web_generic.png"></a> | <a href="https://apps.apple.com/it/app/door-entry-classe100x/id1260500637"><img alt="Download on the App Store" height="40" src="https://developer.apple.com/assets/elements/badges/download-on-the-app-store.svg"></a> |
| Classe300X | <a href="https://play.google.com/store/apps/details?id=com.legrandgroup.c300x"><img alt="Get it on Google Play" height="40" src="https://play.google.com/intl/en_us/badges/static/images/badges/en_badge_web_generic.png"></a> | <a href="https://apps.apple.com/it/app/door-entry-classe300x/id1067299253"><img alt="Download on the App Store" height="40" src="https://developer.apple.com/assets/elements/badges/download-on-the-app-store.svg"></a> |

## Installation

Connect to the intercom over SSH and run the installer:

```sh
ssh root@<intercom-ip>
wget -qO /tmp/install.sh "https://github.com/abeilprincipino/BTicino-GO-Companion/raw/main/scripts/install.sh" && sh /tmp/install.sh
```

This installs the latest release. To pin a specific release, set `COMPANION_RELEASE_TAG`:

```sh
wget -qO /tmp/install.sh "https://github.com/abeilprincipino/BTicino-GO-Companion/raw/main/scripts/install.sh" && COMPANION_RELEASE_TAG=v3.0.0 sh /tmp/install.sh
```

The installer prints `INFO:`, `OK:`, `WARN:` and `FAIL:` lines and ends with
`Installation complete.` It exits with code `1` if any post-install check fails; the `FAIL:` lines
say which one.

The same command is used to upgrade an existing installation.

## What the Installer Does

In order:

1. Checks that it runs as `root`, then remounts `/` read-write. `/` is restored read-only on exit,
   even when the installation fails.
2. Downloads the `companion.tar.gz` release bundle and verifies its SHA256 against the GitHub
   release asset digest.
3. Installs the binary to `/home/bticino/cfg/extra/companion/companion`, keeping the previous one as
   `companion.previous`.
4. Installs the bundled GStreamer runtime to `/home/bticino/cfg/extra/companion/gst`, keeping the
   previous one as `gst.previous`.
5. Installs `/etc/init.d/companion` and registers the boot symlinks: `S99zzcompanion` in runlevels
   `2`, `3`, `4`, `5` and `K55companion` in runlevels `0`, `1`, `6`.
6. Persists firewall access in `/etc/network/if-pre-up.d/iptables`:

   | Protocol | Port | Purpose |
   | --- | --- | --- |
   | TCP | `80` | WebUI |
   | TCP | `8080` | HTTP API |
   | TCP | `8554` | RTSP |
   | TCP | `51826` | HomeKit bridge |
   | UDP | `5353` | mDNS |
   | UDP | `8555` | WebRTC ICE |

   If the expected firewall block is not found the installer prints a `WARN:` and continues; the
   init script still opens the ports at every start, so they stay reachable until the next reboot.
7. Starts the service.
8. Provisions the `companion` SIP user in Flexisip so the companion can answer calls (see
   [Answering Calls from Home Assistant](#answering-calls-from-home-assistant)). Skipped with
   `COMPANION_SIP_INBOUND=0`.
9. Runs the post-install checks: binary, init script, boot symlink, running service, GStreamer
   launcher, health endpoint, and read-only root filesystem.

## Installing a Local Build

`install.sh` installs a local binary instead of downloading a release when the binary path is passed
as its argument. The script expects this layout next to itself:

```
<stage>/scripts/install.sh
<stage>/scripts/init.d/companion
<stage>/gst/                      required on a first installation
<stage>/companion                 the binary
```

From a clone of this repository, after building `dist/companion` (see
[docs/build-and-install.md](docs/build-and-install.md)):

```sh
git -c core.autocrlf=false archive HEAD gst scripts | ssh root@<intercom-ip> 'mkdir -p /tmp/stage && tar -x -C /tmp/stage'
scp dist/companion root@<intercom-ip>:/tmp/stage/companion
ssh root@<intercom-ip> 'sh /tmp/stage/scripts/install.sh /tmp/stage/companion'
```

Use `git archive`, not `scp -r`: on Windows checkouts the symlinks in `gst/lib` become text files,
the execute bits are lost and the scripts get CRLF line endings, which produces a broken GStreamer
runtime and an installer that dies with `set: -: invalid option`.

A local build made without `BuildReleaseRepo` has the auto-updater disabled, so it is never replaced
by a GitHub release.

## Installer Options

Environment variables read by `install.sh`:

| Variable | Default | Purpose |
| --- | --- | --- |
| `COMPANION_RELEASE_REPO` | `abeilprincipino/BTicino-GO-Companion` | Repository to download the release from. |
| `COMPANION_RELEASE_TAG` | latest | Release tag to install. |
| `COMPANION_RELEASE_BUNDLE_ASSET` | `companion.tar.gz` | Release asset name. |
| `COMPANION_RELEASE_BASE_URL` | derived from repo and tag | Alternative download location. |
| `COMPANION_RELEASE_API` | derived from repo and tag | Alternative release metadata URL. |
| `COMPANION_RELEASE_BUNDLE_SHA256` | from the GitHub API digest | Expected bundle digest, when the API is not reachable. |
| `COMPANION_HEALTHCHECK_TIMEOUT_SEC` | `45` | Health check timeout. Also applies to local installs. |
| `COMPANION_SIP_INBOUND` | `1` | `0` skips SIP provisioning and leaves the BTicino files untouched. Also applies to local installs. |

Example:

```sh
COMPANION_SIP_INBOUND=0 COMPANION_RELEASE_TAG=v3.0.0 sh /tmp/install.sh
```

## After Installation

Check the service and its log:

```sh
/etc/init.d/companion status
tail -30 /tmp/companion.log
```

Check the health endpoint (no authentication required):

```sh
wget -qO- http://127.0.0.1:8080/api/v3/health
```

Then:

- Open the WebUI at `http://<intercom-ip>/`. On first access it asks you to create the admin
  account.
- The configuration is created on first start at
  `/home/bticino/cfg/extra/companion/config.yaml`. There is nothing to prepare beforehand: it is
  seeded with a default entrypoint (`id: main`, `devaddr: 20`, with stream, unlock and ring
  enabled).
- In Home Assistant, the companion is discovered over mDNS by
  [BTicino GO Integration](https://github.com/abeilprincipino/BTicino-GO-Integration).

## Answering Calls from Home Assistant

The installer adds a `companion@<domain>` SIP user to Flexisip and to the `alluser` group, so the
intercom forks incoming calls to the companion, and enables `companion.sip.inbound` in
`config.yaml`. Backups of the modified files are kept next to them with a `.companion.bak` suffix.

**Flexisip is not restarted by the installer.** It reads `/etc/flexisip/users/*` at startup, so
after `OK: SIP inbound is provisioned.` restart Flexisip or reboot the intercom. Until then
Home Assistant shows the ring but no Answer button.

> **Known limitation.** Logging out and back in from the BTicino Door Entry app rewrites
> `/etc/flexisip/users/*` and removes the `companion` user. Answering then stops working silently:
> the ring is shown but there is no Answer button. Run the installer again to restore it.

## Service Commands

```sh
/etc/init.d/companion start
/etc/init.d/companion stop
/etc/init.d/companion restart
/etc/init.d/companion status
```

The init script starts a watchdog that checks the process and the health endpoint every 15 seconds
and restarts the companion if it is dead or not responding. `stop` also stops the watchdog.

At boot the init script waits up to 60 seconds for `/home/bticino/cfg/extra` to be mounted and up to
30 seconds for the LAN address, and starts Flexisip if the local SIP proxy is not already listening.

## Updates and Rollback

**Installer update.** Run the installer again. The current binary and GStreamer runtime are kept as
`companion.previous` and `gst.previous`.

**API-managed update.** Release builds check GitHub for a newer release periodically. When update
control is enabled and exposed, Home Assistant shows an update entity that can trigger the
companion to download, verify, install and health-check the new version itself, keeping the
previous binary for rollback.

**Binary-only redeploy.** With the GStreamer runtime already installed, a new binary can be staged
as `companion.new`; the init script promotes it on the next start and keeps the old one as
`companion.previous`:

```sh
scp dist/companion root@<intercom-ip>:/home/bticino/cfg/extra/companion/companion.new
ssh root@<intercom-ip> 'chmod 755 /home/bticino/cfg/extra/companion/companion.new && /etc/init.d/companion restart'
```

Do not replace `companion` by hand without going through `restart`: the watchdog would start the old
version again.

**Rollback:**

```sh
cd /home/bticino/cfg/extra/companion
cp -f companion.previous companion.new
/etc/init.d/companion restart
```

## Uninstall

```sh
/etc/init.d/companion stop
mount -o remount,rw /
rm -f /etc/init.d/companion /etc/rc?.d/S99zzcompanion /etc/rc?.d/K55companion
mount -o remount,ro /
rm -rf /home/bticino/cfg/extra/companion
```

The firewall entries added to `/etc/network/if-pre-up.d/iptables` are not removed. To undo the SIP
provisioning, restore the `.companion.bak` copies in `/etc/flexisip/users/` and restart Flexisip.

---

Credits and prior art: this project takes inspiration from the research done in
[`slyoldfox/c300x-controller`](https://github.com/slyoldfox/c300x-controller) and the BTicino
community work around it.
