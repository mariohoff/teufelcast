# Task: "teufelcast" – stream my Linux desktop audio to a DLNA speaker

Build a small Go application that makes my Teufel Streamer (a UPnP/DLNA
renderer) usable as an audio output for my Fedora 44 desktop, controlled
through a minimal local web UI. Goal: I select "Teufel" as my output in GNOME
(or the app does it for me), play music in e.g. YouTube in the browser, and
hear it on the speaker.

Latency of a few seconds is acceptable – this is for music, not video.

## How it works (already verified manually)

```
Apps (browser etc.) → PipeWire null sink "teufel" → teufel.monitor
    → ffmpeg (capture + encode) → HTTP stream served by this app on :8090
Speaker ← SOAP SetAVTransportURI + Play ("play http://<pc-ip>:8090/live.mp3")
Speaker → HTTP GET /live.mp3 → pulls the endless stream
```

DLNA is a pull model: the app only tells the speaker which URL to play
(via SOAP), and the speaker then fetches it over HTTP.

These steps were tested by hand and work:

- Creating the virtual output:
  `pactl load-module module-null-sink sink_name=teufel sink_properties=device.description=Teufel`
  → "Teufel" shows up in GNOME sound settings.
- Capturing it: `parec -d teufel.monitor` and
  `pw-record -P stream.capture.sink=true --target teufel` both work.
  Note: PipeWire-native tools do NOT know the name `teufel.monitor`; the
  PulseAudio-compat layer (pactl, parec, `ffmpeg -f pulse`) does. Use
  `ffmpeg -f pulse -i teufel.monitor`.
- Live stream: `ffmpeg -f pulse -i teufel.monitor -c:a libmp3lame -b:a 320k -f mp3 ...`
  served over HTTP, then SetAVTransportURI + Play via SOAP.

## Existing code to reuse

`./main.go` in this directory is a working stdlib-only Go sketch that plays a
single audio file on the speaker. Reuse its pieces:

- SSDP discovery (M-SEARCH for `urn:schemas-upnp-org:service:AVTransport:1`)
- Parsing the device description XML (nested devices, URLBase, resolving
  control URLs) for AVTransport and ConnectionManager
- SOAP helper, `GetProtocolInfo`, DIDL-Lite metadata, DLNA headers
- `localIPFor(host)` to find the PC's IP as seen from the speaker
- Renderer selection by name (exact match wins over substring match)

## Facts about my setup

- The speaker exposes TWO renderers on the same IP, on different ports:
  - `Coolzimmer` → `http://192.168.178.183:57785/TransportService/Control`
    (**use this one** – direct renderer, starts playing instantly)
  - `Streamer Coolzimmer` → `http://192.168.178.183:60596/AVTransport/ctrl`
    (Raumfeld multiroom "zone" renderer, noticeably more buffering/delay)
- The ports are dynamic and may change after the speaker reboots → always
  discover, never hardcode URLs. Default device name: `Coolzimmer`.
- Accepted MIME types (from `GetProtocolInfo` Sink on "Coolzimmer"):
  audio/mpeg, audio/mp3, audio/flac, audio/x-flac, audio/wav, audio/x-wav,
  audio/ogg, application/ogg, audio/vorbis, audio/mp4, audio/m4a, audio/alac,
  audio/x-ac3, audio/x-mpegurl, …
  No `audio/L16` → raw LPCM is not an option.
- Fedora 44, PipeWire with pipewire-pulse, GNOME. Go 1.24+. ffmpeg and pactl
  available.

## Requirements

### Backend (Go, keep dependencies minimal – stdlib preferred)

1. **Startup checks:** verify `ffmpeg` and `pactl` exist in PATH; verify
   ffmpeg has the `libmp3lame` encoder (`ffmpeg -hide_banner -encoders`).
   Print a clear error with a fix hint if not (Fedora: `ffmpeg-free`
   normally includes lame; else RPM Fusion ffmpeg).

2. **Discovery:** discover renderers on startup and on demand (refresh
   button). Cache results. Also look up the RenderingControl service URL for
   volume.

3. **Virtual sink management** (via `pactl`):
   - On start: if a sink named `teufel` already exists (e.g. left over from a
     crash), reuse it; otherwise `pactl load-module module-null-sink ...` and
     remember the returned module ID.
   - Optional (UI checkbox, default on): remember the current default sink
     (`pactl get-default-sink`), then `pactl set-default-sink teufel`. Restore
     the previous default on stop.
   - On stop/exit: restore the default sink, unload the module only if we
     created it.

4. **Stream server** on `0.0.0.0:8090` (must be reachable from the speaker;
   the web UI is separate, see below):
   - Path `/live.<ext>` depending on format.
   - For each GET request: start ffmpeg with `exec.CommandContext(r.Context())`
     reading `-f pulse -i teufel.monitor`, writing to stdout, and copy stdout
     into the response, flushing continuously (`http.Flusher`). When the
     speaker disconnects, the context cancels and ffmpeg dies. Speakers often
     open several connections (probe + real), so per-request ffmpeg is fine
     and simpler than fan-out.
   - HEAD requests: headers only.
   - Headers: correct `Content-Type`, **no** `Content-Length`,
     `transferMode.dlna.org: Streaming`, `contentFeatures.dlna.org` with
     `DLNA.ORG_OP=00` (no seeking on a live stream).
   - No write timeouts on this server.
   - ffmpeg low-latency flags: `-fflags nobuffer -flush_packets 1`,
     fixed `-ac 2 -ar 44100`.
   - Formats (selectable in UI, default MP3):
     - `mp3`: `-c:a libmp3lame -b:a 320k -f mp3`, `audio/mpeg`
     - `flac`: `-c:a flac -f flac`, `audio/flac` (lossless, untested live)
     - `wav`: `-c:a pcm_s16le -f wav`, `audio/wav` (lossless, untested live;
       the WAV header has unknown length on a pipe – may or may not work)
   - Track stats for the UI: active connections, bytes sent, stream uptime,
     remote address of the speaker.

5. **UPnP control:**
   - Start: `Stop` (ignore errors), then `SetAVTransportURI` with DIDL-Lite
     metadata (class `object.item.audioItem.audioBroadcast`, title e.g.
     "Desktop audio", no duration), then `Play`. If SetAVTransportURI fails,
     retry once with empty metadata.
   - Stop: send `Stop` to the speaker, then tear down.
   - Volume: `GetVolume` / `SetVolume` on RenderingControl
     (`urn:schemas-upnp-org:service:RenderingControl:1`, `Channel=Master`).

6. **Watchdog / reconnect** (toggle in UI, default on):
   - While streaming, poll `GetTransportInfo` every ~5 s.
   - If the speaker is STOPPED/NO_MEDIA_PRESENT and no stream connection is
     active, re-send SetAVTransportURI + Play. Back off; give up after a few
     attempts and show an error.
   - If `GetMediaInfo` shows a different CurrentURI than ours, someone took
     over the speaker (e.g. the Teufel app) → do NOT fight it; go to idle
     and show "speaker taken over" in the UI.

7. **Lifecycle:** on SIGINT/SIGTERM: stop the speaker, kill ffmpeg, restore
   the default sink, unload the module. Log state changes clearly.

8. **Flags:** `-ui-addr` (default `127.0.0.1:8091`), `-stream-port`
   (default `8090`), `-device` (default `Coolzimmer`), `-format`
   (default `mp3`).

### Web UI (minimal)

- Single static HTML page with vanilla JS, embedded via `embed`, served on
  `127.0.0.1:8091`. No frameworks, no build step. Plain, clean look is
  enough.
- Elements:
  - Device dropdown (from discovery) + "Refresh" button
  - Format dropdown (mp3 / flac / wav)
  - Checkboxes: "Switch system output to Teufel", "Auto-reconnect"
  - Big Start / Stop button
  - Volume slider (speaker's own volume via RenderingControl)
  - Status: state (idle / starting / streaming / reconnecting / error),
    speaker connected yes/no, bytes sent, uptime, last error
  - Small log area with the last ~20 events
- JSON API:
  - `GET  /api/devices[?refresh=1]`
  - `GET  /api/status` (UI polls every 1 s)
  - `POST /api/start` `{device, format, switchDefault, autoReconnect}`
  - `POST /api/stop`
  - `GET/POST /api/volume` `{volume: 0-100}`

### Project layout (suggestion)

Single `main` package, split into files: `discovery.go`, `upnp.go`
(SOAP, AVTransport, RenderingControl, ConnectionManager), `sink.go`
(pactl), `stream.go` (ffmpeg + HTTP handler), `api.go` (web API),
`web/index.html` (embedded), `main.go`. Keep the old file-playing sketch
working, or move it to a `-file` mode, so I can still test single files.

### Notes / things to watch

- Firewall: Fedora firewalld must allow inbound TCP 8090 from the LAN, or the
  speaker can't fetch the stream. Mention this in the README
  (`sudo firewall-cmd --add-port=8090/tcp --permanent && sudo firewall-cmd --reload`).
- While nothing plays, the monitor delivers silence, so the stream keeps
  flowing – good, the speaker should not drop the connection.
- Log every HTTP request the speaker makes (method, path, Range, User-Agent):
  this is the main debugging aid.
- Not yet verified, please help me check once it runs: behaviour during long
  pauses, stability over 30+ minutes, whether FLAC/WAV live streams work.

### Deliverables

- The working app, `go vet` clean
- A short README: requirements, firewall note, how to run, how to test
- Optional: a systemd user unit example so it can run in the background
