package spot

// Rabbithole — substitute for Spotify's deprecated related-artists endpoint.
//
// We can't ask Spotify "give me artists similar to X" any more, so this
// approximates it from signals we still have access to: the seed artist's
// genre tags + the user's own top-artists history. Score each top artist by
// how many genres they share with the seed, take the top few by overlap,
// and pull a couple of their top tracks. The result is a "rabbithole" walk
// rooted at the seed but personalized to the listener.
//
// The library function returns both the chosen tracks and the per-artist
// walk that produced them, so a future --explain flag can narrate why
// each artist was picked. The default human CLI output prints just the
// tracks; --json includes both fields.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	sp "github.com/zmb3/spotify/v2"
)

// Tunables for the walk. Kept as constants — the CLI exposes --length
// for the final track count, but the per-artist track count and the
// walk fanout are not worth surfacing as flags yet.
const (
	rabbitholeDefaultLength = 20
	rabbitholeMaxArtists    = 8 // top-N artists by genre overlap
	rabbitholeTracksPer     = 3 // tracks taken from each chosen artist
	rabbitholeSeedTracks    = 2 // tracks from the seed artist (front of list)
)

// RabbitholeOptions configures a Rabbithole call.
type RabbitholeOptions struct {
	// Seed is either a "spotify:artist:<id>" URI, a free-text artist name
	// to search, or "" — in which case the user's top short-term artist
	// is used as the seed.
	Seed string

	// Length is the maximum number of tracks to return. <=0 falls back to
	// the default (20).
	Length int
}

// RabbitholeStep is one artist visited during the walk. The library
// returns the steps alongside the chosen tracks so callers (e.g. a
// future --explain flag) can show "we picked Foo because they share 3
// genres with the seed; took these 3 tracks".
type RabbitholeStep struct {
	ArtistID     string   `json:"artist_id"`
	ArtistName   string   `json:"artist_name"`
	GenreOverlap int      `json:"genre_overlap"` // count of shared genres with seed
	TrackIDs     []string `json:"track_ids"`
}

// Rabbithole walks from a seed artist outward through genre overlap with
// the user's own top artists, returning a curated track list plus the
// step-by-step walk that produced it.
//
// Algorithm (substitute for related-artists):
//  1. Resolve the seed artist (URI, query, or default to user's top short-term).
//  2. Read the seed's genre tags.
//  3. Union the user's top medium- and long-term artists, dedup by ID.
//  4. Score each by overlap_count / max(len(seed.genres), 1), descending.
//  5. Take top rabbitholeMaxArtists, pull rabbitholeTracksPer top tracks each
//     (skipping seed-artist tracks; those are added separately at the front).
//  6. Prepend rabbitholeSeedTracks of the seed's own top tracks.
//  7. Truncate to Length.
func Rabbithole(ctx context.Context, opts RabbitholeOptions) ([]*trackInfo, []RabbitholeStep, error) {
	c, err := getClient(ctx)
	if err != nil {
		return nil, nil, err
	}

	length := opts.Length
	if length <= 0 {
		length = rabbitholeDefaultLength
	}

	seed, err := resolveSeedArtist(ctx, c, opts.Seed)
	if err != nil {
		return nil, nil, err
	}

	// Pull user top artists from medium and long ranges; union+dedup. The
	// short range is excluded on purpose — short-term taste is often the
	// seed itself, and we want to surface artists the user knows but isn't
	// currently listening to.
	med, err := TopArtists(ctx, TopMedium, 50)
	if err != nil {
		return nil, nil, fmt.Errorf("rabbithole: top artists (medium): %w", err)
	}
	long, err := TopArtists(ctx, TopLong, 50)
	if err != nil {
		return nil, nil, fmt.Errorf("rabbithole: top artists (long): %w", err)
	}
	pool := dedupArtists(append(append([]sp.FullArtist{}, med...), long...))

	// Score each pool artist by genre overlap with the seed. The seed
	// itself is filtered out so it doesn't compete for a slot.
	seedGenres := lowerSet(seed.Genres)
	type scored struct {
		artist  sp.FullArtist
		overlap int
	}
	candidates := make([]scored, 0, len(pool))
	for _, a := range pool {
		if string(a.ID) == string(seed.ID) {
			continue
		}
		ov := genreOverlap(seedGenres, a.Genres)
		if ov == 0 {
			continue
		}
		candidates = append(candidates, scored{artist: a, overlap: ov})
	}
	// Stable sort: higher overlap first, then by descending popularity for
	// a tie-break that's deterministic from Spotify's perspective.
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].overlap != candidates[j].overlap {
			return candidates[i].overlap > candidates[j].overlap
		}
		return candidates[i].artist.Popularity > candidates[j].artist.Popularity
	})
	if len(candidates) > rabbitholeMaxArtists {
		candidates = candidates[:rabbitholeMaxArtists]
	}

	// Walk: seed first, then the chosen artists. Each step records what
	// tracks contributed, so the caller can render the rationale.
	walk := make([]RabbitholeStep, 0, 1+len(candidates))
	tracks := make([]*trackInfo, 0, length)
	seen := make(map[string]struct{}, length)

	// 1) Seed artist's own top 1-2 tracks at the front. We don't dedup
	// against the seed's own artistTopTracks because excludeID wants a
	// single track to skip — we just slice rabbitholeSeedTracks here.
	seedTracks, err := artistTopTracks(ctx, string(seed.ID), "")
	if err != nil {
		return nil, nil, err
	}
	if rabbitholeSeedTracks < len(seedTracks) {
		seedTracks = seedTracks[:rabbitholeSeedTracks]
	}
	seedStep := RabbitholeStep{
		ArtistID:     string(seed.ID),
		ArtistName:   seed.Name,
		GenreOverlap: len(seedGenres), // self-overlap is the full count
		TrackIDs:     make([]string, 0, len(seedTracks)),
	}
	for _, t := range seedTracks {
		if _, dup := seen[t.TrackID]; dup {
			continue
		}
		seen[t.TrackID] = struct{}{}
		tracks = append(tracks, t)
		seedStep.TrackIDs = append(seedStep.TrackIDs, t.TrackID)
	}
	walk = append(walk, seedStep)

	// 2) Chosen artists. Skip any track whose primary artist is the seed
	// (these are already at the front).
	seedID := string(seed.ID)
	for _, sc := range candidates {
		if len(tracks) >= length {
			break
		}
		artistID := string(sc.artist.ID)
		top, err := artistTopTracks(ctx, artistID, "")
		if err != nil {
			// Best-effort: a single artist failure shouldn't kill the walk.
			continue
		}
		step := RabbitholeStep{
			ArtistID:     artistID,
			ArtistName:   sc.artist.Name,
			GenreOverlap: sc.overlap,
			TrackIDs:     make([]string, 0, rabbitholeTracksPer),
		}
		taken := 0
		for _, t := range top {
			if taken >= rabbitholeTracksPer {
				break
			}
			if t.ArtistID == seedID {
				continue
			}
			if _, dup := seen[t.TrackID]; dup {
				continue
			}
			seen[t.TrackID] = struct{}{}
			tracks = append(tracks, t)
			step.TrackIDs = append(step.TrackIDs, t.TrackID)
			taken++
			if len(tracks) >= length {
				break
			}
		}
		walk = append(walk, step)
	}

	if len(tracks) > length {
		tracks = tracks[:length]
	}
	return tracks, walk, nil
}

// resolveSeedArtist turns a Seed string into a FullArtist. The three
// recognized forms are "spotify:artist:<id>", a free-text query, and ""
// (use the user's top short-term artist).
func resolveSeedArtist(ctx context.Context, c *sp.Client, seed string) (*sp.FullArtist, error) {
	if strings.HasPrefix(seed, "spotify:artist:") {
		id := sp.ID(strings.TrimPrefix(seed, "spotify:artist:"))
		a, err := c.GetArtist(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("rabbithole: get seed artist %s: %w", id, err)
		}
		return a, nil
	}
	if strings.HasPrefix(seed, "spotify:") {
		return nil, fmt.Errorf("rabbithole: only spotify:artist:<id> URIs accepted as seed (got %q)", seed)
	}
	if seed != "" {
		res, err := c.Search(ctx, seed, sp.SearchTypeArtist, sp.Limit(1))
		if err != nil {
			return nil, fmt.Errorf("rabbithole: search seed %q: %w", seed, err)
		}
		if res.Artists == nil || len(res.Artists.Artists) == 0 {
			return nil, fmt.Errorf("rabbithole: no artist matching %q", seed)
		}
		// Search returns []FullArtist by value; we want a pointer to the
		// first hit so the genre slice is preserved.
		a := res.Artists.Artists[0]
		// Re-fetch via GetArtist to guarantee the genres slice is
		// populated — search results sometimes ship with a stub artist.
		full, err := c.GetArtist(ctx, a.ID)
		if err != nil {
			return nil, fmt.Errorf("rabbithole: hydrate seed %s: %w", a.ID, err)
		}
		return full, nil
	}
	// No seed — pick the user's top short-term artist.
	top, err := TopArtists(ctx, TopShort, 1)
	if err != nil {
		return nil, fmt.Errorf("rabbithole: default seed (top short-term): %w", err)
	}
	if len(top) == 0 {
		return nil, fmt.Errorf("rabbithole: no seed given and user has no top short-term artists")
	}
	// TopArtists returns FullArtist by value; re-fetch isn't strictly needed
	// because /me/top/artists returns full objects, but we want a pointer.
	a := top[0]
	return &a, nil
}

// dedupArtists returns the input slice with duplicates (by Spotify ID)
// removed; first occurrence wins.
func dedupArtists(in []sp.FullArtist) []sp.FullArtist {
	seen := make(map[sp.ID]struct{}, len(in))
	out := make([]sp.FullArtist, 0, len(in))
	for _, a := range in {
		if _, dup := seen[a.ID]; dup {
			continue
		}
		seen[a.ID] = struct{}{}
		out = append(out, a)
	}
	return out
}

// lowerSet folds genre tags to a lower-case lookup set. Spotify's genre
// strings are lower-case in practice, but the comparison is case-folded
// here for safety.
func lowerSet(genres []string) map[string]struct{} {
	out := make(map[string]struct{}, len(genres))
	for _, g := range genres {
		out[strings.ToLower(strings.TrimSpace(g))] = struct{}{}
	}
	delete(out, "")
	return out
}

// genreOverlap counts how many of `theirs` appear in the seed set.
func genreOverlap(seed map[string]struct{}, theirs []string) int {
	if len(seed) == 0 {
		return 0
	}
	n := 0
	for _, g := range theirs {
		key := strings.ToLower(strings.TrimSpace(g))
		if key == "" {
			continue
		}
		if _, ok := seed[key]; ok {
			n++
		}
	}
	return n
}
