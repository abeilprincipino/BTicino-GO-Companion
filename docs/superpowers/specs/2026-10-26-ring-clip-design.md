# Doorbell Ring Clip — Design

**Date:** 2026-10-26
**Status:** Approved for planning
**Repos:** `BTicinoGO` (companion), `BTicinoGO-Integration` (integration and card, branch `v3`)

---

## 1. Goal

Record a short video clip of every doorbell ring, also when nobody answers and no card is open,
keep the history in Home Assistant, and show the latest clips in the intercom card as a carousel
the user can play from.

Decisions taken with the user:

| Question | Decision |
|---|---|
| Length | The first **15 s** of video |
| Someone answers | The recording **stops at the answer**; the clip recorded so far is kept |
| Storage | **At most one clip per entrypoint on the intercom**; Home Assistant keeps the history |
| Retention in Home Assistant | Configurable in days, default **30**, `0` = never delete |
| Card | Carousel of the latest clips under the controls, playable in the viewer |

## 2. What the code already gives

- **The ring holder** (`internal/app/ringholder.go`, spec 2026-10-22) opens the intercom stream on
  every SIP ring and keeps it until the answer or the end of the ring, whether or not a card is
  open. It acquires with empty `SourceEvents`.
- **The shared fan-out** (`internal/media/fanout.go`) replays the keyframe cache to every consumer
  that joins, so a consumer added while a card already holds the stream starts from an IDR.
- **Pure-Go muxing is already in the module graph:** `gortsplib/v5/pkg/format/rtph264` turns RTP
  into H.264 access units, `mediacommon/v2/pkg/formats/pmp4` writes a progressive MP4. No new
  download, no transcoding, no GStreamer plugin. The bundled GStreamer has neither `isomp4` nor
  `videoparsersbad`.
- The video is H.264, `profile-level-id=42801F`, packetization mode 1. The SDP carries no
  `sprop-parameter-sets`: **SPS and PPS arrive only in band.**

Known constraints:

- **The clip is video only.** The intercom sends no audio before the answer (spec 2026-10-15,
  verified on the device), and the recording stops at the answer.
- **The clip starts about 2–3 s after the ring** (INVITE, video start, first IDR) and so covers
  roughly +2.5 s to +17.5 s.
- **The intercom's flash is small** (`/home/bticino/cfg/extra/companion`): one file per entrypoint,
  bounded in size.

Out of scope: audio in the clip; recording live views or answered calls; a video attachment in
mobile notifications (needs a signed URL reachable from the phone); RTSP or HomeKit recording.

## 3. Design — companion

### 3.1 `RingClipRecorder` (`internal/media/ringclip.go`, new)

```go
func NewRingClipRecorder(dir string, logger *slog.Logger) *RingClipRecorder
func (r *RingClipRecorder) SetOnSaved(func())
func (r *RingClipRecorder) Start(entrypointID string) *RingClip
func (r *RingClipRecorder) Open(entrypointID string) (*os.File, time.Time, error) // ErrRingClipNotFound
func (r *RingClipRecorder) RecordedAt(entrypointID string) time.Time              // zero when none

func (c *RingClip) VideoRTP(*rtp.Packet)
func (c *RingClip) Close()
```

- **One clip at a time.** `Start` closes the clip still open, as `SnapshotManager.Arm` does: the
  hardware carries one stream. A nil recorder or an invalid entrypoint id returns a nil clip, and
  every method on a nil clip is a no-op.
- **Never blocks the fan-out.** `VideoRTP` clones the packet and queues it on a 256-packet channel;
  when the channel is full the packet is dropped. A goroutine per clip does the rest.
- **Depacketizing.** `rtph264.Decoder` turns packets into access units.
  `ErrMorePacketsNeeded` and `ErrNonStartingPacketAndNoPrevious` are skipped silently; any other
  error drops the current access unit.
- **Start of the clip.** Access units are discarded until an SPS and a PPS have been seen and an
  access unit with an IDR (NAL 5) arrives. That IDR is sample 0; the wall-clock time at that moment
  is the clip's **recorded time**.
- **Timing.** PTS is the RTP timestamp minus the first IDR's, as a signed 32-bit difference so a
  timestamp wrap stays correct, at timescale 90 000. A sample's duration is the gap to the next
  access unit. The last sample repeats the previous duration, or 3 000 (1/30 s) when the clip has
  one sample. An access unit whose PTS is not after the previous one is dropped.
- **End of the clip**, whichever comes first:
  - an access unit at PTS ≥ 15 s: it is not written, and the clip is finalized;
  - `Close()`: answer, end of ring, stream failure, a new `Start`;
  - 8 MB of samples in memory: the clip is finalized with what it has;
  - an SPS different from the first: the clip is finalized with what it has.
- **Finalizing.** A clip with less than 1 s of video is discarded and the previous file stays.
  Otherwise a `pmp4.Presentation` with one H.264 track (SPS and PPS from the first IDR, AVCC
  samples served through `Sample.GetPayload`, non-IDR samples marked non-sync) is marshalled into
  a `bytes.Buffer` and written to `<dir>/clips/ring/<id>.mp4` with a temporary file and a rename, like
  `writeSnapshot`. The file's modification time is then set to the recorded time (`os.Chtimes`),
  so `RecordedAt` is the start of the clip and survives a restart. `onSaved` is called afterwards.
- **Paths.** The entrypoint id follows the same rule as `SnapshotManager.path`. Temporary files
  are `.ringclip-*.mp4` in the same directory; `NewRingClipRecorder` removes any left over by a
  crash.
- `Close` is idempotent and returns at once; finalizing runs on the clip's goroutine.

### 3.2 Ring holder (`internal/app/ringholder.go`)

The holder gets a `ringClipRecorder` interface (`Start(string) ringClip`) next to the coordinator.

- `acquire` calls `clips.Start(entrypoint.ID)` first and acquires with
  `SourceEvents{VideoRTP: clip.VideoRTP, RemoteBYE: clip.Close, Failed: func(error) { clip.Close() }}`.
- The clip is stored next to the lease, under the same generation.
- `release()` (answer, decline, hangup, end of the incoming call) closes the clip as well as
  leaving the lease. The answer therefore stops the recording, which is the decision in section 1.
- An acquire that fails closes the clip, which holds no video and is discarded. An acquire that
  completes after its ring is gone leaves the lease and closes the clip.
- `hold()` closes the previous clip together with the previous lease.
- After 15 s the clip stops consuming but **the holder keeps the stream**, so a card opened later
  can still join it.

### 3.3 API and state (`internal/api`, `internal/app/run.go`)

- `GET /api/v3/entrypoints/{id}/clip/ring.mp4`: same auth as `ring.jpg`; `http.ServeContent` with
  `Content-Type: video/mp4`; 404 `not_found` when there is no clip.
- `EntrypointDTO` gains `ring_clip_recorded_at` (RFC 3339, omitted when there is no clip), read
  from `RecordedAt`.
- `run.go` builds the recorder on `system.CompanionDataDir`, wires `SetOnSaved(server.BroadcastState)`
  and hands it to the ring holder and to the API server (`SetRingClips`, following the existing
  `Set*` injection).

## 4. Design — Home Assistant integration

### 4.1 Model and client

- `Entrypoint.ring_clip_recorded_at: datetime | None`.
- `API_PATH_CLIP_RING` and `CompanionApiClient.async_entrypoint_clip_ring(entrypoint_id) -> bytes`.
  The file is at most 8 MB.

### 4.2 `RingClipArchiver` (`clip_archive.py`, new)

One per config entry, owned by `IntegrationRuntime.clip_archive`, started with the entry and
stopped on unload.

- **Folder:** `<media>/bticino_go/<slug(device_id)>/<slug(entrypoint_id)>/`, where `<media>` is
  `hass.config.media_dirs["local"]`. When no `local` media dir is configured the archiver logs one
  warning and stays off; the rest of the integration is unaffected.
- **File name:** `YYYY-MM-DD_HH-MM-SS.mp4` from `ring_clip_recorded_at` in local time. An
  identical name in the repeated hour of a DST change is treated as already archived; accepted as
  negligible.
- **Trigger:** a coordinator listener. On every state it compares, per entrypoint,
  `ring_clip_recorded_at` with the expected file. When the file is missing and no download for it
  is in flight, it downloads the clip.
- **Idempotent by name**, so no state is persisted. After a Home Assistant restart the clip still
  on the companion is archived only if it was missing.
- **Writing:** in the executor, to a temporary file in the target folder, then `os.replace`, then
  `os.utime` to the recorded time.
- **Poster:** when `ring_snapshot_updated_at` is within 60 s of `ring_clip_recorded_at` in either
  direction (the normal case, since both come from the same stream), `ring.jpg` is saved next to
  the clip as `<same name>.jpg`. A poster that fails to download is skipped; the clip is kept.
- **Failure:** a failed clip download logs a warning; the next state push retries, because the
  file is still missing. A newer clip arriving before the old one was fetched replaces it on the
  companion and the old one is lost; accepted, given one clip on the intercom.
- **After a save:**
  - fires `bticino_companion_ring_clip_saved` with `entrypoint_id`, `recorded_at` (ISO),
    `path`, `poster_path` (or `null`) and `media_content_id`
    (`media-source://media_source/local/bticino_go/…`);
  - records the entrypoint's latest archived time and calls `coordinator.async_update_listeners()`,
    so the camera attribute in 4.4 updates.
- At start it scans its folders for the newest clip per entrypoint, so the attribute is right
  before the first new ring.
- The clips appear in **Media → My media** with no custom media source.

### 4.3 Retention and options flow

- The integration's first **options flow**: one field, `clip_retention_days`, integer 0–3650,
  default 30, labelled "Ring clip retention (days)"; `0` keeps clips forever. Saving reloads the
  entry.
- A purge runs at start, every 24 h and after every save. In the executor it deletes `*.mp4` and
  `*.jpg` under `bticino_go/<device_id>/` whose modification time is older than the retention,
  then removes empty entrypoint folders. It touches nothing outside that folder.

### 4.4 Card data

- **Camera attribute** `bticino_last_ring_clip_at`: the entrypoint's latest archived clip time,
  ISO, or absent. The card reloads its list when it changes. `subscribeEvents` is not used,
  because non-admin users cannot subscribe to custom events.
- **WebSocket command** `bticino_companion/card_ring_clips`
  `{camera_entity_id, limit}` (limit 1–50, default 10) →
  `{clips: [{recorded_at, url, poster_url}]}`, newest first. `url` and `poster_url` are
  `/media/local/bticino_go/…` paths signed with `async_sign_path` for 1 h; `poster_url` is `null`
  when there is no poster. The folder listing runs in the executor. Errors use the existing
  `_camera_for_message` handling, with the code `ring_clips_failed`.

### 4.5 README

- What the clip contains and what it does not (no audio, starts 2–3 s after the ring).
- Where the clips are stored and how retention works.
- An example automation on `bticino_companion_ring_clip_saved` that sends a notification with the
  ring picture (`/api/image_proxy/<image entity>`) and a link to the Media panel.

## 5. Design — card

### 5.1 Structure

Today `_render()` rewrites the whole shadow root on every `hass` update, several times a second.
A video playing inside it, or a carousel the user is scrolling, would be destroyed each time. The
card is split into a skeleton built once and parts rendered separately:

```
<style>…</style>
<ha-card>
  <div class="top"></div>          ← title and status: _render()
  <div class="viewer">
    <div class="live"></div>       ← snapshot, live <video>, ring overlay: _render()
    <div class="playback"></div>   ← clip player: _renderPlayback()
  </div>
  <div class="bottom"></div>       ← controls and error: _render()
  <section class="clips"></section> ← carousel: _renderClips()
</ha-card>
```

`_render()` keeps its current logic and markup, but writes into `.top`, `.live` and `.bottom`.
`_attachVideo()` is unchanged. `.playback` and `.clips` change only when the list or the playback
state changes. The card's behaviour around calls is otherwise untouched.

### 5.2 Configuration

| Option | Type | Default | Meaning |
|---|---|---|---|
| `clips` | integer 0–50 | 10 | Clips shown in the carousel; `0` removes the carousel and never calls the command |

### 5.3 Carousel

- **Header:** "Recent rings" on the left, the number of clips on the right.
- **Strip:** horizontal scroll with `scroll-snap-type: x mandatory`, newest first, scrollbar
  hidden but still scrollable by touch, wheel and keyboard. The right edge fades out with a
  `mask-image` gradient while more clips are off screen; the fade is removed when scrolled to the
  end.
- **Thumbnail:** a `<button>`, 4:3, 112 px wide on screens ≥ 400 px and 96 px below. Inside it:
  - the poster (`loading="lazy"`, `object-fit: cover`), or the `mdi:video` icon on a neutral
    background when there is none;
  - a centred play badge;
  - the label underneath: day on the first line, time on the second.
- **Labels:** "Today" and "Yesterday" come from `Intl.RelativeTimeFormat(lang, {numeric: "auto"})`
  with the first letter capitalized; older dates are `d MMM`. Times use `hass.locale.language` and
  the user's 12/24 h setting. Labels are therefore localized with no translation strings.
- **States:**
  - loading the first time: three placeholder tiles with a shimmer;
  - empty list or failed command: the whole section is hidden, and a failure is only logged with
    `console.debug`, so call errors never mix with clip errors;
  - the clip being played: its thumbnail gets a 2 px `--primary-color` ring and a "Playing" label.
- **Accessibility:** each thumbnail has `aria-label="Play ring clip, <day> <time>"`, a visible
  `:focus-visible` ring, and the strip is `role="list"`.

### 5.4 Playback

- A tap on a thumbnail fills `.playback` with `<video controls autoplay playsinline muted>` over
  the live layer. `muted` is required for autoplay and loses nothing, because the clip has no
  audio. The video uses `object-fit: contain` on the viewer's dark background, so the clip is never
  cropped.
- A top bar over a gradient shows the clip's day and time on the left and a 36 px round ✕ on the
  right. `Escape` closes the player too.
- When the list is older than 50 min, the card reloads it before playing so the signed URL is
  still valid.
- **End of the clip:** the video stays on its last frame with a centred replay button; ✕ returns
  to the snapshot.
- **A clip that fails to load** (an `error` event, for example a file just purged) shows
  "Clip unavailable" in the player and reloads the list.
- **The doorbell comes first.** A ring, a live view or an answer from this card closes the player
  at once. Tapping the call button while a clip plays starts the live view and closes the player.
  Playback reads Home Assistant files only and never touches the intercom.

### 5.5 Style

All colours come from Home Assistant theme variables, so the card follows light and dark themes
and custom themes with no media queries of its own.

| Element | Style |
|---|---|
| Section | `padding: 4px 14px 14px`; top border `1px solid var(--divider-color)` |
| Header | `font-size: 13px; font-weight: 600; color: var(--secondary-text-color)`; count at 12px, weight 400 |
| Strip | `display: flex; gap: 10px; padding: 8px 2px 2px; overflow-x: auto; scroll-padding: 0 2px` |
| Thumbnail | `border-radius: calc(var(--ha-card-border-radius, 12px) * .75); overflow: hidden; background: var(--secondary-background-color)`; `box-shadow: 0 1px 2px rgba(0,0,0,.15)` |
| Hover, desktop only | `@media (hover: hover)`: `transform: translateY(-2px)` and a stronger shadow, 150 ms |
| Play badge | 30 px circle, `background: rgba(0,0,0,.45)`, `backdrop-filter: blur(4px)`, white `mdi:play` at 18 px |
| Playing | `outline: 2px solid var(--primary-color); outline-offset: 2px`; badge replaced by `mdi:equalizer` |
| Label | Day at `12px/1.2 var(--primary-text-color)`, weight 500; time at `11px var(--secondary-text-color)`; both centred, ellipsized |
| Shimmer | `linear-gradient` across `--secondary-background-color` and `--divider-color`, 1.2 s |
| Player bar | `background: linear-gradient(rgba(0,0,0,.6), transparent)`; white text, 13px, weight 500 |
| Player ✕ and replay | Round buttons like the call controls, 36 px and 56 px, `background: rgba(0,0,0,.5)`, white icon |
| "Clip unavailable" | Centred, `color: #ddd`, 14px, with `mdi:video-off` above |

`@media (prefers-reduced-motion: reduce)` turns off the shimmer, the hover lift and the smooth
scrolling. The existing title bar, viewer and controls keep their current style.

## 6. Error handling

| Case | Behaviour |
|---|---|
| The ring holder cannot open the stream | No clip, no ring picture; a warning, as today |
| No IDR, or under 1 s of video | Clip discarded; the previous file stays |
| Answer, end of ring or stream failure before 15 s | The partial clip is kept |
| Over 8 MB, or the SPS changes | The clip is finalized with what it has |
| Companion disk write fails | Warning; the previous file stays |
| Home Assistant offline at the ring | Archived on reconnection, unless a newer clip replaced it |
| No `local` media dir | Archiver off, one warning; the carousel stays hidden |
| Clip purged while the card shows it | "Clip unavailable", and the list is reloaded |

## 7. Testing

- **`internal/media`** — `ringclip_test.go`:
  - start at the first IDR after SPS and PPS; packets before it dropped;
  - stop at 15 s, with the access unit at 15 s left out;
  - under 1 s discarded and the previous file kept;
  - 8 MB cap; SPS change;
  - timestamp wrap; FU-A and STAP-A input;
  - atomic write and the modification time equal to the recorded time;
  - `Close` while idle, twice, and on a nil clip;
  - the output read back with `pmp4.Presentation.Unmarshal`.

  These do not build on the Windows host: check with `GOOS=linux go build ./...` and leave
  execution to CI or the device.
- **`internal/app`** — the ring holder: the clip gets the packets, and is closed on answer, on
  end of ring, on a failed acquire, on a late acquire and on a new ring.
- **`internal/api`** — the `ring.mp4` route (200, 404, auth) and `ring_clip_recorded_at`.
- **Integration** (pytest):
  - the archiver: download on a new time, no second download, retry after a failure, poster
    within and outside 60 s, the event payload, the scan at start;
  - the purge: only old files, only inside its folder, `0` keeps everything;
  - the options flow;
  - `card_ring_clips`: order, limit, signed URLs, `null` poster, unknown camera;
  - the camera attribute.
- **Manual, on the intercom:**
  - a ring nobody answers gives a clip of about 15 s in Media, with a poster;
  - a ring answered after 5 s gives a partial clip;
  - the carousel updates without reloading the page;
  - the player survives state updates while a clip plays; ✕, `Escape`, replay;
  - a ring during playback closes the player and shows the preview;
  - `clips: 0`;
  - light and dark theme, and a phone-width dashboard.
