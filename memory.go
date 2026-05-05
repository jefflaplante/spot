package spot

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Feedback records a love/hate/skip-forever verdict for a track. The
// track is resolved from `query` (free-text or "spotify:track:..." URI),
// or — when query is empty — from the currently-playing track on `zone`.
// `note` is optional free-form text. The resolved track is also written
// to the local cache so future History rows can join it.
func Feedback(ctx context.Context, query, zone, verdict, note string) (*trackInfo, error) {
	switch verdict {
	case "love", "hate", "skip-forever":
	default:
		return nil, fmt.Errorf("verdict must be love|hate|skip-forever, got %q", verdict)
	}

	info, err := resolveForFeedback(ctx, query, zone)
	if err != nil {
		return nil, err
	}

	payload := map[string]string{"verdict": verdict}
	if note != "" {
		payload["note"] = note
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode payload: %w", err)
	}

	_ = CacheTrack(ctx, info, -1)

	if err := InsertEvent(ctx, EventRow{
		Kind:       "feedback",
		TrackID:    nullable(info.TrackID),
		ArtistID:   nullable(info.ArtistID),
		Zone:       nullable(zone),
		Source:     sql.NullString{String: "cli", Valid: true},
		Payload:    sql.NullString{String: string(buf), Valid: true},
		OccurredAt: time.Now().Unix(),
	}); err != nil {
		return nil, err
	}
	return info, nil
}

// resolveForFeedback picks the track that the user means: an explicit
// query wins; otherwise it falls back to the zone's currently-playing
// track (which must be a Spotify URI on Sonos).
func resolveForFeedback(ctx context.Context, query, zone string) (*trackInfo, error) {
	if query != "" {
		return resolveTrack(ctx, query)
	}
	if zone == "" {
		return nil, fmt.Errorf("either a query or --current with a zone is required")
	}
	return currentTrackInfo(ctx, zone)
}

// Mark records a free-text note on the currently-playing track of `zone`.
// If `zone` is empty, Mark picks the first zone whose state is PLAYING.
// Returns the track that was annotated.
func Mark(ctx context.Context, zone, note string) (*trackInfo, error) {
	if note == "" {
		return nil, fmt.Errorf("note is required")
	}
	if zone == "" {
		picked, err := pickPlayingZone(ctx)
		if err != nil {
			return nil, err
		}
		zone = picked
	}
	info, err := currentTrackInfo(ctx, zone)
	if err != nil {
		return nil, err
	}

	payload := map[string]string{"note": note, "zone": zone}
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode payload: %w", err)
	}

	_ = CacheTrack(ctx, info, -1)

	if err := InsertEvent(ctx, EventRow{
		Kind:       "note",
		TrackID:    nullable(info.TrackID),
		ArtistID:   nullable(info.ArtistID),
		Zone:       sql.NullString{String: zone, Valid: true},
		Source:     sql.NullString{String: "cli", Valid: true},
		Payload:    sql.NullString{String: string(buf), Valid: true},
		OccurredAt: time.Now().Unix(),
	}); err != nil {
		return nil, err
	}
	return info, nil
}

// pickPlayingZone returns the name of the first zone reporting State="PLAYING".
// Returns an error when no zones are playing.
func pickPlayingZone(ctx context.Context) (string, error) {
	zones, err := ListSonosZones(ctx)
	if err != nil {
		return "", err
	}
	for _, z := range zones {
		state, err := getTransportInfo(ctx, z.CoordinatorIP)
		if err != nil {
			continue
		}
		if state == "PLAYING" {
			return z.Name, nil
		}
	}
	return "", fmt.Errorf("no zone is currently playing — pass --zone to pick one explicitly")
}

// currentTrackInfo returns a *trackInfo for whatever's playing on the zone.
// It resolves the Sonos URI to a Spotify track ID, then enriches via the
// Spotify Web API (or the local cache).
func currentTrackInfo(ctx context.Context, zone string) (*trackInfo, error) {
	p, err := NowPlaying(ctx, zone)
	if err != nil {
		return nil, err
	}
	id := extractSpotifyTrackID(p.URI)
	if id == "" {
		return nil, fmt.Errorf("zone %q is not playing a Spotify track (uri=%q)", zone, p.URI)
	}
	if cached, err := LookupTrack(ctx, id); err == nil && cached != nil {
		return cached, nil
	}
	info, err := resolveTrack(ctx, "spotify:track:"+id)
	if err != nil {
		return nil, err
	}
	_ = CacheTrack(ctx, info, -1)
	return info, nil
}

// HistoryFilter narrows down the rows returned by History.
type HistoryFilter struct {
	Zone   string
	Artist string        // name substring or "spotify:artist:<id>"
	Kind   string        // event kind; "" matches all
	Since  time.Duration // 0 means default 24h
	Limit  int           // 0 means default 50
}

// HistoryRow is a single annotated event.
type HistoryRow struct {
	ID         int64
	Kind       string
	Zone       string
	Source     string
	OccurredAt time.Time
	Track      *trackInfo
}

// History returns recent rows from the events table, joined to the local
// tracks cache so each row carries title/artist/album where available.
// Newest first.
func History(ctx context.Context, f HistoryFilter) ([]HistoryRow, error) {
	db, err := getDB(ctx)
	if err != nil {
		return nil, err
	}

	since := f.Since
	if since == 0 {
		since = 24 * time.Hour
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}

	q := strings.Builder{}
	q.WriteString(`
		SELECT e.id, e.kind, e.track_id, e.artist_id, e.zone, e.source, e.occurred_at,
		       t.id, t.title, t.artist_id, t.artist_name, t.album
		  FROM events e
		  LEFT JOIN tracks t ON t.id = e.track_id
		 WHERE e.occurred_at >= ?
	`)
	args := []any{time.Now().Add(-since).Unix()}

	if f.Kind != "" {
		q.WriteString(" AND e.kind = ?")
		args = append(args, f.Kind)
	}
	if f.Zone != "" {
		q.WriteString(" AND e.zone = ?")
		args = append(args, f.Zone)
	}
	if f.Artist != "" {
		if strings.HasPrefix(f.Artist, "spotify:artist:") {
			id := strings.TrimPrefix(f.Artist, "spotify:artist:")
			q.WriteString(" AND (e.artist_id = ? OR t.artist_id = ?)")
			args = append(args, id, id)
		} else {
			q.WriteString(" AND t.artist_name LIKE ?")
			args = append(args, "%"+f.Artist+"%")
		}
	}
	q.WriteString(" ORDER BY e.occurred_at DESC, e.id DESC LIMIT ?")
	args = append(args, limit)

	rows, err := db.QueryContext(ctx, q.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("query history: %w", err)
	}
	defer rows.Close()

	out := make([]HistoryRow, 0, limit)
	for rows.Next() {
		var (
			r                                     HistoryRow
			trackID, artistID, zone, source       sql.NullString
			occurred                              int64
			cachedID, title, cArtistID, cArtist, cAlbum sql.NullString
		)
		if err := rows.Scan(&r.ID, &r.Kind, &trackID, &artistID, &zone, &source, &occurred,
			&cachedID, &title, &cArtistID, &cArtist, &cAlbum); err != nil {
			return nil, fmt.Errorf("scan history: %w", err)
		}
		r.Zone = zone.String
		r.Source = source.String
		r.OccurredAt = time.Unix(occurred, 0)
		if cachedID.Valid && cachedID.String != "" {
			r.Track = &trackInfo{
				TrackID:  cachedID.String,
				Title:    title.String,
				ArtistID: cArtistID.String,
				Artist:   cArtist.String,
				Album:    cAlbum.String,
			}
		} else if trackID.Valid && trackID.String != "" {
			// No cache hit — return a stub so callers see the ID.
			r.Track = &trackInfo{TrackID: trackID.String, ArtistID: artistID.String}
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate history: %w", err)
	}
	return out, nil
}

// TrackStat is a per-track aggregation for the stats view.
type TrackStat struct {
	TrackID string `json:"track_id"`
	Title   string `json:"title,omitempty"`
	Artist  string `json:"artist,omitempty"`
	Count   int    `json:"count"`
}

// ArtistStat is a per-artist aggregation for the stats view.
type ArtistStat struct {
	ArtistID string `json:"artist_id"`
	Name     string `json:"name,omitempty"`
	Count    int    `json:"count"`
}

// Stats bundles the various aggregations a single ComputeStats call returns.
type Stats struct {
	TopTracks  []TrackStat  `json:"top_tracks,omitempty"`
	TopArtists []ArtistStat `json:"top_artists,omitempty"`
	Skipped    []TrackStat  `json:"skipped,omitempty"`
}

// ComputeStats runs the aggregation requested by `mode`:
//
//	"top"     — most-played tracks/artists from kind='play_started' (all time)
//	"recent"  — same, restricted to the last 7 days
//	"skipped" — tracks with the most kind='skip' events in the last 7 days
//
// `n` is the result limit. `play_started` events are not yet emitted by spot
// so the top/recent queries return empty lists today; that's fine.
func ComputeStats(ctx context.Context, mode string, n int) (*Stats, error) {
	if n <= 0 {
		n = 10
	}
	db, err := getDB(ctx)
	if err != nil {
		return nil, err
	}
	out := &Stats{}
	switch mode {
	case "", "top":
		ts, as, err := topPlayed(ctx, db, n, 0)
		if err != nil {
			return nil, err
		}
		out.TopTracks, out.TopArtists = ts, as
	case "recent":
		ts, as, err := topPlayed(ctx, db, n, 7*24*time.Hour)
		if err != nil {
			return nil, err
		}
		out.TopTracks, out.TopArtists = ts, as
	case "skipped":
		ts, err := topSkipped(ctx, db, n, 7*24*time.Hour)
		if err != nil {
			return nil, err
		}
		out.Skipped = ts
	default:
		return nil, fmt.Errorf("unknown stats mode %q (expected top|recent|skipped)", mode)
	}
	return out, nil
}

func topPlayed(ctx context.Context, db *sql.DB, n int, since time.Duration) ([]TrackStat, []ArtistStat, error) {
	var (
		whereTime string
		args      []any
	)
	if since > 0 {
		whereTime = " AND e.occurred_at >= ?"
		args = append(args, time.Now().Add(-since).Unix())
	}

	trackArgs := append([]any(nil), args...)
	trackArgs = append(trackArgs, n)
	trackQ := `
		SELECT e.track_id, COALESCE(t.title,''), COALESCE(t.artist_name,''), COUNT(*) AS c
		  FROM events e
		  LEFT JOIN tracks t ON t.id = e.track_id
		 WHERE e.kind = 'play_started' AND e.track_id IS NOT NULL` + whereTime + `
		 GROUP BY e.track_id
		 ORDER BY c DESC
		 LIMIT ?`
	trows, err := db.QueryContext(ctx, trackQ, trackArgs...)
	if err != nil {
		return nil, nil, fmt.Errorf("top tracks: %w", err)
	}
	defer trows.Close()
	var tracks []TrackStat
	for trows.Next() {
		var s TrackStat
		if err := trows.Scan(&s.TrackID, &s.Title, &s.Artist, &s.Count); err != nil {
			return nil, nil, fmt.Errorf("scan top tracks: %w", err)
		}
		tracks = append(tracks, s)
	}
	if err := trows.Err(); err != nil {
		return nil, nil, err
	}

	artistArgs := append([]any(nil), args...)
	artistArgs = append(artistArgs, n)
	artistQ := `
		SELECT COALESCE(e.artist_id, t.artist_id, '') AS aid,
		       COALESCE(t.artist_name, '')           AS name,
		       COUNT(*)                              AS c
		  FROM events e
		  LEFT JOIN tracks t ON t.id = e.track_id
		 WHERE e.kind = 'play_started'` + whereTime + `
		 GROUP BY aid
		 HAVING aid != ''
		 ORDER BY c DESC
		 LIMIT ?`
	arows, err := db.QueryContext(ctx, artistQ, artistArgs...)
	if err != nil {
		return nil, nil, fmt.Errorf("top artists: %w", err)
	}
	defer arows.Close()
	var artists []ArtistStat
	for arows.Next() {
		var s ArtistStat
		if err := arows.Scan(&s.ArtistID, &s.Name, &s.Count); err != nil {
			return nil, nil, fmt.Errorf("scan top artists: %w", err)
		}
		artists = append(artists, s)
	}
	if err := arows.Err(); err != nil {
		return nil, nil, err
	}
	return tracks, artists, nil
}

func topSkipped(ctx context.Context, db *sql.DB, n int, since time.Duration) ([]TrackStat, error) {
	cutoff := time.Now().Add(-since).Unix()
	q := `
		SELECT e.track_id, COALESCE(t.title,''), COALESCE(t.artist_name,''), COUNT(*) AS c
		  FROM events e
		  LEFT JOIN tracks t ON t.id = e.track_id
		 WHERE e.kind = 'skip' AND e.track_id IS NOT NULL AND e.occurred_at >= ?
		 GROUP BY e.track_id
		 ORDER BY c DESC
		 LIMIT ?`
	rows, err := db.QueryContext(ctx, q, cutoff, n)
	if err != nil {
		return nil, fmt.Errorf("top skipped: %w", err)
	}
	defer rows.Close()
	var out []TrackStat
	for rows.Next() {
		var s TrackStat
		if err := rows.Scan(&s.TrackID, &s.Title, &s.Artist, &s.Count); err != nil {
			return nil, fmt.Errorf("scan top skipped: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// nullable is a small helper for converting "" → NULL, "x" → "x".
func nullable(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
