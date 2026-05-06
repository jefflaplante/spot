// Package spotify is a minimal Spotify Web API helper for an agent harness
// that wants to play tracks on Sonos (or any Spotify Connect device).
//
// One-time setup, on a machine with a browser (or via SSH port-forward
// `ssh -L 8888:127.0.0.1:8888 user@vm`):
//
//	spot.Authenticate(ctx)
//
// Then from the harness, as many times as you like:
//
//	spot.Play(ctx, "bonobo migration", "Living Room")
//	spot.Play(ctx, "spotify:track:4cOdK2wGLETKBW3PvgPWqT", "Kitchen")
//
// Env vars:
//
//	SPOTIFY_ID            (required)
//	SPOTIFY_SECRET        (required)
//	SPOTIFY_REDIRECT_URI  (default http://127.0.0.1:8888/callback)
//	SPOTIFY_TOKEN_PATH    (default ./.spotify-cache)
package spot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	mrand "math/rand/v2"
	"net/http"
	"os"
	"strings"
	"sync"

	sp "github.com/zmb3/spotify/v2"
	spotifyauth "github.com/zmb3/spotify/v2/auth"
	"golang.org/x/oauth2"
)

const (
	defaultRedirectURI = "http://127.0.0.1:8888/callback"
	defaultTokenPath   = ".spotify-cache"
)

var (
	clientOnce   sync.Once
	cachedClient *sp.Client
	cachedErr    error
)

// spotifyScopes is the single source of truth for OAuth scopes requested
// by both newAuthenticator (bootstrap) and newOAuth2Config (refresh). If
// the two diverge, token refresh silently downgrades scopes — keep them
// reading from this slice.
var spotifyScopes = []string{
	// Playback (existing).
	spotifyauth.ScopeUserReadPlaybackState,
	spotifyauth.ScopeUserModifyPlaybackState,
	// Phase 1: read-only history/library.
	spotifyauth.ScopeUserTopRead,
	spotifyauth.ScopeUserReadRecentlyPlayed,
	spotifyauth.ScopeUserLibraryRead,
	// Phase 2: library/follow/playlist mutation.
	spotifyauth.ScopeUserLibraryModify,
	spotifyauth.ScopeUserFollowModify,
	spotifyauth.ScopePlaylistModifyPrivate,
	spotifyauth.ScopePlaylistModifyPublic,
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func newAuthenticator() *spotifyauth.Authenticator {
	return spotifyauth.New(
		spotifyauth.WithRedirectURL(env("SPOTIFY_REDIRECT_URI", defaultRedirectURI)),
		spotifyauth.WithScopes(spotifyScopes...),
	)
}

func newOAuth2Config() *oauth2.Config {
	return &oauth2.Config{
		ClientID:     os.Getenv("SPOTIFY_ID"),
		ClientSecret: os.Getenv("SPOTIFY_SECRET"),
		RedirectURL:  env("SPOTIFY_REDIRECT_URI", defaultRedirectURI),
		Scopes:       append([]string(nil), spotifyScopes...),
		Endpoint: oauth2.Endpoint{
			AuthURL:  spotifyauth.AuthURL,
			TokenURL: spotifyauth.TokenURL,
		},
	}
}

// persistingSource writes refreshed tokens back to disk so the harness
// survives restarts after a refresh.
type persistingSource struct {
	src  oauth2.TokenSource
	path string
	mu   sync.Mutex
}

func (p *persistingSource) Token() (*oauth2.Token, error) {
	tok, err := p.src.Token()
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if f, err := os.OpenFile(p.path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600); err == nil {
		_ = json.NewEncoder(f).Encode(tok)
		_ = f.Close()
	}
	return tok, nil
}

func loadClient(ctx context.Context) (*sp.Client, error) {
	path := env("SPOTIFY_TOKEN_PATH", defaultTokenPath)
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open token cache %s: %w (run Authenticate first)", path, err)
	}
	defer f.Close()

	var tok oauth2.Token
	if err := json.NewDecoder(f).Decode(&tok); err != nil {
		return nil, fmt.Errorf("decode token: %w", err)
	}

	cfg := newOAuth2Config()
	base := cfg.TokenSource(ctx, &tok)
	src := oauth2.ReuseTokenSource(&tok, &persistingSource{src: base, path: path})
	return sp.New(oauth2.NewClient(ctx, src)), nil
}

func getClient(ctx context.Context) (*sp.Client, error) {
	clientOnce.Do(func() {
		cachedClient, cachedErr = loadClient(ctx)
	})
	return cachedClient, cachedErr
}

// Authenticate runs the one-time OAuth flow and writes the token cache.
func Authenticate(ctx context.Context) error {
	if os.Getenv("SPOTIFY_ID") == "" || os.Getenv("SPOTIFY_SECRET") == "" {
		return fmt.Errorf("SPOTIFY_ID and SPOTIFY_SECRET must be set")
	}

	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return fmt.Errorf("random state: %w", err)
	}
	state := hex.EncodeToString(stateBytes)

	auth := newAuthenticator()
	tokens := make(chan *oauth2.Token, 1)
	errs := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		tok, err := auth.Token(r.Context(), state, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			errs <- err
			return
		}
		fmt.Fprintln(w, "Authed. You can close this tab.")
		tokens <- tok
	})

	srv := &http.Server{Addr: "127.0.0.1:8888", Handler: mux}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errs <- err
		}
	}()
	defer srv.Shutdown(context.Background())

	fmt.Println("Open this URL in a browser:")
	fmt.Println(auth.AuthURL(state))

	select {
	case tok := <-tokens:
		path := env("SPOTIFY_TOKEN_PATH", defaultTokenPath)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
		if err != nil {
			return fmt.Errorf("create token cache: %w", err)
		}
		if err := json.NewEncoder(f).Encode(tok); err != nil {
			_ = f.Close()
			return fmt.Errorf("write token cache: %w", err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("close token cache: %w", err)
		}
		client := sp.New(auth.Client(ctx, tok))
		user, err := client.CurrentUser(ctx)
		if err != nil {
			return fmt.Errorf("verify auth: %w", err)
		}
		fmt.Println("authed as:", user.DisplayName)
		return nil
	case err := <-errs:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Devices returns the playback devices currently visible to Spotify for the
// authenticated user.
func Devices(ctx context.Context) ([]sp.PlayerDevice, error) {
	c, err := getClient(ctx)
	if err != nil {
		return nil, err
	}
	return c.PlayerDevices(ctx)
}

// PlayOption configures Play / PlayViaSonos behavior. Pass options after the
// required positional args, e.g. spot.Play(ctx, "x", "y", spot.WithContinue()).
type PlayOption func(*playOpts)

type playOpts struct {
	continueAfter bool
	shuffle       bool
}

// WithContinue tells Play (or PlayViaSonos) to keep playing related music
// after the seed track ends. The current implementation queues the primary
// artist's top tracks. Without this option, a single track is played.
func WithContinue() PlayOption {
	return func(o *playOpts) { o.continueAfter = true }
}

// WithShuffle adds entropy to the continuation queue: the candidate pool
// is broadened (artist top tracks plus a random sample of album cuts) and
// shuffled, so repeated `play -c -s "<artist>"` invocations produce
// different track sets. Has no effect without WithContinue. The seed track
// still plays first.
func WithShuffle() PlayOption {
	return func(o *playOpts) { o.shuffle = true }
}

// trackInfo is a seed track resolved from a query. Carries the metadata
// Sonos needs in DIDL-Lite (Title/Artist/Album) so that queue browsing
// shows useful labels rather than blank rows.
type trackInfo struct {
	TrackID  string
	ArtistID string // primary artist; needed for continuation
	Title    string
	Artist   string // primary artist's name
	Album    string
}

func makeTrackInfo(t *sp.FullTrack) *trackInfo {
	info := &trackInfo{
		TrackID: strings.TrimPrefix(string(t.URI), "spotify:track:"),
		Title:   t.Name,
		Album:   t.Album.Name,
	}
	if len(t.Artists) > 0 {
		info.ArtistID = string(t.Artists[0].ID)
		info.Artist = t.Artists[0].Name
	}
	return info
}

// resolveTrack searches Spotify for `query` and returns the track plus
// enough metadata for Sonos DIDL.
//
// Resolution order:
//  1. spotify:track:<id> — direct GetTrack.
//  2. spotify:artist:<id> — top track of that artist becomes the seed.
//  3. Free text whose normalized form equals a top artist-search hit:
//     resolved as that artist's #1 top track. This is the fix for the
//     "play nine inch nails" → Linkin Park bug — track-search top hits
//     for bare artist names can be covers, remixes, or unrelated tracks
//     whose metadata happens to mention the artist. Artist search is
//     reliable when the query unambiguously names an artist.
//  4. Otherwise, track search (the original behavior).
func resolveTrack(ctx context.Context, query string) (*trackInfo, error) {
	c, err := getClient(ctx)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(query, "spotify:track:") {
		id := sp.ID(strings.TrimPrefix(query, "spotify:track:"))
		t, err := c.GetTrack(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("get track %s: %w", id, err)
		}
		return makeTrackInfo(t), nil
	}
	if strings.HasPrefix(query, "spotify:artist:") {
		id := strings.TrimPrefix(query, "spotify:artist:")
		return artistTopAsSeed(ctx, id)
	}
	if strings.HasPrefix(query, "spotify:") {
		return nil, fmt.Errorf("only spotify:track:... or spotify:artist:... URIs supported (got %q)", query)
	}

	// Try artist resolution first. Conservative: only switches modes
	// when the query, normalized, exactly equals a top artist-search
	// result's name. Anything ambiguous falls through to track search.
	if info, ok := tryArtistResolution(ctx, query); ok {
		return info, nil
	}

	res, err := c.Search(ctx, query, sp.SearchTypeTrack, sp.Limit(1))
	if err != nil {
		return nil, fmt.Errorf("search %q: %w", query, err)
	}
	if res.Tracks == nil || len(res.Tracks.Tracks) == 0 {
		return nil, fmt.Errorf("no tracks found for %q", query)
	}
	t := res.Tracks.Tracks[0]
	return makeTrackInfo(&t), nil
}

// artistTopAsSeed fetches the artist's #1 top track and returns it as a
// trackInfo. Used when the query is a spotify:artist: URI or a bare
// artist name that matched via tryArtistResolution.
func artistTopAsSeed(ctx context.Context, artistID string) (*trackInfo, error) {
	c, err := getClient(ctx)
	if err != nil {
		return nil, err
	}
	tops, err := c.GetArtistsTopTracks(ctx, sp.ID(artistID), "US")
	if err != nil {
		return nil, fmt.Errorf("artist top tracks: %w", err)
	}
	if len(tops) == 0 {
		return nil, fmt.Errorf("artist %s has no top tracks (try a track or album query)", artistID)
	}
	return makeTrackInfo(&tops[0]), nil
}

// tryArtistResolution checks whether `query` unambiguously names a
// Spotify artist; if so, returns that artist's #1 top track as the seed.
// Returns (nil, false) for ambiguous queries — caller should fall back
// to track search.
func tryArtistResolution(ctx context.Context, query string) (*trackInfo, bool) {
	c, err := getClient(ctx)
	if err != nil {
		return nil, false
	}
	res, err := c.Search(ctx, query, sp.SearchTypeArtist, sp.Limit(3))
	if err != nil || res.Artists == nil {
		return nil, false
	}
	for _, a := range res.Artists.Artists {
		if !matchArtistName(query, a.Name) {
			continue
		}
		tops, err := c.GetArtistsTopTracks(ctx, a.ID, "US")
		if err != nil || len(tops) == 0 {
			return nil, false
		}
		return makeTrackInfo(&tops[0]), true
	}
	return nil, false
}

// matchArtistName decides whether a free-text query is the kind of
// close match to an artist's name that should switch resolveTrack into
// artist mode. Conservative: requires case-insensitive equality after
// trimming and collapsing internal whitespace. Anything looser risks
// false positives ("nine" matching multiple "Nine ..." artists).
func matchArtistName(query, name string) bool {
	norm := func(s string) string {
		return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(s))), " ")
	}
	return norm(query) != "" && norm(query) == norm(name)
}

// lookupSpotifyTracks batch-fetches track metadata for a list of Spotify
// track IDs and returns a map keyed by ID. Used to enrich Sonos queue
// listings, where Sonos discards the metadata we send and only stores
// stub entries derived from the URI.
//
// Spotify's /v1/tracks endpoint accepts up to 50 IDs per call; this
// function chunks larger lists.
func lookupSpotifyTracks(ctx context.Context, ids []string) (map[string]*trackInfo, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	c, err := getClient(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*trackInfo, len(ids))
	for i := 0; i < len(ids); i += 50 {
		end := i + 50
		if end > len(ids) {
			end = len(ids)
		}
		chunk := make([]sp.ID, end-i)
		for j, id := range ids[i:end] {
			chunk[j] = sp.ID(id)
		}
		tracks, err := c.GetTracks(ctx, chunk)
		if err != nil {
			return nil, err
		}
		for _, t := range tracks {
			if t == nil {
				continue
			}
			info := makeTrackInfo(t)
			out[info.TrackID] = info
		}
	}
	return out, nil
}

// artistTopTracks returns the primary artist's top tracks (full metadata),
// excluding `excludeID`. Used for continuation.
func artistTopTracks(ctx context.Context, artistID, excludeID string) ([]*trackInfo, error) {
	c, err := getClient(ctx)
	if err != nil {
		return nil, err
	}
	tracks, err := c.GetArtistsTopTracks(ctx, sp.ID(artistID), "US")
	if err != nil {
		return nil, fmt.Errorf("artist top tracks: %w", err)
	}
	out := make([]*trackInfo, 0, len(tracks))
	for i := range tracks {
		info := makeTrackInfo(&tracks[i])
		if info.TrackID != excludeID {
			out = append(out, info)
		}
	}
	return out, nil
}

// gatherContinuationTracks builds the list of tracks to queue after the
// seed track when WithContinue is set. Without shuffle, it returns the
// artist's top tracks in API-default order (deterministic). With shuffle,
// it broadens the pool with random samples from the artist's recent
// albums, dedupes, and shuffles, so repeated invocations vary.
func gatherContinuationTracks(ctx context.Context, info *trackInfo, shuffle bool) ([]*trackInfo, error) {
	top, err := artistTopTracks(ctx, info.ArtistID, info.TrackID)
	if err != nil {
		return nil, err
	}
	if !shuffle {
		return top, nil
	}

	// Best-effort: extra album cuts. If the API call fails, fall back to
	// just shuffling the top tracks.
	extra, _ := artistRandomAlbumCuts(ctx, info.ArtistID, info.TrackID, 5)

	pool := append([]*trackInfo{}, top...)
	seen := make(map[string]bool, len(pool))
	for _, t := range pool {
		seen[t.TrackID] = true
	}
	for _, t := range extra {
		if !seen[t.TrackID] {
			pool = append(pool, t)
			seen[t.TrackID] = true
		}
	}
	mrand.Shuffle(len(pool), func(i, j int) {
		pool[i], pool[j] = pool[j], pool[i]
	})
	return pool, nil
}

// artistRandomAlbumCuts returns one random track from each of up to N of
// the artist's albums (full-length only — singles excluded). Used to
// broaden the candidate pool for play -c -s beyond just top tracks.
func artistRandomAlbumCuts(ctx context.Context, artistID, excludeID string, n int) ([]*trackInfo, error) {
	c, err := getClient(ctx)
	if err != nil {
		return nil, err
	}
	page, err := c.GetArtistAlbums(ctx, sp.ID(artistID), []sp.AlbumType{sp.AlbumTypeAlbum})
	if err != nil {
		return nil, err
	}
	if page == nil || len(page.Albums) == 0 {
		return nil, nil
	}
	if n > len(page.Albums) {
		n = len(page.Albums)
	}
	out := make([]*trackInfo, 0, n)
	for i := 0; i < n; i++ {
		album := page.Albums[i]
		full, err := c.GetAlbum(ctx, album.ID)
		if err != nil || full == nil || len(full.Tracks.Tracks) == 0 {
			continue
		}
		tr := full.Tracks.Tracks[mrand.IntN(len(full.Tracks.Tracks))]
		info := &trackInfo{
			TrackID:  string(tr.ID),
			ArtistID: artistID,
			Title:    tr.Name,
			Album:    full.Name,
		}
		if len(tr.Artists) > 0 {
			info.Artist = tr.Artists[0].Name
		}
		if info.TrackID != "" && info.TrackID != excludeID {
			out = append(out, info)
		}
	}
	return out, nil
}

// Play searches for `query` (free-text or "spotify:track:..." URI) and starts
// playback on a device whose name contains `room` (case-insensitive substring).
//
// Pass WithContinue() to also queue the artist's top tracks after the seed.
//
// If the device is listed but inactive, Play transfers playback to wake it.
// If the device isn't listed at all, the speaker has dropped out of Spotify
// Connect — there's no Web API fix; nudge it via the Sonos or Spotify app.
func Play(ctx context.Context, query, room string, opts ...PlayOption) error {
	var o playOpts
	for _, fn := range opts {
		fn(&o)
	}

	c, err := getClient(ctx)
	if err != nil {
		return err
	}

	info, err := resolveTrack(ctx, query)
	if err != nil {
		return err
	}
	uris := []sp.URI{sp.URI("spotify:track:" + info.TrackID)}
	if o.continueAfter {
		more, err := gatherContinuationTracks(ctx, info, o.shuffle)
		if err != nil {
			return err
		}
		for _, t := range more {
			uris = append(uris, sp.URI("spotify:track:"+t.TrackID))
		}
	}

	devs, err := c.PlayerDevices(ctx)
	if err != nil {
		return fmt.Errorf("list devices: %w", err)
	}

	needle := strings.ToLower(room)
	var dev *sp.PlayerDevice
	for i := range devs {
		if strings.Contains(strings.ToLower(devs[i].Name), needle) {
			dev = &devs[i]
			break
		}
	}
	if dev == nil {
		names := make([]string, len(devs))
		for i, d := range devs {
			names[i] = d.Name
		}
		return fmt.Errorf(
			"no device matching %q; visible to Spotify: %v\n"+
				"  Sonos only registers as a Spotify Connect device while the\n"+
				"  Spotify app (not the Sonos app) is actively driving it. Open\n"+
				"  the Spotify app on any device, tap the Connect/devices icon,\n"+
				"  pick the Sonos zone, and play any track — then retry.",
			room, names)
	}

	devID := sp.ID(dev.ID)
	if !dev.Active {
		if err := c.TransferPlayback(ctx, devID, false); err != nil {
			return fmt.Errorf("transfer to %s: %w", dev.Name, err)
		}
	}

	if err := c.PlayOpt(ctx, &sp.PlayOptions{
		DeviceID: &devID,
		URIs:     uris,
	}); err != nil {
		return fmt.Errorf("play on %s: %w", dev.Name, err)
	}

	// Best-effort: cache the seed track and log a play_started event.
	_ = CacheTrack(ctx, info, -1)
	logEvent(ctx, EventRow{
		Kind:     "play_started",
		TrackID:  nullable(info.TrackID),
		ArtistID: nullable(info.ArtistID),
		Zone:     nullable(dev.Name),
		Source:   nullable("play"),
		Payload: marshalPayload(map[string]any{
			"transport":    "connect",
			"continuation": o.continueAfter,
		}),
	})
	return nil
}
