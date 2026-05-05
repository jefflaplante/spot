package spot

// Mix is a curated track-list surface for the local agent: combine the
// user's recent listening signal (top tracks, or a seed-anchored artist
// pool) with the local feedback memory in spot.db, and produce a list of
// fresh-feeling track IDs the user is likely to want to hear.
//
// The pipeline is intentionally small and best-effort: the SQLite memory
// is always optional — if a query fails (no DB yet, schema drift, JSON1
// missing, etc.) we skip the filter rather than abort the command. The
// goal is "give me something to play", not "guaranteed perfect results".

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// MixOptions controls the candidate pool size, optional seed anchor, and
// the recent-play exclusion window. Zero values pick sensible defaults.
type MixOptions struct {
	// Length is the maximum number of tracks the resulting mix carries.
	// <=0 falls back to 20.
	Length int

	// Seed optionally anchors the mix to a specific track or artist. It
	// accepts the same shapes as resolveTrack ("spotify:track:..." URI
	// or free-text query) plus "spotify:artist:..." URIs for artist
	// seeds. Empty means "use the user's medium_term top tracks".
	Seed string

	// ExcludeRecent drops candidates that have a kind='play_started' row
	// within this duration before now. Zero falls back to 7*24h. Pass a
	// negative value to skip the recent-play filter entirely.
	ExcludeRecent time.Duration
}

// mixCandidatePool is the size of the no-seed top-tracks pool we pull
// from /me/top/tracks before filtering. 50 is the Spotify per-call max.
const mixCandidatePool = 50

// Mix builds a curated track list per the algorithm in MixOptions. The
// list is filtered against local feedback (skip-forever / hate verdicts)
// and recently-played history, then truncated to opts.Length.
//
// Returns nil and an error if the candidate fetch fails. Filter failures
// are best-effort and silently skipped: the caller still gets a list.
func Mix(ctx context.Context, opts MixOptions) ([]*trackInfo, error) {
	length := opts.Length
	if length <= 0 {
		length = 20
	}
	excludeRecent := opts.ExcludeRecent
	if excludeRecent == 0 {
		excludeRecent = 7 * 24 * time.Hour
	}

	candidates, err := mixCandidates(ctx, opts.Seed)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no candidate tracks for mix")
	}

	// Best-effort feedback filter: query distinct track_ids that have a
	// skip-forever or hate verdict. If anything goes wrong (DB missing,
	// JSON1 absent, payload format drift) we skip the filter rather
	// than fail the whole command.
	skip := mixSkipSet(ctx)

	// Best-effort recent-plays filter. Same posture as above.
	if excludeRecent > 0 {
		for id := range mixRecentSet(ctx, excludeRecent) {
			skip[id] = struct{}{}
		}
	}

	out := make([]*trackInfo, 0, length)
	seen := make(map[string]struct{}, length)
	for _, t := range candidates {
		if t == nil || t.TrackID == "" {
			continue
		}
		if _, dup := seen[t.TrackID]; dup {
			continue
		}
		if _, drop := skip[t.TrackID]; drop {
			continue
		}
		seen[t.TrackID] = struct{}{}
		out = append(out, t)
		if len(out) >= length {
			break
		}
	}
	return out, nil
}

// mixCandidates assembles the unfiltered candidate pool. With a seed:
// resolve the seed (track or artist) and pull artist top-tracks, with
// the seed track itself prepended when applicable. Without a seed: the
// user's medium_term top tracks (~50).
func mixCandidates(ctx context.Context, seed string) ([]*trackInfo, error) {
	if seed == "" {
		tracks, err := TopTracks(ctx, TopMedium, mixCandidatePool)
		if err != nil {
			return nil, err
		}
		out := make([]*trackInfo, 0, len(tracks))
		for i := range tracks {
			out = append(out, makeTrackInfo(&tracks[i]))
		}
		return out, nil
	}

	// Artist URI: skip the track resolve, jump straight to top-tracks.
	if strings.HasPrefix(seed, "spotify:artist:") {
		artistID := strings.TrimPrefix(seed, "spotify:artist:")
		more, err := artistTopTracks(ctx, artistID, "")
		if err != nil {
			return nil, err
		}
		return more, nil
	}

	// Track URI or free-text query. Resolve, then pull the primary
	// artist's top tracks and prepend the seed.
	info, err := resolveTrack(ctx, seed)
	if err != nil {
		return nil, fmt.Errorf("resolve seed: %w", err)
	}
	out := []*trackInfo{info}
	if info.ArtistID != "" {
		more, err := artistTopTracks(ctx, info.ArtistID, info.TrackID)
		if err != nil {
			return out, nil // keep at least the seed; surface no error
		}
		out = append(out, more...)
	}
	return out, nil
}

// mixSkipSet returns the set of track IDs the user has marked
// skip-forever or hate. Best-effort: returns an empty set on any error
// (DB unopened, query failure, etc.) so callers can keep going.
func mixSkipSet(ctx context.Context) map[string]struct{} {
	out := make(map[string]struct{})
	db, err := getDB(ctx)
	if err != nil || db == nil {
		return out
	}
	// payload is JSON like {"verdict":"skip-forever"}; use LIKE so we
	// don't depend on the SQLite JSON1 extension being compiled in.
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT track_id
		  FROM events
		 WHERE kind = 'feedback'
		   AND track_id IS NOT NULL
		   AND (payload LIKE '%"verdict":"skip-forever"%'
		     OR payload LIKE '%"verdict":"hate"%')`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id sql.NullString
		if err := rows.Scan(&id); err != nil {
			continue
		}
		if id.Valid && id.String != "" {
			out[id.String] = struct{}{}
		}
	}
	return out
}

// mixRecentSet returns the set of track IDs with a kind='play_started'
// event newer than `since` ago. Best-effort: empty set on any error.
func mixRecentSet(ctx context.Context, since time.Duration) map[string]struct{} {
	out := make(map[string]struct{})
	db, err := getDB(ctx)
	if err != nil || db == nil {
		return out
	}
	cutoff := time.Now().Add(-since).Unix()
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT track_id
		  FROM events
		 WHERE kind = 'play_started'
		   AND track_id IS NOT NULL
		   AND occurred_at > ?`, cutoff)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id sql.NullString
		if err := rows.Scan(&id); err != nil {
			continue
		}
		if id.Valid && id.String != "" {
			out[id.String] = struct{}{}
		}
	}
	return out
}

