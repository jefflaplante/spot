package spot

import (
	"context"
	"errors"
	"fmt"
	"strings"

	sp "github.com/zmb3/spotify/v2"
)

// Like resolves `query` (free-text or "spotify:track:..." URI) to a single
// track and adds it to the user's "Liked Songs" library
// (PUT /v1/me/tracks). Returns the resolved track info.
func Like(ctx context.Context, query string) (*trackInfo, error) {
	c, err := getClient(ctx)
	if err != nil {
		return nil, err
	}
	info, err := resolveTrack(ctx, query)
	if err != nil {
		return nil, err
	}
	if err := c.AddTracksToLibrary(ctx, sp.ID(info.TrackID)); err != nil {
		return nil, fmt.Errorf("like %s: %w", info.TrackID, err)
	}
	_ = CacheTrack(ctx, info, -1)
	logEvent(ctx, EventRow{
		Kind:     "library",
		TrackID:  nullable(info.TrackID),
		ArtistID: nullable(info.ArtistID),
		Source:   nullable("like"),
		Payload:  marshalPayload(map[string]any{"action": "like"}),
	})
	return info, nil
}

// Unlike resolves `query` and removes the track from the user's library
// (DELETE /v1/me/tracks).
func Unlike(ctx context.Context, query string) (*trackInfo, error) {
	c, err := getClient(ctx)
	if err != nil {
		return nil, err
	}
	info, err := resolveTrack(ctx, query)
	if err != nil {
		return nil, err
	}
	if err := c.RemoveTracksFromLibrary(ctx, sp.ID(info.TrackID)); err != nil {
		return nil, fmt.Errorf("unlike %s: %w", info.TrackID, err)
	}
	_ = CacheTrack(ctx, info, -1)
	logEvent(ctx, EventRow{
		Kind:     "library",
		TrackID:  nullable(info.TrackID),
		ArtistID: nullable(info.ArtistID),
		Source:   nullable("unlike"),
		Payload:  marshalPayload(map[string]any{"action": "unlike"}),
	})
	return info, nil
}

// resolveArtist accepts a "spotify:artist:<id>" URI directly OR runs a Search
// for SearchTypeArtist and returns the top hit's ID and display name.
func resolveArtist(ctx context.Context, query string) (id, name string, err error) {
	c, err := getClient(ctx)
	if err != nil {
		return "", "", err
	}
	if strings.HasPrefix(query, "spotify:artist:") {
		artistID := sp.ID(strings.TrimPrefix(query, "spotify:artist:"))
		a, err := c.GetArtist(ctx, artistID)
		if err != nil {
			return "", "", fmt.Errorf("get artist %s: %w", artistID, err)
		}
		return string(a.ID), a.Name, nil
	}
	if strings.HasPrefix(query, "spotify:") {
		return "", "", fmt.Errorf("only spotify:artist:... URIs supported (got %q)", query)
	}
	res, err := c.Search(ctx, query, sp.SearchTypeArtist, sp.Limit(1))
	if err != nil {
		return "", "", fmt.Errorf("search artist %q: %w", query, err)
	}
	if res.Artists == nil || len(res.Artists.Artists) == 0 {
		return "", "", fmt.Errorf("no artists found for %q", query)
	}
	a := res.Artists.Artists[0]
	return string(a.ID), a.Name, nil
}

// Follow resolves `query` to an artist and follows them
// (PUT /v1/me/following?type=artist).
func Follow(ctx context.Context, query string) (string, string, error) {
	c, err := getClient(ctx)
	if err != nil {
		return "", "", err
	}
	id, name, err := resolveArtist(ctx, query)
	if err != nil {
		return "", "", err
	}
	if err := c.FollowArtist(ctx, sp.ID(id)); err != nil {
		return "", "", fmt.Errorf("follow %s: %w", id, err)
	}
	logEvent(ctx, EventRow{
		Kind:     "library",
		ArtistID: nullable(id),
		Source:   nullable("follow"),
		Payload:  marshalPayload(map[string]any{"action": "follow"}),
	})
	return id, name, nil
}

// Unfollow resolves `query` and unfollows the artist
// (DELETE /v1/me/following?type=artist).
func Unfollow(ctx context.Context, query string) (string, string, error) {
	c, err := getClient(ctx)
	if err != nil {
		return "", "", err
	}
	id, name, err := resolveArtist(ctx, query)
	if err != nil {
		return "", "", err
	}
	if err := c.UnfollowArtist(ctx, sp.ID(id)); err != nil {
		return "", "", fmt.Errorf("unfollow %s: %w", id, err)
	}
	logEvent(ctx, EventRow{
		Kind:     "library",
		ArtistID: nullable(id),
		Source:   nullable("unfollow"),
		Payload:  marshalPayload(map[string]any{"action": "unfollow"}),
	})
	return id, name, nil
}

// CreatePlaylist creates a new playlist owned by the current user
// (POST /v1/users/{me.id}/playlists). Defaults to private; pass public=true
// to make it public. Description may be empty.
func CreatePlaylist(ctx context.Context, name, description string, public bool) (*sp.FullPlaylist, error) {
	c, err := getClient(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("playlist name must not be empty")
	}
	me, err := c.CurrentUser(ctx)
	if err != nil {
		return nil, fmt.Errorf("current user: %w", err)
	}
	pl, err := c.CreatePlaylistForUser(ctx, me.ID, name, description, public, false)
	if err != nil {
		return nil, fmt.Errorf("create playlist %q: %w", name, err)
	}
	return pl, nil
}

// myPlaylists returns every playlist owned/followed by the current user,
// across all pages. Local equivalent of the Phase 1 MyPlaylists() helper
// being added in parallel — defined here to keep this file self-contained
// and avoid merge conflicts.
func myPlaylists(ctx context.Context) ([]sp.SimplePlaylist, error) {
	c, err := getClient(ctx)
	if err != nil {
		return nil, err
	}
	var out []sp.SimplePlaylist
	page, err := c.CurrentUsersPlaylists(ctx, sp.Limit(50))
	if err != nil {
		return nil, fmt.Errorf("current users playlists: %w", err)
	}
	for {
		out = append(out, page.Playlists...)
		if err := c.NextPage(ctx, page); err != nil {
			if errors.Is(err, sp.ErrNoMorePages) {
				break
			}
			return nil, fmt.Errorf("paginate playlists: %w", err)
		}
	}
	return out, nil
}

// findPlaylist matches a single playlist by case-insensitive substring against
// names returned by myPlaylists. Errors if zero or more than one match.
func findPlaylist(ctx context.Context, name string) (*sp.SimplePlaylist, error) {
	pls, err := myPlaylists(ctx)
	if err != nil {
		return nil, err
	}
	needle := strings.ToLower(strings.TrimSpace(name))
	if needle == "" {
		return nil, errors.New("playlist name must not be empty")
	}
	var matches []sp.SimplePlaylist
	for i := range pls {
		if strings.Contains(strings.ToLower(pls[i].Name), needle) {
			matches = append(matches, pls[i])
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no playlist matching %q", name)
	case 1:
		return &matches[0], nil
	default:
		names := make([]string, len(matches))
		for i, m := range matches {
			names[i] = m.Name
		}
		return nil, fmt.Errorf("ambiguous playlist %q matches %d playlists: %v", name, len(matches), names)
	}
}

// AddToPlaylist resolves `query` to a track and adds it to the playlist
// matched by case-insensitive substring against the user's playlists
// (POST /v1/playlists/{id}/tracks).
func AddToPlaylist(ctx context.Context, playlistName, query string) (*trackInfo, *sp.SimplePlaylist, error) {
	c, err := getClient(ctx)
	if err != nil {
		return nil, nil, err
	}
	pl, err := findPlaylist(ctx, playlistName)
	if err != nil {
		return nil, nil, err
	}
	info, err := resolveTrack(ctx, query)
	if err != nil {
		return nil, nil, err
	}
	if _, err := c.AddTracksToPlaylist(ctx, pl.ID, sp.ID(info.TrackID)); err != nil {
		return nil, nil, fmt.Errorf("add %s to playlist %s: %w", info.TrackID, pl.ID, err)
	}
	_ = CacheTrack(ctx, info, -1)
	logEvent(ctx, EventRow{
		Kind:     "playlist",
		TrackID:  nullable(info.TrackID),
		ArtistID: nullable(info.ArtistID),
		Source:   nullable("playlist-add"),
		Payload:  marshalPayload(map[string]any{"playlist": pl.Name}),
	})
	return info, pl, nil
}

// RemoveFromPlaylist resolves `query` to a track and removes it from the
// playlist matched by case-insensitive substring
// (DELETE /v1/playlists/{id}/tracks).
func RemoveFromPlaylist(ctx context.Context, playlistName, query string) (*trackInfo, *sp.SimplePlaylist, error) {
	c, err := getClient(ctx)
	if err != nil {
		return nil, nil, err
	}
	pl, err := findPlaylist(ctx, playlistName)
	if err != nil {
		return nil, nil, err
	}
	info, err := resolveTrack(ctx, query)
	if err != nil {
		return nil, nil, err
	}
	if _, err := c.RemoveTracksFromPlaylist(ctx, pl.ID, sp.ID(info.TrackID)); err != nil {
		return nil, nil, fmt.Errorf("remove %s from playlist %s: %w", info.TrackID, pl.ID, err)
	}
	_ = CacheTrack(ctx, info, -1)
	logEvent(ctx, EventRow{
		Kind:     "playlist",
		TrackID:  nullable(info.TrackID),
		ArtistID: nullable(info.ArtistID),
		Source:   nullable("playlist-remove"),
		Payload:  marshalPayload(map[string]any{"playlist": pl.Name}),
	})
	return info, pl, nil
}
