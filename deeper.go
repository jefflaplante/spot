package spot

// Deeper surfaces "deep cuts" for an artist: tracks from their albums that
// don't make their top-10. The idea is to give a listener a path past the
// hits — pick an artist, get a chronological walk through their album
// catalog, biased toward tracks they probably haven't already heard a
// hundred times. Singles, compilations, and appears-on records are skipped
// so the result feels like the artist's "real" discography.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	sp "github.com/zmb3/spotify/v2"
)

// DeeperOptions tunes how Deeper builds its track list. PerAlbum caps how
// many non-top tracks are surfaced from each album — the default of 1 keeps
// long catalogs from blowing past sane queue lengths, while 2 (or more) is
// useful for artists whose albums you actually want to dig into.
type DeeperOptions struct {
	PerAlbum int // tracks to take per album; defaults to 1 if <= 0
}

// DeeperTrack is one surfaced track plus enough context to render it
// usefully in CLI output or to queue it on Sonos.
type DeeperTrack struct {
	AlbumID     string
	AlbumName   string
	ReleaseDate time.Time
	TrackID     string
	Title       string
	Artist      string
}

// resolveArtistID maps an arbitrary artist input to a Spotify artist ID.
// Accepts either a "spotify:artist:<id>" URI or a free-text query that gets
// run through Search. The first hit wins; an empty result is an error so
// the caller can surface a clear "no artist found" rather than walking an
// empty pipeline.
func resolveArtistID(ctx context.Context, c *sp.Client, query string) (sp.ID, string, error) {
	if strings.HasPrefix(query, "spotify:artist:") {
		id := sp.ID(strings.TrimPrefix(query, "spotify:artist:"))
		// Look up the artist so we can return a real name; if that fails
		// we still return the ID with an empty name rather than blocking.
		if a, err := c.GetArtist(ctx, id); err == nil {
			return id, a.Name, nil
		}
		return id, "", nil
	}
	if strings.HasPrefix(query, "spotify:") {
		return "", "", fmt.Errorf("only spotify:artist:... URIs supported (got %q)", query)
	}
	res, err := c.Search(ctx, query, sp.SearchTypeArtist, sp.Limit(1))
	if err != nil {
		return "", "", fmt.Errorf("search artist %q: %w", query, err)
	}
	if res.Artists == nil || len(res.Artists.Artists) == 0 {
		return "", "", fmt.Errorf("no artist found for %q", query)
	}
	a := res.Artists.Artists[0]
	return a.ID, a.Name, nil
}

// Deeper returns a chronological walk through an artist's albums (LPs only —
// no singles, compilations, or appears-on), with each album contributing
// up to opts.PerAlbum tracks that are NOT in the artist's top 10. The
// intent is to surface deep cuts in the order the artist released them.
func Deeper(ctx context.Context, artist string, opts DeeperOptions) ([]DeeperTrack, error) {
	if opts.PerAlbum <= 0 {
		opts.PerAlbum = 1
	}
	c, err := getClient(ctx)
	if err != nil {
		return nil, err
	}

	artistID, artistName, err := resolveArtistID(ctx, c, artist)
	if err != nil {
		return nil, err
	}

	// Skip-set of "well-known" track IDs. We don't want these in the
	// output: the whole point of Deeper is the OTHER tracks. US is a
	// reasonable default market — matches artistTopTracks in play.go.
	topTracks, err := c.GetArtistsTopTracks(ctx, artistID, "US")
	if err != nil {
		return nil, fmt.Errorf("artist top tracks: %w", err)
	}
	skip := make(map[string]struct{}, len(topTracks))
	for _, t := range topTracks {
		id := strings.TrimPrefix(string(t.URI), "spotify:track:")
		if id != "" {
			skip[id] = struct{}{}
		}
	}

	// Walk the artist's album list, paginating until exhausted. We
	// restrict to AlbumTypeAlbum so singles/compilations/appears_on don't
	// dilute the catalog; Spotify's `album_group` filter is what
	// include_groups encodes under the hood.
	page, err := c.GetArtistAlbums(ctx, artistID, []sp.AlbumType{sp.AlbumTypeAlbum}, sp.Market("US"), sp.Limit(50))
	if err != nil {
		return nil, fmt.Errorf("artist albums: %w", err)
	}
	type albumMeta struct {
		id      string
		name    string
		release time.Time
	}
	var albums []albumMeta
	seen := make(map[string]struct{}) // dedupe — Spotify returns regional duplicates
	for {
		for _, a := range page.Albums {
			if a.AlbumType != "" && a.AlbumType != "album" {
				continue
			}
			id := string(a.ID)
			if id == "" {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			albums = append(albums, albumMeta{
				id:      id,
				name:    a.Name,
				release: parseReleaseDate(a.ReleaseDate, a.ReleaseDatePrecision),
			})
		}
		if err := c.NextPage(ctx, page); err != nil {
			if err == sp.ErrNoMorePages {
				break
			}
			return nil, fmt.Errorf("artist albums page: %w", err)
		}
	}

	// Oldest first — that's the chronological-walk shape the spec asks
	// for, and it makes the listing read like a discography.
	sort.Slice(albums, func(i, j int) bool {
		return albums[i].release.Before(albums[j].release)
	})

	// For each album, fetch its tracks, drop the top-10 hits, and take
	// up to PerAlbum survivors. AlbumTrackIDs already strips the
	// spotify:track: prefix and skips empties.
	var out []DeeperTrack
	var allIDs []string
	type pending struct {
		albumID   string
		albumName string
		release   time.Time
		trackIDs  []string
	}
	var queue []pending
	for _, alb := range albums {
		ids, err := AlbumTrackIDs(ctx, alb.id)
		if err != nil || len(ids) == 0 {
			continue
		}
		var picked []string
		for _, id := range ids {
			if _, top := skip[id]; top {
				continue
			}
			picked = append(picked, id)
			if len(picked) >= opts.PerAlbum {
				break
			}
		}
		if len(picked) == 0 {
			continue
		}
		queue = append(queue, pending{
			albumID: alb.id, albumName: alb.name, release: alb.release, trackIDs: picked,
		})
		allIDs = append(allIDs, picked...)
	}

	// One batched track lookup so we can fill in titles/artists for
	// either the JSON output or DIDL metadata when --zone is set.
	enriched, _ := lookupSpotifyTracks(ctx, allIDs)
	for _, p := range queue {
		for _, id := range p.trackIDs {
			row := DeeperTrack{
				AlbumID:     p.albumID,
				AlbumName:   p.albumName,
				ReleaseDate: p.release,
				TrackID:     id,
				Artist:      artistName,
			}
			if info, ok := enriched[id]; ok && info != nil {
				row.Title = info.Title
				if info.Artist != "" {
					row.Artist = info.Artist
				}
				if row.AlbumName == "" {
					row.AlbumName = info.Album
				}
			}
			out = append(out, row)
		}
	}
	return out, nil
}
