package spot

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"

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
`

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
