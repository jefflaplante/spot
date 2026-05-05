package spot

// Read-only personalization endpoints: top tracks/artists, recently-played,
// liked tracks, followed artists, and the current user's playlists.
//
// All functions here go through getClient(ctx) — the same authenticated
// *spotify.Client used by play.go — and rely on the OAuth scopes wired up
// in spotifyScopes (user-top-read, user-read-recently-played,
// user-library-read).
//
// Pagination is transparent to callers: where Spotify returns a paged
// response and the caller asked for more than a single page, NextPage is
// driven internally until either the requested limit is reached or the
// server runs out of items.

import (
	"context"
	"fmt"
	"time"

	sp "github.com/zmb3/spotify/v2"
)

// TopRange selects the rolling window the personalization endpoints use to
// compute "top" tracks/artists. The values are the literal strings Spotify
// expects in the time_range query parameter.
type TopRange string

const (
	TopShort  TopRange = "short_term"
	TopMedium TopRange = "medium_term"
	TopLong   TopRange = "long_term"
)

// resolveTopRange maps the short labels accepted on the CLI (and "" for
// default) to the canonical short_term / medium_term / long_term values.
// It accepts either form so library callers can pass TopShort directly or
// the bare "short" used by argument parsing.
func resolveTopRange(r TopRange) (TopRange, error) {
	switch r {
	case "", "medium", TopMedium:
		return TopMedium, nil
	case "short", TopShort:
		return TopShort, nil
	case "long", TopLong:
		return TopLong, nil
	default:
		return "", fmt.Errorf("invalid time range %q (want short|medium|long)", r)
	}
}

// clampTopLimit applies the Spotify-side max of 50 and the default of 20
// for the /me/top/* endpoints (which only accept a single page).
func clampTopLimit(limit int) int {
	if limit <= 0 {
		return 20
	}
	if limit > 50 {
		return 50
	}
	return limit
}

// TopTracks returns the user's top tracks for the given time range. limit
// is capped at the Spotify per-call max of 50; pass <=0 for the default 20.
func TopTracks(ctx context.Context, r TopRange, limit int) ([]sp.FullTrack, error) {
	tr, err := resolveTopRange(r)
	if err != nil {
		return nil, err
	}
	c, err := getClient(ctx)
	if err != nil {
		return nil, err
	}
	page, err := c.CurrentUsersTopTracks(ctx, sp.Limit(clampTopLimit(limit)), sp.Timerange(sp.Range(tr)))
	if err != nil {
		return nil, fmt.Errorf("top tracks: %w", err)
	}
	return page.Tracks, nil
}

// TopArtists returns the user's top artists for the given time range. limit
// is capped at the Spotify per-call max of 50; pass <=0 for the default 20.
func TopArtists(ctx context.Context, r TopRange, limit int) ([]sp.FullArtist, error) {
	tr, err := resolveTopRange(r)
	if err != nil {
		return nil, err
	}
	c, err := getClient(ctx)
	if err != nil {
		return nil, err
	}
	page, err := c.CurrentUsersTopArtists(ctx, sp.Limit(clampTopLimit(limit)), sp.Timerange(sp.Range(tr)))
	if err != nil {
		return nil, fmt.Errorf("top artists: %w", err)
	}
	return page.Artists, nil
}

// RecentlyPlayed returns recently-played items, optionally trimmed to those
// played within the given duration before now. Spotify's endpoint only
// remembers the last fifty plays, so `limit` is capped at 50 and the time
// filter is applied client-side after the fetch.
//
// Pass since=0 to skip the time filter; pass limit<=0 for the default 50.
func RecentlyPlayed(ctx context.Context, since time.Duration, limit int) ([]sp.RecentlyPlayedItem, error) {
	c, err := getClient(ctx)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	opt := &sp.RecentlyPlayedOptions{Limit: sp.Numeric(limit)}
	items, err := c.PlayerRecentlyPlayedOpt(ctx, opt)
	if err != nil {
		return nil, fmt.Errorf("recently played: %w", err)
	}
	if since <= 0 {
		return items, nil
	}
	cutoff := time.Now().Add(-since)
	out := make([]sp.RecentlyPlayedItem, 0, len(items))
	for _, it := range items {
		if it.PlayedAt.After(cutoff) {
			out = append(out, it)
		}
	}
	return out, nil
}

// LikedTracks returns saved ("liked") tracks from the current user's library.
// Pages are fetched transparently up to the requested total. Pass limit<=0
// for the default of 50; pass a larger limit to gather multiple pages.
func LikedTracks(ctx context.Context, limit int) ([]sp.SavedTrack, error) {
	c, err := getClient(ctx)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	pageSize := 50
	if limit < pageSize {
		pageSize = limit
	}
	page, err := c.CurrentUsersTracks(ctx, sp.Limit(pageSize))
	if err != nil {
		return nil, fmt.Errorf("liked tracks: %w", err)
	}
	out := make([]sp.SavedTrack, 0, limit)
	out = append(out, page.Tracks...)
	for len(out) < limit && page.Next != "" {
		if err := c.NextPage(ctx, page); err != nil {
			return nil, fmt.Errorf("liked tracks (next page): %w", err)
		}
		out = append(out, page.Tracks...)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// FollowedArtists returns every artist the current user follows. The
// endpoint is cursor-paginated; this function walks all pages and returns
// the flattened list.
func FollowedArtists(ctx context.Context) ([]sp.FullArtist, error) {
	c, err := getClient(ctx)
	if err != nil {
		return nil, err
	}
	var out []sp.FullArtist
	var after string
	for {
		opts := []sp.RequestOption{sp.Limit(50)}
		if after != "" {
			opts = append(opts, sp.After(after))
		}
		page, err := c.CurrentUsersFollowedArtists(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("followed artists: %w", err)
		}
		out = append(out, page.Artists...)
		if page.Cursor.After == "" || page.Next == "" {
			break
		}
		after = page.Cursor.After
	}
	return out, nil
}

// MyPlaylists returns every playlist owned or followed by the current user,
// walking the offset-paginated endpoint until exhausted.
func MyPlaylists(ctx context.Context) ([]sp.SimplePlaylist, error) {
	c, err := getClient(ctx)
	if err != nil {
		return nil, err
	}
	page, err := c.CurrentUsersPlaylists(ctx, sp.Limit(50))
	if err != nil {
		return nil, fmt.Errorf("my playlists: %w", err)
	}
	out := make([]sp.SimplePlaylist, 0, len(page.Playlists))
	out = append(out, page.Playlists...)
	for page.Next != "" {
		if err := c.NextPage(ctx, page); err != nil {
			return nil, fmt.Errorf("my playlists (next page): %w", err)
		}
		out = append(out, page.Playlists...)
	}
	return out, nil
}
