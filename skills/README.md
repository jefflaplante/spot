# Claude Code skills

This directory holds Claude Code skills that wrap the `spot` CLI so you
can drive Spotify-on-Sonos from natural-language requests inside Claude
Code (e.g. "play Bonobo in the parlor", "what's playing?", "skip this
track").

## What's here

- [`spot/SKILL.md`](spot/SKILL.md) — controls Spotify playback on Sonos
  zones via the `spot` CLI. Activates on phrases like "play X on Y",
  "pause Y", "skip", "what's playing in Y", etc.

## Installing

Skills are loaded by Claude Code from one of two locations:

- **User-wide**: `~/.claude/skills/<name>/SKILL.md` — the skill is
  available in every project.
- **Project-local**: `<project>/.claude/skills/<name>/SKILL.md` — the
  skill is only loaded when you start Claude Code from that project.

Pick whichever scope you prefer. Symlinking is convenient because the
skill stays in sync as you update it here:

```bash
# user-wide (recommended)
mkdir -p ~/.claude/skills
ln -s "$(pwd)/skills/spot" ~/.claude/skills/spot

# OR project-local for a specific repo:
mkdir -p /path/to/project/.claude/skills
ln -s "$(pwd)/skills/spot" /path/to/project/.claude/skills/spot
```

If you'd rather copy than symlink:

```bash
mkdir -p ~/.claude/skills/spot
cp skills/spot/SKILL.md ~/.claude/skills/spot/
```

## Pre-flight

The skill assumes:

- `spot` is installed on `PATH` (`make install` from the repo root puts
  it in `$(go env GOBIN)`, typically `~/go/bin`).
- `spot auth` has been run once interactively, so a `.spotify-cache`
  refresh token exists.
- `SPOTIFY_ID` and `SPOTIFY_SECRET` are set in the environment Claude
  Code runs in (e.g. exported from your shell rc, or via the `env` block
  of a launcher).

The skill itself surfaces clear errors when any of these are missing.
