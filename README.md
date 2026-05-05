# spotify

A small Go library and `spot` CLI that play Spotify tracks on Sonos (or any
other Spotify Connect device) via the Spotify Web API. Designed for an agent
harness running on a headless Linux VM.

Three ways to use it:

- **Library** — `import "github.com/jefflaplante/spot"` and call
  `spot.Play(ctx, query, room)` from Go code.
- **CLI** — `spot play "bonobo migration" "Living Room"` from a shell or
  subprocess.
- **Claude Code skill** — natural-language control from inside Claude
  Code (e.g. "play Bonobo in the parlor"). See [`skills/`](skills/).

Sonos appears to Spotify as a Connect device once you've linked your Spotify
account in the Sonos app, so this driver doesn't stream audio itself — it
just tells Spotify which device to play on.

## Prerequisites

- **Spotify Premium account.** Player endpoints reject Free accounts.
- **Sonos linked to Spotify** in the Sonos app (Settings → Services & Voice
  → Add a Service → Spotify).
- **Go 1.22+** on the Linux host.
- A browser somewhere — even on a different machine — to complete the
  one-time OAuth consent. After that, the VM can run fully headless.

## Setup

### 1. Create a Spotify app

1. Sign in at <https://developer.spotify.com/dashboard> and click
   **Create app**.
2. Name and description: anything.
3. **Redirect URI:** `http://127.0.0.1:8888/callback` — exactly this
   string. Spotify deprecated `localhost` and bare `http://` URLs in 2025;
   `127.0.0.1` over `http` is still allowed for development.
4. APIs to use: **Web API**.
5. Save and note the **Client ID** and **Client secret**.

### 2. Build and install

```bash
git clone https://github.com/jefflaplante/spot.git
cd spot
make install        # or: make build  →  ./spot
```

`make install` runs `go install ./cmd/spot`, placing the `spot` binary in
`$(go env GOBIN)` (typically `~/go/bin`). Add that to `PATH` if you haven't
already. `make build` produces `./spot` in the repo root instead.

### 3. One-time OAuth

The agent needs a refresh token, which requires an interactive consent
screen exactly once. After that, the token cache (`.spotify-cache`)
persists indefinitely.

Set the credentials and run `auth`:

```bash
export SPOTIFY_ID=<your client id>
export SPOTIFY_SECRET=<your client secret>
spot auth
```

The command prints a Spotify authorization URL and starts a tiny HTTP
listener on `127.0.0.1:8888` to catch the redirect.

#### If the Linux host has no browser (typical for a VM)

Open an SSH session with a port forward, then run `auth` inside it:

```bash
ssh -L 8888:127.0.0.1:8888 user@vm
# inside the SSH session:
export SPOTIFY_ID=...
export SPOTIFY_SECRET=...
spot auth
```

Copy the printed URL into a browser **on your laptop**. The browser hits
`127.0.0.1:8888`, which the SSH tunnel forwards to the listener on the VM.
You'll see "Authed. You can close this tab." and `spot` will print
`authed as: <your name>`.

A `.spotify-cache` file (mode `0600`) is written in the working directory.
Treat it like a credential — anyone with this file plus your client
secret can control your Spotify playback.

### 4. Verify Sonos is reachable

Open the Sonos or Spotify app and start any track on a Sonos zone — this
"wakes" the speaker as a Spotify Connect device. Then:

```bash
spot play "test" "Living Room"
```

Replace `Living Room` with any case-insensitive substring of one of your
zone names.

## Commands

The CLI exits `0` on success, `1` on error, and respects `SIGINT`/`SIGTERM`
for clean shutdown when killed by a harness. Every command targeting a
Sonos zone matches the zone name as a case-insensitive substring, so
`Parlor`, `parlor`, and `parl` all match a zone named "Parlor".

### Authentication

#### `spot auth`

One-time OAuth bootstrap. Reads `SPOTIFY_ID` and `SPOTIFY_SECRET` from the
environment, opens an HTTP listener on `127.0.0.1:8888`, prints the Spotify
authorization URL, and writes the refresh token to `.spotify-cache` (or
`SPOTIFY_TOKEN_PATH`) once you complete the consent screen.

```bash
spot auth
```

After this runs once, the harness can use every other command without any
further interaction. See [§ 3. One-time OAuth](#3-one-time-oauth) for the
SSH port-forward dance if your VM has no browser.

### Discovery and introspection

#### `spot devices`

List the playback devices currently visible to Spotify (i.e. Spotify
Connect endpoints registered to your account). Includes Echo, modern
Sonos with current firmware, phones running the Spotify app, etc.

```bash
spot devices
# NAME    TYPE        ACTIVE  RESTRICTED  ID
# iPhone  Smartphone  false   false       e9c38024…
# Office  Speaker     true    false       b6acd95b-…_amzn_1
```

#### `spot zones`

List Sonos zones discovered on the LAN via SSDP, with the IP of each
zone's coordinator.

```bash
spot zones
# ZONE         COORDINATOR
# Parlor       192.168.0.42
# Living Room  192.168.0.43
```

Set `SONOS_HOST=<ip>` to skip SSDP if multicast is blocked on your network.

#### `spot diag <zone>`

Print the Spotify SMAPI service registration that the Sonos zone reports
(sid, type, sn). Useful for debugging "UPnP error 800" if Sonos refuses a
playback request.

```bash
spot diag Parlor
# zone:        Parlor
# coordinator: 192.168.0.42
# sid:         12
# type:        57863
# sn:          1  (override with SONOS_SN env var)
```

#### `spot probe <zone>`

Dump the raw `TrackURI` and `TrackMetaData` (DIDL-Lite) of whatever the
zone is currently playing. Useful for capturing the URI/metadata format
Sonos itself uses, e.g. when adding support for a new content type.

```bash
spot probe Parlor
```

#### `spot now <zone>`

Show what's playing on a Sonos zone — title, artist, album, position, and
state (`playing`, `paused`, `stopped`, `transitioning`). For Spotify
tracks, missing fields are looked up via the Spotify Web API since Sonos
strips DIDL metadata for SMAPI items.

```bash
spot now Parlor
# Parlor (playing)
#   Subtronics — Itchy Scratchy
#   album: Itchy Scratchy
#   01:23 / 03:13
```

### Playback

#### `spot play <query> <zone> [-v connect|sonos] [-c]`

Search Spotify for `<query>` and start playback on `<zone>`.

`<query>` may be free-text (`"bonobo migration"`), Spotify search syntax
(`'artist:Subtronics track:"Itchy Scratchy"'`), or a Spotify URI
(`"spotify:track:4cOdK2wGLETKBW3PvgPWqT"`). Top hit wins.

| Flag           | Default   | Meaning                                                                                          |
|----------------|-----------|--------------------------------------------------------------------------------------------------|
| `-v`, `--via`  | `connect` | Playback transport. `connect` uses the Spotify Web API; `sonos` uses UPnP directly to the zone.  |
| `-c`, `--continue` | off   | After the seed track, queue the artist's top tracks (~10 more) so the music keeps playing.       |

```bash
spot play "bonobo migration" "Living Room"               # via Spotify Connect
spot play -v sonos "subtronics drums" Parlor             # via Sonos UPnP
spot play -c -v sonos "armin van buuren" Parlor          # seed + 10 more, on Sonos
spot play "spotify:track:4cOdK2wGLETKBW3PvgPWqT" Kitchen # exact URI
```

`connect` works for any device that appears in `spot devices`. `sonos`
works for any Sonos zone, including older speakers (Play:1, Play:3, One
SL) that don't register as Spotify Connect devices — see
[Gotchas](#gotchas).

#### `spot pause <zone>`

Pause playback on a Sonos zone.

#### `spot resume <zone>`

Resume playback from the current position.

#### `spot stop <zone>`

Stop playback. Unlike pause, this resets position to the start of the
current track.

#### `spot next <zone>` (alias: `spot skip`)

Advance to the next track in the zone's queue. Only meaningful when a
queue is loaded — e.g. after `spot play -c …` or playback started from
the Sonos app. Single-track playback (without `-c`) has nothing to
advance to.

```bash
spot next Parlor
spot skip Parlor   # same thing
```

### Queue

#### `spot enqueue <query> <zone>`

Append a track to a Sonos zone's playback queue. Resolves the query the
same way `play` does. Does not change transport state — if the zone
isn't already playing from its queue, start it with `spot play -v sonos
-c …` first, then `enqueue` adds to the same queue.

```bash
spot enqueue "subtronics phaze 2" Parlor
# Parlor: queued at position 11 (queue now has 11 tracks)
```

#### `spot queue <zone> [--raw]`

Show the playback queue and the current position.

```bash
spot queue Parlor
# Parlor — 10 tracks, playing 3
#     1  Armin van Buuren — Blah Blah Blah
#     2  Armin van Buuren — In and Out of Love
#  >  3  Armin van Buuren — This Is What It Feels Like
#     …
```

`--raw` prints the unparsed DIDL-Lite XML returned by Sonos's
`Browse Q:0` — handy for debugging metadata issues.

### Volume

#### `spot vol <zone> [up|down|<level>] [step]`

Show or change the volume of a Sonos zone (`0`–`100`). With no extra args,
prints the current level. Any change prints the resulting clamped value.

```bash
spot vol Parlor          # show
spot vol Parlor up       # +5
spot vol Parlor up 10    # +10
spot vol Parlor down     # -5
spot vol Parlor down 10  # -10
spot vol Parlor 50       # absolute
```

`up`/`down` are used instead of `+5`/`-5` so the leading dash isn't
parsed as a flag.

## Library

```go
import (
    "context"

    "github.com/jefflaplante/spot"
)

func playSomething(ctx context.Context) error {
    return spot.Play(ctx, "bonobo migration", "Living Room")
}
```

The package lazy-initialises a singleton client on the first `Play` call,
so `Play` is safe to call from multiple goroutines without explicit setup.

## Configuration

All configuration is environment variables.

| Variable               | Required | Default                              | Notes                                                         |
|------------------------|----------|--------------------------------------|---------------------------------------------------------------|
| `SPOTIFY_ID`           | yes      |                                      | Client ID from the Spotify dashboard.                         |
| `SPOTIFY_SECRET`       | yes      |                                      | Client secret from the Spotify dashboard.                     |
| `SPOTIFY_REDIRECT_URI` | no       | `http://127.0.0.1:8888/callback`     | Must match the URI registered on the app exactly.             |
| `SPOTIFY_TOKEN_PATH`   | no       | `.spotify-cache` (in cwd)            | Where the OAuth refresh token is persisted. Mode `0600`.      |
| `SONOS_HOST`           | no       |                                      | Skip SSDP discovery; use this Sonos IP for topology lookup.   |
| `SONOS_SN`             | no       | `1`                                  | Spotify "service number" in Sonos. Try `2` or `3` if playback fails on the Sonos path. |

For an agent harness, set these in the unit file or whatever secret store
you use, and pin `SPOTIFY_TOKEN_PATH` to an absolute path so the harness
doesn't have to care about cwd.

### Example systemd unit

```ini
[Unit]
Description=Spotify agent harness
After=network-online.target

[Service]
Environment=SPOTIFY_ID=...
Environment=SPOTIFY_SECRET=...
Environment=SPOTIFY_TOKEN_PATH=/var/lib/spot/token.json
WorkingDirectory=/var/lib/spot
ExecStart=/home/agent/go/bin/spot play "lofi beats" "Office"
User=agent

[Install]
WantedBy=multi-user.target
```

For real harness use, the agent process will call `spot` (or the library)
on demand; the unit above just illustrates env wiring.

## Gotchas

- **Sonos drops out of the device list when idle.** If `spot play`
  returns `no device matching "<room>"; visible: []` on the default
  Connect path, the speaker isn't registered with Spotify Connect.
  Use `spot play -v sonos ...` instead — that path goes through Sonos's
  native Spotify integration over UPnP and works on every Sonos speaker
  with Spotify linked, regardless of Connect registration.
- **Older Sonos hardware (Play:1, Play:3, One SL, Connect, Connect:Amp)
  never registers as a Spotify Connect device** — their firmware doesn't
  speak Connect. Always use `-v sonos` for these.
- **SSDP requires multicast.** Some VM/container networks block UDP
  multicast, so SSDP discovery returns nothing. Set `SONOS_HOST=<ip>` to
  the IP of any Sonos speaker on your LAN to skip discovery.
- **Look up devices by name each call**, not by cached ID. Sonos device
  IDs rotate.
- **Premium-only.** Player endpoints return `403` for Free accounts.
- **Rate limits.** Spotify returns `429` with a `Retry-After` header; the
  underlying client doesn't auto-retry. Don't poll `/me/player` more than
  ~once per second.
- **Refresh-token rotation.** The library writes refreshed tokens back to
  `SPOTIFY_TOKEN_PATH` on every refresh, so harness restarts after long
  uptimes don't fail. Don't symlink or bind-mount that file read-only.

## Project layout

```
.
├── cmd/spot/main.go   # Cobra CLI entry point (binary: spot)
├── play.go            # Spotify Web API path: Authenticate, Devices, Play,
│                      #   PlayOption / WithContinue, track resolution and
│                      #   batched lookups via /v1/tracks
├── sonos.go           # Sonos UPnP/SOAP path: SSDP discovery, ListSonosZones,
│                      #   PlayViaSonos, EnqueueViaSonos, ShowQueue, NowPlaying,
│                      #   Pause / Resume / Stop / Next, Volume / SetVolume,
│                      #   plus diagnostic helpers (BrowseQueueRaw,
│                      #   CurrentTrackFor, SpotifyServiceFor)
├── Makefile           # build / install / vet / fmt / tidy / clean
├── skills/            # Claude Code skill that wraps the CLI
│   ├── README.md
│   └── spot/SKILL.md
├── go.mod / go.sum
└── README.md
```
