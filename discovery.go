package spot

// Discovery surfaces — finds music for the user from signals other than
// direct queries. Phase 4 starts with `Fresh`: recent releases from
// followed artists.

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	sp "github.com/zmb3/spotify/v2"
)

// FreshAlbum is one recently-released album from a followed artist. The
// TrackIDs slice carries the Spotify track IDs to surface for the album:
// by default just the first track on the album, but callers can opt into
// every track by setting the all-tracks flag in Fresh's request shape if
// we ever add one. Today Fresh always populates the first-track-only form;
// the CLI expands to all-tracks after the fact when --all-tracks is set.
type FreshAlbum struct {
	AlbumID     string
	AlbumName   string
	ArtistID    string
	ArtistName  string
	ReleaseDate time.Time
	TrackIDs    []string
}

// freshFanout is the number of concurrent GetArtistAlbums calls in Fresh's
// walk over followed artists. Tuned to be polite to Spotify's rate limiter
// while still being noticeably faster than a serial walk for someone with
// a few hundred follows.
const freshFanout = 5

// parseReleaseDate maps Spotify's release_date + release_date_precision to
// a time.Time. Spotify returns "2026-04-15", "2026-04", or "2026" depending
// on how confident the catalog is in the actual date. Year-only values are
// treated as Jan 1 of that year (spec calls for this); month-only is the
// 1st of the month. Returns the zero time if parsing fails outright.
func parseReleaseDate(date, precision string) time.Time {
	if date == "" {
		return time.Time{}
	}
	// Try the most-specific format first, then back off. We don't strictly
	// trust `precision` because some payloads have set it to "day" with a
	// year-only date in the wild; falling through is the safe play.
	if t, err := time.Parse("2006-01-02", date); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01", date); err == nil {
		return t
	}
	if y, err := strconv.Atoi(strings.TrimSpace(date)); err == nil {
		return time.Date(y, time.January, 1, 0, 0, 0, 0, time.UTC)
	}
	_ = precision
	return time.Time{}
}

// Fresh returns albums from followed artists with a release_date in the
// last `since` window, sorted newest-first and capped at `limit` (0 means
// unlimited). For each album, TrackIDs is the album's first track only —
// enough for a "what's new" surface; expand from there with GetAlbum if
// needed. Walks the followed-artist list with bounded concurrency.
func Fresh(ctx context.Context, since time.Duration, limit int) ([]FreshAlbum, error) {
	c, err := getClient(ctx)
	if err != nil {
		return nil, err
	}
	artists, err := FollowedArtists(ctx)
	if err != nil {
		return nil, err
	}
	if since <= 0 {
		since = 30 * 24 * time.Hour
	}
	cutoff := time.Now().Add(-since)

	// Fan out: workers consume artist IDs from `jobs`, push FreshAlbum
	// candidates to `out`. We collect into a slice under a mutex rather
	// than buffering on a channel so we don't have to size the channel.
	type job struct {
		id   sp.ID
		name string
	}
	jobs := make(chan job)
	var (
		mu       sync.Mutex
		all      []FreshAlbum
		firstErr error
	)
	var wg sync.WaitGroup
	workers := freshFanout
	if workers > len(artists) && len(artists) > 0 {
		workers = len(artists)
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				page, err := c.GetArtistAlbums(ctx, j.id, []sp.AlbumType{sp.AlbumTypeAlbum, sp.AlbumTypeSingle})
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("artist albums %s: %w", j.id, err)
					}
					mu.Unlock()
					continue
				}
				var local []FreshAlbum
				for _, a := range page.Albums {
					rel := parseReleaseDate(a.ReleaseDate, a.ReleaseDatePrecision)
					if rel.IsZero() || rel.Before(cutoff) {
						continue
					}
					local = append(local, FreshAlbum{
						AlbumID:     string(a.ID),
						AlbumName:   a.Name,
						ArtistID:    string(j.id),
						ArtistName:  j.name,
						ReleaseDate: rel,
					})
				}
				if len(local) == 0 {
					continue
				}
				mu.Lock()
				all = append(all, local...)
				mu.Unlock()
			}
		}()
	}

	// Feed the workers, honoring ctx cancellation so a Ctrl-C doesn't
	// have to wait for the entire follow list to drain.
	go func() {
		defer close(jobs)
		for _, a := range artists {
			select {
			case <-ctx.Done():
				return
			case jobs <- job{id: a.ID, name: a.Name}:
			}
		}
	}()
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Newest-first.
	sort.Slice(all, func(i, j int) bool {
		return all[i].ReleaseDate.After(all[j].ReleaseDate)
	})
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}

	// Populate TrackIDs with the album's first track. Best-effort per
	// album: a single failure shouldn't kill the whole surface.
	for i := range all {
		full, err := c.GetAlbum(ctx, sp.ID(all[i].AlbumID))
		if err != nil {
			continue
		}
		if len(full.Tracks.Tracks) == 0 {
			continue
		}
		ids := make([]string, 0, len(full.Tracks.Tracks))
		for _, t := range full.Tracks.Tracks {
			id := strings.TrimPrefix(string(t.URI), "spotify:track:")
			if id == "" {
				continue
			}
			ids = append(ids, id)
			break // first track only — see doc comment on FreshAlbum.
		}
		all[i].TrackIDs = ids
	}

	return all, nil
}

// AlbumTrackIDs returns every track ID on the given album, in album
// order. Useful to expand a Fresh result when callers want every track
// queued rather than just the first-track teaser.
func AlbumTrackIDs(ctx context.Context, albumID string) ([]string, error) {
	c, err := getClient(ctx)
	if err != nil {
		return nil, err
	}
	full, err := c.GetAlbum(ctx, sp.ID(albumID))
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(full.Tracks.Tracks))
	for _, t := range full.Tracks.Tracks {
		id := strings.TrimPrefix(string(t.URI), "spotify:track:")
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// PlayTrackIDsViaSonos queues the given Spotify track IDs on the named
// Sonos zone and starts playback. Track metadata is enriched via the
// Spotify Web API so the queue listing in the Sonos app shows real
// titles/artists rather than blank stubs.
//
// This is a thin wrapper over the same private playQueueOnSonos used by
// PlayViaSonos with WithContinue() — it lets discovery surfaces (Fresh,
// future "for you" commands, etc.) hand a pre-built track list to Sonos
// without re-implementing service lookup, queue clear, and DIDL metadata.
func PlayTrackIDsViaSonos(ctx context.Context, zone string, trackIDs []string) error {
	if len(trackIDs) == 0 {
		return fmt.Errorf("no tracks to play")
	}
	zones, err := ListSonosZones(ctx)
	if err != nil {
		return err
	}
	needle := strings.ToLower(zone)
	var z *Zone
	for i := range zones {
		if strings.Contains(strings.ToLower(zones[i].Name), needle) {
			z = &zones[i]
			break
		}
	}
	if z == nil {
		names := make([]string, len(zones))
		for i, zz := range zones {
			names[i] = zz.Name
		}
		return fmt.Errorf("no Sonos zone matching %q; visible zones: %v", zone, names)
	}

	cfg, err := lookupSpotifyService(ctx, z.CoordinatorIP)
	if err != nil {
		return err
	}

	// Enrich metadata in one batched lookup so each queued item carries
	// title/artist/album in its DIDL-Lite blob. Best-effort: if Spotify
	// errors, we still queue the bare URIs.
	enriched, _ := lookupSpotifyTracks(ctx, trackIDs)
	tracks := make([]*trackInfo, 0, len(trackIDs))
	for _, id := range trackIDs {
		if info, ok := enriched[id]; ok && info != nil {
			tracks = append(tracks, info)
			continue
		}
		tracks = append(tracks, &trackInfo{TrackID: id})
	}
	return playQueueOnSonos(ctx, z, tracks, cfg)
}
