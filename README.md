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

Every command also accepts a global `--json` flag that emits a
machine-readable JSON document instead of the human-readable table or
single-line summary. Useful for piping into `jq` or wiring into a
harness.

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

### Personalization (read-only)

These commands read your Spotify account and don't mutate anything.
All of them honour the global `--json` flag for machine-readable output.

#### `spot top tracks [short|medium|long] [--limit N]`

List your top tracks for the given Spotify time range (default `medium`,
~6 months). `short` ≈ last 4 weeks, `long` ≈ several years.

| Flag       | Default | Meaning                                       |
|------------|---------|-----------------------------------------------|
| `--limit`  | `20`    | Number of tracks to return (Spotify max 50).  |

```bash
spot top tracks
spot top tracks short --limit 10
# RANK  ARTIST       TITLE              POP
# 1     Subtronics   Itchy Scratchy     63
# …
```

#### `spot top artists [short|medium|long] [--limit N]`

Same as above, but artists. Output includes Spotify's genre tags, which
`mix`/`rabbithole`/`fresh` lean on.

| Flag       | Default | Meaning                                        |
|------------|---------|------------------------------------------------|
| `--limit`  | `20`    | Number of artists to return (Spotify max 50).  |

```bash
spot top artists long --limit 5
```

#### `spot recent [--since DURATION] [--limit N]`

List recently-played tracks. Spotify only remembers the **last fifty**
plays — there is no way to fetch more from the Web API. `--since`
filters that list to plays within the given Go duration (e.g. `24h`,
`168h`).

| Flag      | Default | Meaning                                                            |
|-----------|---------|--------------------------------------------------------------------|
| `--since` |         | Drop plays older than this duration (`24h`, `168h`, `30m`).        |
| `--limit` | `50`    | Max items to fetch from Spotify before `--since` filtering (≤ 50). |

```bash
spot recent --since 24h
# WHEN                  ARTIST       TITLE
# 2026-05-05T08:14:02Z  Subtronics   Itchy Scratchy
# …
```

#### `spot liked [--limit N]`

List your saved (liked) tracks. Paginates transparently — `--limit`
above 50 fetches as many pages as needed.

```bash
spot liked --limit 200
```

#### `spot follows`

List every artist the current user follows. Walks all pages.

```bash
spot follows
# NAME         POP  GENRES
# Bonobo       72   downtempo, electronica, trip hop
# …
```

#### `spot playlists`

List playlists you own or follow. Walks all pages.

```bash
spot playlists
# NAME            OWNER  TRACKS  PUBLIC
# Mowing music    jeff   42      false
# …
```

### Library mutation

State-changing operations against your Spotify account. Each one also
records an event in the local memory layer (see [Memory](#memory)).

#### `spot like [<query>|--current <zone>]`

Save a track to your Liked Songs (`PUT /v1/me/tracks`). With a
`<query>` (free-text or `spotify:track:…` URI), the top hit is liked.
With `--current <zone>`, whatever's playing on the named Sonos zone
gets liked instead — handy for "save what I'm hearing right now".

| Flag         | Default | Meaning                                                          |
|--------------|---------|------------------------------------------------------------------|
| `--current`  |         | Like the track currently playing on this Sonos zone.             |

```bash
spot like "subtronics phaze 2"
spot like --current Parlor
```

#### `spot unlike [<query>|--current <zone>]`

Inverse of `like` — removes the resolved track from your Liked Songs
(`DELETE /v1/me/tracks`).

```bash
spot unlike --current Parlor
```

#### `spot follow <artist>`

Resolve `<artist>` (free-text or `spotify:artist:…` URI) and follow them
(`PUT /v1/me/following?type=artist`).

```bash
spot follow "bonobo"
```

#### `spot unfollow <artist>`

Inverse of `follow`.

```bash
spot unfollow "some artist I no longer care about"
```

#### `spot playlist new <name> [--description] [--public]`

Create a new playlist owned by the current user. Private by default.

| Flag            | Default | Meaning                                       |
|-----------------|---------|-----------------------------------------------|
| `--description` | `""`    | Free-text description.                        |
| `--public`      | `false` | Make the playlist public.                     |

```bash
spot playlist new "Mowing music"
spot playlist new "Friday late" --description "after 11pm only" --public
```

#### `spot playlist add <playlist> <query>`

Match `<playlist>` by case-insensitive substring against your playlists,
resolve `<query>` to a track, and add it. Errors if zero or more than one
playlist match.

```bash
spot playlist add "mowing" "bonobo migration"
```

#### `spot playlist remove <playlist> <query>`

Inverse of `playlist add`.

```bash
spot playlist remove "mowing" "that one track I don't like anymore"
```

### Memory

`spot` keeps a small SQLite database (`spot.db`) next to its OAuth
cache. Every state-mutating command logs an event there, and `now`/
`queue` calls produce `observe` events as a side effect — together
they form a queryable local history that the discovery commands also
read from. See [Privacy](#privacy) for what's stored and how to clear
it.

#### `spot feedback love|hate|skip-forever <query>|--current <zone> [--note]`

Record a verdict on a track. `love` boosts a track in `daily`; `hate`
and `skip-forever` exclude a track from `mix` and `daily`. The resolved
track is also cached so future `history` rows show artist/title.

| Flag        | Default | Meaning                                                    |
|-------------|---------|------------------------------------------------------------|
| `--current` | `false` | Apply the verdict to the zone's currently-playing track.   |
| `--zone`    |         | Sonos zone name (required with `--current`).               |
| `--note`    | `""`    | Free-text note attached to the feedback event.             |

```bash
spot feedback love "bonobo migration"
spot feedback skip-forever --current --zone Parlor --note "wife hates it"
```

#### `spot mark <note>`

Annotate the currently-playing track with a free-text note. With no
`--zone`, picks the first zone in the household reporting `PLAYING`; if
nothing's playing, errors out.

| Flag     | Default | Meaning                                                  |
|----------|---------|----------------------------------------------------------|
| `--zone` |         | Sonos zone (default: first zone reporting `PLAYING`).    |

```bash
spot mark "perfect for the drive home"
spot mark --zone Parlor "what is this synth"
```

#### `spot history [--zone --since --artist --kind --limit]`

Print rows from the events table joined to the cached track metadata.
Newest first.

| Flag       | Default | Meaning                                                               |
|------------|---------|-----------------------------------------------------------------------|
| `--zone`   |         | Filter by Sonos zone name.                                            |
| `--artist` |         | Substring match on cached artist name, or `spotify:artist:<id>`.      |
| `--kind`   |         | Filter by event kind (`play_started`, `observe`, `feedback`, `note`). |
| `--since`  | `24h`   | Go duration window relative to now.                                   |
| `--limit`  | `50`    | Max rows.                                                             |

```bash
spot history --since 168h --kind play_started
spot history --artist "subtronics"
```

#### `spot stats [--top N --recent --skipped]`

Aggregate the events table. Modes are mutually exclusive; default is
`--top 10`.

| Flag        | Default | Meaning                                                                     |
|-------------|---------|-----------------------------------------------------------------------------|
| `--top`     | `10`    | Top-N most-played tracks and artists from `play_started` events (all time). |
| `--recent`  | `false` | Same as `--top` but limited to the last 7 days.                             |
| `--skipped` | `false` | Tracks with the most `skip` events in the last 7 days.                      |

```bash
spot stats --top 10
spot stats --skipped
```

### Discovery and recommendations

Curated track lists built from your Spotify history, your followed
artists, and the local memory layer. Every command supports `--zone Y`
to queue the result on a Sonos zone (omit to just print) and
`--explain` to print the algorithmic reasoning before the list.

#### `spot mix [--length N] [--seed <q>] [--exclude-recent DAYS] [--zone Y] [--explain]`

Curated mix from your medium-term top tracks (or, with `--seed`, the
seed track plus its primary artist's top tracks), filtered against
local feedback (`hate`/`skip-forever` verdicts drop tracks) and recent
plays. Truncated to `--length`.

| Flag               | Default | Meaning                                                          |
|--------------------|---------|------------------------------------------------------------------|
| `--length`         | `20`    | Number of tracks in the mix.                                     |
| `--seed`           |         | Anchor mix to this track or artist (URI or query).               |
| `--exclude-recent` | `7`     | Drop tracks played in the last N days (`0` disables).            |
| `--zone`           |         | Queue the result on this Sonos zone via UPnP.                    |
| `--explain`        | `false` | Print the pool / dropped / kept counts before the track list.    |

```bash
spot mix --zone Parlor
spot mix --seed "bonobo" --length 30 --explain
```

#### `spot daily [--zone Y] [--length N] [--explain]`

Spotify-Daily replacement: long-term top tracks the user has not played
in the last 30 days, with local feedback applied. `love` verdicts boost
to the front; `hate`/`skip-forever` drop entirely.

| Flag        | Default | Meaning                                          |
|-------------|---------|--------------------------------------------------|
| `--length`  | `20`    | Max number of tracks to return.                  |
| `--zone`    |         | Queue the result on this Sonos zone.             |
| `--explain` | `false` | Print the algorithmic reasoning before the list. |

```bash
spot daily --zone Parlor
spot daily --explain --length 30
```

#### `spot fresh [--days 30] [--zone Y] [--all-tracks] [--explain]`

Recent releases from artists you follow. Walks each followed artist's
albums and lists those released within the last `--days`. By default
each fresh album contributes only its first track; `--all-tracks`
surfaces every track on each album.

| Flag           | Default | Meaning                                          |
|----------------|---------|--------------------------------------------------|
| `--days`       | `30`    | Lookback window in days (must be positive).      |
| `--zone`       |         | Queue the result on this Sonos zone.             |
| `--all-tracks` | `false` | Include every track per album, not just the first. |
| `--explain`    | `false` | Print the algorithmic reasoning before the list. |

```bash
spot fresh --days 60
spot fresh --zone Parlor --all-tracks
```

#### `spot deeper <artist> [--zone Y] [--per-album N] [--explain]`

Surface deep cuts from a single artist: walk their full-length albums
in chronological order (oldest first) and pick 1–2 tracks from each,
**skipping the artist's top 10 "greatest hits"** so you get the deeper
material instead. Singles, compilations, and appears-on records are
filtered out.

`<artist>` may be free-text (`"radiohead"`) or a Spotify artist URI.

| Flag          | Default | Meaning                                                       |
|---------------|---------|---------------------------------------------------------------|
| `--per-album` | `1`     | Tracks to surface per album (`1` or `2` keeps the queue tight). |
| `--zone`      |         | Queue the result on this Sonos zone.                          |
| `--explain`   | `false` | Print the catalog walk before the track list.                 |

```bash
spot deeper "radiohead"
spot deeper "spotify:artist:4Z8W4fKeB5YxbusRsdQVPb" --per-album 2 --zone Parlor
```

#### `spot rabbithole [--seed <q>] [--length N] [--zone Y] [--explain]`

Walk from a seed artist through genre overlap with your top artists.
Substitute for Spotify's deprecated related-artists endpoint: read the
seed's genre tags, score your medium- and long-term top artists by how
many of those genres they share, take the top eight, and pull a couple
of top tracks from each.

| Flag        | Default                          | Meaning                                                          |
|-------------|----------------------------------|------------------------------------------------------------------|
| `--seed`    | your top short-term artist       | Seed artist (free-text query or `spotify:artist:<id>`).          |
| `--length`  | `20`                             | Max number of tracks to return.                                  |
| `--zone`    |                                  | Queue the result on this Sonos zone.                             |
| `--explain` | `false`                          | Print the artist-by-artist walk before the track list.           |

```bash
spot rabbithole --seed "bonobo" --zone Parlor
spot rabbithole --explain
```

### Playback

#### `spot play <query> <zone> [-v connect|sonos] [-c] [-s]`

Search Spotify for `<query>` and start playback on `<zone>`.

`<query>` may be free-text (`"bonobo migration"`), Spotify search syntax
(`'artist:Subtronics track:"Itchy Scratchy"'`), or a Spotify URI
(`"spotify:track:4cOdK2wGLETKBW3PvgPWqT"`). Top hit wins.

| Flag           | Default   | Meaning                                                                                          |
|----------------|-----------|--------------------------------------------------------------------------------------------------|
| `-v`, `--via`  | `connect` | Playback transport. `connect` uses the Spotify Web API; `sonos` uses UPnP directly to the zone.  |
| `-c`, `--continue` | off   | After the seed track, queue the artist's top tracks (~10 more) so the music keeps playing.       |
| `-s`, `--shuffle`  | off   | With `-c`, broaden the candidate pool with a random sample of the artist's album cuts and shuffle. Repeated invocations produce different track sets. The seed track still plays first. |

```bash
spot play "bonobo migration" "Living Room"               # via Spotify Connect
spot play -v sonos "subtronics drums" Parlor             # via Sonos UPnP
spot play -c -v sonos "armin van buuren" Parlor          # seed + 10 more, on Sonos
spot play -c -s -v sonos "underworld" Parlor             # different tracks each time
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

#### `spot shuffle <zone> [off]`

Toggle the zone's Sonos shuffle mode. Default arg turns shuffle **on**
(`SHUFFLE_NOREPEAT`); pass `off` to restore sequential playback. The
queue's stored order isn't physically changed — Sonos just plays items
in random order while shuffle is on.

```bash
spot shuffle Parlor          # turn shuffle on
spot shuffle Parlor off      # turn it off
```

For *content-level* entropy (different tracks each time you ask for an
artist), use the `-s` flag on `spot play` — see that command.

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
| `SPOT_DB_PATH`         | no       | `spot.db` next to `SPOTIFY_TOKEN_PATH` | Where the local memory SQLite file lives. Mode `0600`. See [Privacy](#privacy). |
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

## Privacy

`spot` keeps a small SQLite database on disk so that `history`, `stats`,
and the discovery commands (`mix`, `daily`, …) have something to read.
This section covers exactly what it stores and how to get rid of it.

### What `spot.db` records

- **Mutating commands.** Every `play`, `enqueue`, `pause`, `resume`,
  `next`, `stop`, `like`, `unlike`, `follow`, `unfollow`, `playlist
  new`/`add`/`remove`, `feedback`, and `mark` writes a row to the
  `events` table with the kind, the resolved Spotify track or artist
  ID, the zone (when applicable), and a timestamp.
- **`observe` events.** `spot now` and `spot queue` emit an `observe`
  event each time they poll a zone. This is how `history` shows what
  Sonos was doing without requiring you to instrument the speakers.
- **Track-metadata cache.** A `tracks` table caches title, artist
  name, artist ID, album, and Spotify popularity for every track ID
  the CLI has touched. Entries refresh after 30 days. This lets
  `history` render artist/title for tracks you played weeks ago
  without re-hitting the Spotify API.
- **Feedback verdicts.** `love`, `hate`, and `skip-forever` are
  events; the discovery commands read them back to bias the output.
- **Free-text notes.** Anything you pass to `spot mark "<note>"` or
  `spot feedback --note "<note>"` lands verbatim in the event row's
  payload column.

### Where it lives

`SPOT_DB_PATH` if you set it; otherwise `spot.db` in the same
directory as `SPOTIFY_TOKEN_PATH` (which defaults to `.spotify-cache`
in the current working directory). The file is `chmod`'d to `0600` on
first open.

### What it doesn't do

- The database **never leaves the machine.** There is no telemetry, no
  analytics endpoint, no upload to Spotify or anywhere else.
- Nothing in `spot.db` is sent to Spotify. Library mutation calls
  (`like`, `follow`, etc.) hit Spotify's API directly; the local
  event log is purely a side-effect for your own consumption.

### How to clear it

```bash
rm "$(printenv SPOT_DB_PATH || dirname "$(printenv SPOTIFY_TOKEN_PATH || echo .spotify-cache)")/spot.db"
# or just:
rm spot.db
```

The next `spot` invocation re-creates an empty schema. Feedback
verdicts and the cached track metadata are gone too, so `mix` and
`daily` will start over without your "hate" / "love" history.

The cached `tracks` table holds artist names + album names. It's
useful for offline `history` lookups but worth knowing — anyone with
read access to `spot.db` can see every artist and album the CLI has
ever resolved on this machine.

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
├── cmd/spot/main.go      # Cobra CLI entry point (binary: spot)
├── cmd/spot/output.go    # --json envelope helper shared by every subcommand
├── play.go               # Spotify Web API path: Authenticate, Devices, Play,
│                         #   PlayOption / WithContinue, resolveTrack and
│                         #   batched lookups via /v1/tracks
├── sonos.go              # Sonos UPnP/SOAP path: SSDP discovery, ListSonosZones,
│                         #   PlayViaSonos, EnqueueViaSonos, ShowQueue,
│                         #   NowPlaying, Pause / Resume / Stop / Next,
│                         #   Volume / SetVolume, plus diagnostic helpers
├── personalization.go    # top / recent / liked / follows / playlists (read)
├── library.go            # like / follow / playlist new|add|remove (write)
├── memory.go             # feedback / mark / history / stats
├── events.go             # logEvent — instruments every mutating command
├── db.go / db_test.go    # SQLite scaffold + tracks cache
├── discovery.go          # fresh + helpers shared with the discovery commands
├── mix.go                # `spot mix`
├── daily.go              # `spot daily`
├── deeper.go             # `spot deeper`
├── rabbithole.go         # `spot rabbithole`
├── Makefile              # build / install / vet / fmt / tidy / clean
├── skills/                # Claude Code skill that wraps the CLI
│   ├── README.md
│   └── spot/SKILL.md
├── go.mod / go.sum
└── README.md
```
