---
name: spot
description: Use when the user wants to control Spotify playback on Sonos speakers, get personalized recommendations from their own Spotify history, or curate their library — play, pause, resume, skip, stop, shuffle, queue, browse, change volume, check what's playing, see top tracks/artists, list recents/likes/follows/playlists, like/follow/save the current track, build mixes, daily picks, deeper cuts, fresh releases, or "more like this", and record love/hate/skip-forever feedback or notes for what's playing. Triggers on phrases like "play X on <zone>", "pause the parlor", "what's playing in the kitchen", "skip this track", "shuffle the kitchen", "turn shuffle on/off", "mix it up", "add X to the queue", "turn down the volume", "list my speakers", "what are my top tracks", "what have I been playing", "show my liked songs", "list my playlists", "I love this", "I hate this", "save this song", "save this track", "queue up my favorites", "make me a playlist", "make me a mix", "play me something I'd love", "play me my morning music", "play me a daily mix", "what should I listen to today", "what's new from artists I follow", "deeper cuts from <artist>", "what does this artist sound like beyond the hits", "play me something like <artist>", "more like this", "what I've been into", "music for my morning", "why did you pick that". Wraps the local `spot` CLI; do NOT trigger for general music questions or playback in non-Sonos contexts.
---

# Spotify on Sonos via the `spot` CLI

`spot` is a CLI on the user's PATH that controls Spotify playback on Sonos
zones (and other Spotify Connect devices). It supports two transports:

- **`-v sonos`** — direct UPnP/SOAP to the Sonos coordinator. Works on
  every Sonos speaker that has Spotify linked, including older hardware
  (Play:1, Play:3, One SL) that doesn't register with Spotify Connect.
  **Default to this for any Sonos zone.**
- **default (no `-v`)** — Spotify Web API targeting a Spotify Connect
  device. Use for Echo, Google Home, phones running Spotify, modern Sonos
  with current firmware — anything that shows up in `spot devices`.

When the user names a Sonos zone, always pass `-v sonos`. The Connect
path frequently fails for Sonos because the speakers drop out of the
Spotify Connect device list when idle.

## Pre-flight (mention only on first failure, not proactively)

`spot` needs three things present at runtime:

- `SPOTIFY_ID` and `SPOTIFY_SECRET` env vars
- A `.spotify-cache` file (the OAuth refresh token; default cwd, override
  via `SPOTIFY_TOKEN_PATH`)
- LAN reachability to the Sonos household (or `SONOS_HOST=<ip>` if
  multicast is blocked)

If a `spot` command errors with "open token cache: ... no such file" or
"SPOTIFY_ID and SPOTIFY_SECRET must be set", tell the user they need to
run `spot auth` once interactively, with the env vars set, on a machine
with a browser (or via SSH port-forward `ssh -L 8888:127.0.0.1:8888 …`).
Don't try to run `spot auth` yourself unsolicited — it's interactive.

## Discovery

```bash
spot zones        # Sonos zones on the LAN, with coordinator IPs
spot devices      # devices Spotify Connect can target
```

Run `spot zones` if the user mentions a zone name you haven't seen — zone
matching is case-insensitive substring, so "parlor", "Parlor", and "parl"
all match the same zone. If the requested zone doesn't appear, list the
visible zones and ask which they meant.

## Playback

```bash
spot play -v sonos -c "<query>" "<zone>"     # default for "play X" requests
spot play -v sonos "<query>" "<zone>"        # one track, then stop
spot play -v sonos "spotify:track:<id>" "<zone>"
spot play "<query>" "<connect-device>"       # Connect path, no -v
```

`<query>` is free-text (`"bonobo migration"`) or Spotify field-search
syntax (`'artist:Subtronics track:"Itchy Scratchy"'`) or a Spotify URI.
Top hit wins.

**Default to `-c` when the user says "play X"** — they almost always
want continuous music, not a single track that stops dead. Drop `-c`
only if the user explicitly says "just one track" or names a single song
in a way that implies they want only that.

**Default to `-s` (shuffle) on top of `-c` when the query is an
artist-style request** — bare artist names like "play underworld",
"play armin van buuren", "put on some bonobo". With `-c -s`, the
candidate pool is broadened (artist top tracks plus a random album-cut
sample) and shuffled, so repeated requests don't produce the same
playlist. **Drop `-s` when the query is specific:**
- A Spotify track URI (`spotify:track:…`)
- Field-search syntax (`track:"…" album:"…"`)
- "Play album X" / "play playlist Y" — deterministic context
- A specific song name the user clearly wants ("play born slippy")

## Queue

```bash
spot enqueue "<query>" "<zone>"   # append a track
spot queue "<zone>"                # show queue + current position
```

`enqueue` does NOT start playback. If the zone isn't already playing
from its queue, run `spot play -v sonos -c …` first to seed the queue,
then `enqueue` adds to that same queue.

## Transport control

```bash
spot pause "<zone>"
spot resume "<zone>"     # continues from current position
spot stop "<zone>"       # resets to start of current track
spot next "<zone>"       # skip; alias: `spot skip`
```

When the user says "stop the music," they almost always mean **pause**,
not stop — pause keeps the position so resume just works. Use `stop`
only if they explicitly say "stop and reset" or similar.

`next` only does anything if a queue is loaded. If `now` shows a track
that arrived via `play -v sonos` without `-c`, `next` will halt
playback. Mention this if the user is surprised.

```bash
spot shuffle "<zone>"        # turn shuffle on (SHUFFLE_NOREPEAT)
spot shuffle "<zone>" off    # turn it off (NORMAL)
```

`shuffle` flips the speaker's playback mode without rearranging the
queue. For *content* entropy (different artist's-top-tracks set each
invocation), use `play -c -s` instead — that's about which tracks get
queued, not playback order over a fixed queue.

## Volume

```bash
spot vol "<zone>"          # show current
spot vol "<zone>" up       # +5
spot vol "<zone>" up 10    # +10
spot vol "<zone>" down     # -5
spot vol "<zone>" down 15  # -15
spot vol "<zone>" 50       # set absolute (clamped 0-100)
```

Use `up`/`down` words instead of `+5`/`-5` — Cobra parses leading dashes
as flags. The command prints the resulting volume after any change.

## Now playing

```bash
spot now "<zone>"
```

Returns title, artist, album, position, and state. For Spotify tracks,
metadata is filled in via the Spotify Web API even when Sonos's own
DIDL is empty.

## Personalization (read-only)

Browse the user's Spotify account. None of these mutate anything; all
support `--json` and pagination is transparent.

```bash
spot top tracks [short|medium|long] [--limit N]
spot top artists [short|medium|long] [--limit N]
spot recent [--since 24h] [--limit N]
spot liked [--limit N]
spot follows
spot playlists
```

Time ranges for `top`: `short` ≈ last 4 weeks, `medium` ≈ ~6 months
(default), `long` ≈ several years. `recent` only ever has the last 50
plays — Spotify exposes no more than that.

## Library mutation

State changes against the user's Spotify account. Each one also writes
an event to the local memory layer so `history` and `stats` see it.

```bash
spot like "<query>"            # save a track to Liked Songs
spot like --current "<zone>"   # like whatever's playing right now
spot unlike "<query>"          # or unlike --current "<zone>"
spot follow "<artist>"         # follow an artist
spot unfollow "<artist>"

spot playlist new "<name>" [--description "…"] [--public]
spot playlist add "<playlist>" "<query>"
spot playlist remove "<playlist>" "<query>"
```

`<playlist>` is a case-insensitive substring against the user's
playlists; if multiple match, the command errors and you should ask
which one. `--current` is the right flag when the user says "I love
this" or "save this track" — it reads `now` for the zone and operates
on that track ID.

## Memory (local, on-disk)

`spot` keeps a small SQLite log (`spot.db`) of mutations and `observe`
events for `now`/`queue`. Discovery commands read it back; you can query
it directly:

```bash
spot feedback love "<query>"
spot feedback hate --current --zone "<zone>"
spot feedback skip-forever --current --zone "<zone>" --note "wife hates it"

spot mark "<note>"             # annotate currently-playing track
spot mark --zone "<zone>" "<note>"

spot history [--zone --since 24h --artist X --kind play_started --limit N]
spot stats  [--top 10 | --recent | --skipped]
```

Verdicts feed into `mix` and `daily`: `hate`/`skip-forever` exclude a
track, `love` boosts it. `mark` is the right tool for free-text notes
about the moment ("perfect for the drive home", "what is this synth").

## Discovery and recommendations

Curated track lists. Every command can either print or queue on a Sonos
zone via `--zone`, and every command supports `--explain` to print the
reasoning before the list. When the user asks "why did you pick that?",
re-run the previous discovery command with `--explain`.

```bash
spot mix [--length 20] [--seed "<q>"] [--exclude-recent 7] [--zone Y] [--explain]
spot daily [--length 20] [--zone Y] [--explain]
spot fresh [--days 30] [--zone Y] [--all-tracks] [--explain]
spot deeper "<artist>" [--per-album 1] [--zone Y] [--explain]
spot rabbithole [--seed "<q>"] [--length 20] [--zone Y] [--explain]
```

- **`mix`** — medium-term top tracks (or seed + primary artist's top
  tracks) minus feedback excludes minus recently-played.
- **`daily`** — long-term top tracks the user hasn't played in 30
  days; loved tracks float to the top.
- **`fresh`** — albums released in the last `--days` from artists the
  user follows.
- **`deeper <artist>`** — non-greatest-hits tracks from a single
  artist's catalog, oldest first.
- **`rabbithole`** — seed-artist genre tags scored against the user's
  top artists; pulls top tracks from the eight closest matches.

Default to `--zone <zone>` whenever the user asks for music to play
("play me something I'd love", "play a daily mix in the parlor", "morning
music in the kitchen") rather than just "show me a list".

## Diagnostics (only if explicitly asked)

```bash
spot diag "<zone>"     # Spotify SMAPI service registration on the zone
spot probe "<zone>"    # raw TrackURI + DIDL-Lite of currently-playing
spot queue "<zone>" --raw   # raw Browse Q:0 XML
```

Useful when playback fails with UPnP errors or when track metadata
appears wrong.

## Quick interpretation map

| User says                                   | Command                                           |
|---------------------------------------------|---------------------------------------------------|
| "play X in/on Y"                            | `spot play -v sonos -c "X" "Y"`                   |
| "play just X" / "play this one track"       | `spot play -v sonos "X" "Y"` (no `-c`)            |
| "play <spotify URL/URI> on Y"               | `spot play -v sonos "<uri>" "Y"`                  |
| "pause Y" / "stop the music in Y"           | `spot pause "Y"`                                  |
| "resume Y" / "play (no track) in Y"         | `spot resume "Y"`                                 |
| "skip" / "next track in Y"                  | `spot next "Y"`                                   |
| "shuffle Y" / "shuffle on" / "mix it up"    | `spot shuffle "Y"`                                |
| "stop shuffling Y" / "shuffle off"          | `spot shuffle "Y" off`                            |
| "play <artist> on Y" (artist-style query)   | `spot play -v sonos -c -s "<artist>" "Y"`         |
| "what's playing in Y"                       | `spot now "Y"`                                    |
| "show the queue for Y"                      | `spot queue "Y"`                                  |
| "add X to Y's queue"                        | `spot enqueue "X" "Y"`                            |
| "louder" / "quieter" in Y                   | `spot vol "Y" up` / `spot vol "Y" down`           |
| "set Y to N percent" / "volume N in Y"      | `spot vol "Y" N`                                  |
| "list speakers" / "what zones do I have"    | `spot zones`                                      |
| "what are my top tracks/artists"            | `spot top tracks` / `spot top artists`            |
| "what have I been playing" / "recent plays" | `spot recent`                                     |
| "show my liked songs" / "saved tracks"      | `spot liked`                                      |
| "who do I follow"                           | `spot follows`                                    |
| "list my playlists"                         | `spot playlists`                                  |
| "I love this" / "save this track" in Y      | `spot like --current "Y"`                         |
| "I hate this" / "never play this again" in Y| `spot feedback skip-forever --current --zone "Y"` |
| "follow this artist <A>"                    | `spot follow "<A>"`                               |
| "add X to a playlist <P>"                   | `spot playlist add "<P>" "X"`                     |
| "make me a playlist called X"               | `spot playlist new "X"`                           |
| "what did I play yesterday/this week"       | `spot history --since 24h` / `--since 168h`       |
| "how often do I play X"                     | `spot history --artist "X"`                       |
| "what's my most-played"                     | `spot stats --top 10`                             |
| "make me a mix" / "play me something I'd love" | `spot mix --zone "Y"`                          |
| "play me a daily mix" / "morning music"     | `spot daily --zone "Y"`                           |
| "what should I listen to today"             | `spot daily --zone "Y"`                           |
| "what's new from artists I follow"          | `spot fresh` (or `--zone "Y"` to play it)         |
| "deeper cuts from <A>" / "more than the hits" | `spot deeper "<A>"`                             |
| "play me something like <A>" / "more like this" | `spot rabbithole --seed "<A>" --zone "Y"`     |
| "why did you pick that?"                    | re-run the previous discovery command with `--explain` |

## Output handling

Most commands print one short line on success. Read the exit code (`spot`
exits `0` on success, `1` on error). On error, the stderr message is
usually self-explanatory — surface it to the user verbatim rather than
re-interpreting.

For long output (e.g. `spot queue` on a busy zone), surface a summary
("12 tracks queued, currently on #3 — Subtronics, Itchy Scratchy")
unless the user asked to see the full list.

## Don'ts

- Don't run `spot auth` yourself; it's an interactive OAuth flow.
- Don't poll `spot now` or `spot queue` aggressively in a loop — Spotify
  rate-limits at ~once/sec for the player endpoints.
- Don't pass `-v connect` for a Sonos zone unless the user explicitly
  asks; the Sonos path is more reliable.
- Don't substitute zone names — match what the user said as a substring.
  If multiple zones match, ask.
