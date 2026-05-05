package spot

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestInsertEventRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "spot.db")
	t.Setenv("SPOT_DB_PATH", path)

	// Reset the package-level once so this test gets a fresh DB
	// independent of any other test in the package.
	dbOnce = sync.Once{}
	dbConn = nil
	dbErr = nil

	ctx := context.Background()
	db, err := getDB(ctx)
	if err != nil {
		t.Fatalf("getDB: %v", err)
	}

	now := time.Now().Unix()
	in := EventRow{
		Kind:       "play",
		TrackID:    sql.NullString{String: "4cOdK2wGLETKBW3PvgPWqT", Valid: true},
		ArtistID:   sql.NullString{String: "0OdUWJ0sBjDrqHygGUXeCF", Valid: true},
		Zone:       sql.NullString{String: "Living Room", Valid: true},
		Source:     sql.NullString{String: "test", Valid: true},
		Payload:    sql.NullString{String: `{"q":"bonobo migration"}`, Valid: true},
		OccurredAt: now,
	}
	if err := InsertEvent(ctx, in); err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}

	var got EventRow
	row := db.QueryRowContext(ctx,
		`SELECT id, kind, track_id, artist_id, zone, source, payload, occurred_at
		 FROM events ORDER BY id DESC LIMIT 1`)
	if err := row.Scan(&got.ID, &got.Kind, &got.TrackID, &got.ArtistID,
		&got.Zone, &got.Source, &got.Payload, &got.OccurredAt); err != nil {
		t.Fatalf("scan: %v", err)
	}

	if got.ID == 0 {
		t.Errorf("expected non-zero id, got 0")
	}
	if got.Kind != in.Kind {
		t.Errorf("kind: got %q, want %q", got.Kind, in.Kind)
	}
	if got.TrackID != in.TrackID {
		t.Errorf("track_id: got %+v, want %+v", got.TrackID, in.TrackID)
	}
	if got.ArtistID != in.ArtistID {
		t.Errorf("artist_id: got %+v, want %+v", got.ArtistID, in.ArtistID)
	}
	if got.Zone != in.Zone {
		t.Errorf("zone: got %+v, want %+v", got.Zone, in.Zone)
	}
	if got.Source != in.Source {
		t.Errorf("source: got %+v, want %+v", got.Source, in.Source)
	}
	if got.Payload != in.Payload {
		t.Errorf("payload: got %+v, want %+v", got.Payload, in.Payload)
	}
	if got.OccurredAt != in.OccurredAt {
		t.Errorf("occurred_at: got %d, want %d", got.OccurredAt, in.OccurredAt)
	}
}
