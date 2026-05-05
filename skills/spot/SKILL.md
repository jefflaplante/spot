---
name: spot
description: Use when the user wants to control Spotify playback on Sonos speakers — play, pause, resume, skip, stop, queue, browse, change volume, or check what's playing. Triggers on phrases like "play X on <zone>", "pause the parlor", "what's playing in the kitchen", "skip this track", "turn down the volume", "add X to the queue", "list my speakers". Wraps the local `spot` CLI; do NOT trigger for general music questions or playback in non-Sonos contexts.
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
| "what's playing in Y"                       | `spot now "Y"`                                    |
| "show the queue for Y"                      | `spot queue "Y"`                                  |
| "add X to Y's queue"                        | `spot enqueue "X" "Y"`                            |
| "louder" / "quieter" in Y                   | `spot vol "Y" up` / `spot vol "Y" down`           |
| "set Y to N percent" / "volume N in Y"      | `spot vol "Y" N`                                  |
| "list speakers" / "what zones do I have"    | `spot zones`                                      |

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
