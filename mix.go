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

// MixExplanation captures the algorithmic reasoning behind a Mix call so
// callers (e.g. the CLI's --explain flag) can surface why each track was
// picked. Counts are recorded after the relevant filter ran, so the math
// reads PoolSize - DroppedHate - DroppedRecent - DroppedDup = Final
// (ignoring truncation to opts.Length).
type MixExplanation struct {
	// SeedKind describes how the candidate pool was chosen. One of:
	// "top_medium_term" (no seed), "artist" (artist URI seed), or
	// "track" (track URI / free-text seed).
	SeedKind string

	// SeedTrackID is the resolved track ID when SeedKind == "track".
	SeedTrackID string

	// SeedTrackTitle / SeedTrackArtist are the resolved track's metadata
	// when SeedKind == "track". Empty otherwise.
	SeedTrackTitle  string
	SeedTrackArtist string

	// SeedArtistID / SeedArtistName describe the artist anchor when the
	// pool is artist-derived (SeedKind "artist" or "track").
	SeedArtistID   string
	SeedArtistName string

	// PoolSource is a short human label for where the pool came from,
	// e.g. "your top medium_term tracks" or "Bonobo's top tracks".
	PoolSource string

	// PoolSize is the number of unfiltered candidates returned by the
	// candidate-fetch step (Spotify-side max ~50).
	PoolSize int

	// DroppedHate is the number of candidates dropped by the
	// skip-forever / hate feedback filter.
	DroppedHate int

	// DroppedRecent is the number of candidates dropped by the
	// recently-played filter (kind='play_started' within ExcludeRecent).
	DroppedRecent int

	// DroppedDup is the number of candidates dropped because they
	// duplicated an earlier-kept track in the pool.
	DroppedDup int

	// Final is the number of tracks returned after filtering and
	// truncation to opts.Length.
	Final int

	// ExcludeRecentDays is the recent-play window in days, for display.
	// 0 means the filter was disabled.
	ExcludeRecentDays int
}

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
	tracks, _, err := MixWithExplain(ctx, opts)
	return tracks, err
}

// MixWithExplain is Mix plus an explanation struct describing the seed,
// the candidate pool, and the per-filter drop counts. Behavior of the
// returned tracks slice is identical to Mix.
func MixWithExplain(ctx context.Context, opts MixOptions) ([]*trackInfo, *MixExplanation, error) {
	length := opts.Length
	if length <= 0 {
		length = 20
	}
	excludeRecent := opts.ExcludeRecent
	if excludeRecent == 0 {
		excludeRecent = 7 * 24 * time.Hour
	}

	candidates, seedInfo, err := mixCandidatesExplain(ctx, opts.Seed)
	if err != nil {
		return nil, nil, err
	}
	if len(candidates) == 0 {
		return nil, nil, fmt.Errorf("no candidate tracks for mix")
	}

	exp := &MixExplanation{
		SeedKind:        seedInfo.kind,
		SeedTrackID:     seedInfo.trackID,
		SeedTrackTitle:  seedInfo.trackTitle,
		SeedTrackArtist: seedInfo.trackArtist,
		SeedArtistID:    seedInfo.artistID,
		SeedArtistName:  seedInfo.artistName,
		PoolSource:      seedInfo.poolSource,
		PoolSize:        len(candidates),
	}
	if excludeRecent > 0 {
		exp.ExcludeRecentDays = int(excludeRecent / (24 * time.Hour))
	}

	// Best-effort feedback filter: query distinct track_ids that have a
	// skip-forever or hate verdict. If anything goes wrong (DB missing,
	// JSON1 absent, payload format drift) we skip the filter rather
	// than fail the whole command.
	hateSet := mixSkipSet(ctx)

	// Best-effort recent-plays filter. Same posture as above.
	recentSet := map[string]struct{}{}
	if excludeRecent > 0 {
		recentSet = mixRecentSet(ctx, excludeRecent)
	}

	out := make([]*trackInfo, 0, length)
	seen := make(map[string]struct{}, length)
	for _, t := range candidates {
		if t == nil || t.TrackID == "" {
			continue
		}
		if _, dup := seen[t.TrackID]; dup {
			exp.DroppedDup++
			continue
		}
		if _, drop := hateSet[t.TrackID]; drop {
			exp.DroppedHate++
			continue
		}
		if _, drop := recentSet[t.TrackID]; drop {
			exp.DroppedRecent++
			continue
		}
		seen[t.TrackID] = struct{}{}
		out = append(out, t)
		if len(out) >= length {
			break
		}
	}
	exp.Final = len(out)
	return out, exp, nil
}

// mixSeedInfo captures the human-readable provenance of a Mix candidate
// pool — what seed was used and where the candidates came from.
type mixSeedInfo struct {
	kind        string // "top_medium_term" | "artist" | "track"
	trackID     string
	trackTitle  string
	trackArtist string
	artistID    string
	artistName  string
	poolSource  string // human label: "your top medium_term tracks", etc.
}

// mixCandidatesExplain assembles the unfiltered candidate pool and
// records what kind of seed produced it. With a seed: resolve the seed
// (track or artist) and pull artist top-tracks, with the seed track
// itself prepended when applicable. Without a seed: the user's
// medium_term top tracks (~50).
func mixCandidatesExplain(ctx context.Context, seed string) ([]*trackInfo, mixSeedInfo, error) {
	var info mixSeedInfo
	if seed == "" {
		tracks, err := TopTracks(ctx, TopMedium, mixCandidatePool)
		if err != nil {
			return nil, info, err
		}
		out := make([]*trackInfo, 0, len(tracks))
		for i := range tracks {
			out = append(out, makeTrackInfo(&tracks[i]))
		}
		info.kind = "top_medium_term"
		info.poolSource = "your top medium_term tracks"
		return out, info, nil
	}

	// Artist URI: skip the track resolve, jump straight to top-tracks.
	if strings.HasPrefix(seed, "spotify:artist:") {
		artistID := strings.TrimPrefix(seed, "spotify:artist:")
		more, err := artistTopTracks(ctx, artistID, "")
		if err != nil {
			return nil, info, err
		}
		info.kind = "artist"
		info.artistID = artistID
		// Best-effort: borrow the artist name from the first track's metadata.
		if len(more) > 0 && more[0] != nil {
			info.artistName = more[0].Artist
		}
		if info.artistName != "" {
			info.poolSource = info.artistName + "'s top tracks"
		} else {
			info.poolSource = "artist top tracks"
		}
		return more, info, nil
	}

	// Track URI or free-text query. Resolve, then pull the primary
	// artist's top tracks and prepend the seed.
	tk, err := resolveTrack(ctx, seed)
	if err != nil {
		return nil, info, fmt.Errorf("resolve seed: %w", err)
	}
	out := []*trackInfo{tk}
	info.kind = "track"
	info.trackID = tk.TrackID
	info.trackTitle = tk.Title
	info.trackArtist = tk.Artist
	info.artistID = tk.ArtistID
	info.artistName = tk.Artist
	if tk.Artist != "" {
		info.poolSource = tk.Artist + "'s top tracks (anchored on " + tk.Title + ")"
	} else {
		info.poolSource = "seed track + artist top tracks"
	}
	if tk.ArtistID != "" {
		more, err := artistTopTracks(ctx, tk.ArtistID, tk.TrackID)
		if err != nil {
			return out, info, nil // keep at least the seed; surface no error
		}
		out = append(out, more...)
	}
	return out, info, nil
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

