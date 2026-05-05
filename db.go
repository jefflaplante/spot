package spot

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// EventRow is a single row in the events table. Nullable columns use
// sql.NullString so callers can distinguish "absent" from empty string.
type EventRow struct {
	ID         int64
	Kind       string
	TrackID    sql.NullString
	ArtistID   sql.NullString
	Zone       sql.NullString
	Source     sql.NullString
	Payload    sql.NullString
	OccurredAt int64
}

const schema = `
CREATE TABLE IF NOT EXISTS events (
  id          INTEGER PRIMARY KEY,
  kind        TEXT NOT NULL,
  track_id    TEXT,
  artist_id   TEXT,
  zone        TEXT,
  source      TEXT,
  payload     TEXT,
  occurred_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS events_track_time ON events(track_id, occurred_at);
CREATE INDEX IF NOT EXISTS events_kind_time  ON events(kind, occurred_at);

CREATE TABLE IF NOT EXISTS tracks (
  id           TEXT PRIMARY KEY,
  title        TEXT,
  artist_id    TEXT,
  artist_name  TEXT,
  album        TEXT,
  popularity   INTEGER,
  fetched_at   INTEGER NOT NULL
);
`

// trackCacheTTL controls how long a cached row in `tracks` is considered
// fresh before LookupTrack falls through to a Spotify refetch.
const trackCacheTTL = 30 * 24 * time.Hour

var (
	dbOnce sync.Once
	dbConn *sql.DB
	dbErr  error
)

// dbPath returns the resolved on-disk location for spot.db. If SPOT_DB_PATH
// is set, it wins. Otherwise the file is co-located with the OAuth token
// cache directory (SPOTIFY_TOKEN_PATH, defaulting to ./.spotify-cache, so
// spot.db lands next to it in the cwd).
func dbPath() string {
	if p := os.Getenv("SPOT_DB_PATH"); p != "" {
		return p
	}
	tokenPath := env("SPOTIFY_TOKEN_PATH", defaultTokenPath)
	dir := filepath.Dir(tokenPath)
	return filepath.Join(dir, "spot.db")
}

// getDB lazily opens spot.db, runs migrations, and chmods it to 0600. The
// connection is cached for the life of the process; subsequent callers get
// the same handle.
func getDB(ctx context.Context) (*sql.DB, error) {
	dbOnce.Do(func() {
		path := dbPath()
		db, err := sql.Open("sqlite", path)
		if err != nil {
			dbErr = fmt.Errorf("open sqlite %s: %w", path, err)
			return
		}
		if _, err := db.ExecContext(ctx, schema); err != nil {
			_ = db.Close()
			dbErr = fmt.Errorf("migrate %s: %w", path, err)
			return
		}
		if err := os.Chmod(path, 0600); err != nil {
			_ = db.Close()
			dbErr = fmt.Errorf("chmod %s: %w", path, err)
			return
		}
		dbConn = db
	})
	return dbConn, dbErr
}

// InsertEvent appends a single row to the events table.
func InsertEvent(ctx context.Context, e EventRow) error {
	db, err := getDB(ctx)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO events (kind, track_id, artist_id, zone, source, payload, occurred_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.Kind, e.TrackID, e.ArtistID, e.Zone, e.Source, e.Payload, e.OccurredAt,
	)
	if err != nil {
		return fmt.Errorf("insert event: %w", err)
	}
	return nil
}

// CacheTrack upserts a row in the local tracks cache. Popularity is the
// Spotify popularity score (0-100) at fetch time; pass -1 (or any negative
// value) when the caller doesn't have it.
func CacheTrack(ctx context.Context, t *trackInfo, popularity int) error {
	if t == nil || t.TrackID == "" {
		return nil
	}
	db, err := getDB(ctx)
	if err != nil {
		return err
	}
	var pop sql.NullInt64
	if popularity >= 0 {
		pop = sql.NullInt64{Int64: int64(popularity), Valid: true}
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO tracks (id, title, artist_id, artist_name, album, popularity, fetched_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   title       = excluded.title,
		   artist_id   = excluded.artist_id,
		   artist_name = excluded.artist_name,
		   album       = excluded.album,
		   popularity  = excluded.popularity,
		   fetched_at  = excluded.fetched_at`,
		t.TrackID, t.Title, t.ArtistID, t.Artist, t.Album, pop, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("cache track %s: %w", t.TrackID, err)
	}
	return nil
}

// LookupTrack returns the cached metadata for a Spotify track ID. Returns
// (nil, nil) when the ID isn't cached. If the cache row is older than
// trackCacheTTL, LookupTrack refetches from Spotify and refreshes the row
// before returning. A refetch failure on a stale row is logged via the
// error path; callers are expected to fall back to resolveTrack themselves.
func LookupTrack(ctx context.Context, id string) (*trackInfo, error) {
	if id == "" {
		return nil, nil
	}
	db, err := getDB(ctx)
	if err != nil {
		return nil, err
	}
	var (
		title, artistID, artistName, album sql.NullString
		fetchedAt                          int64
	)
	err = db.QueryRowContext(ctx,
		`SELECT title, artist_id, artist_name, album, fetched_at
		   FROM tracks WHERE id = ?`,
		id,
	).Scan(&title, &artistID, &artistName, &album, &fetchedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup track %s: %w", id, err)
	}

	if time.Since(time.Unix(fetchedAt, 0)) > trackCacheTTL {
		// Stale: refresh via Spotify. Best-effort — if the refetch
		// fails (offline, auth missing, etc.) return the stale row.
		if fresh, err := resolveTrack(ctx, "spotify:track:"+id); err == nil && fresh != nil {
			_ = CacheTrack(ctx, fresh, -1)
			return fresh, nil
		}
	}
	return &trackInfo{
		TrackID:  id,
		Title:    title.String,
		ArtistID: artistID.String,
		Artist:   artistName.String,
		Album:    album.String,
	}, nil
}
