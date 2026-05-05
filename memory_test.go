package spot

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// resetDB rewinds the package-level sync.Once so the next getDB opens a
// fresh database against the test's SPOT_DB_PATH.
func resetDB(t *testing.T) {
	t.Helper()
	dbOnce = sync.Once{}
	dbConn = nil
	dbErr = nil
}

func setupTempDB(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("SPOT_DB_PATH", filepath.Join(tmp, "spot.db"))
	resetDB(t)
}

func TestCacheTrackLookupRoundTrip(t *testing.T) {
	setupTempDB(t)

	ctx := context.Background()
	want := &trackInfo{
		TrackID:  "4cOdK2wGLETKBW3PvgPWqT",
		Title:    "Migration",
		ArtistID: "0OdUWJ0sBjDrqHygGUXeCF",
		Artist:   "Bonobo",
		Album:    "Migration",
	}
	if err := CacheTrack(ctx, want, 73); err != nil {
		t.Fatalf("CacheTrack: %v", err)
	}

	got, err := LookupTrack(ctx, want.TrackID)
	if err != nil {
		t.Fatalf("LookupTrack: %v", err)
	}
	if got == nil {
		t.Fatalf("LookupTrack returned nil for cached track")
	}
	if got.Title != want.Title || got.Artist != want.Artist ||
		got.Album != want.Album || got.ArtistID != want.ArtistID {
		t.Errorf("round-trip mismatch:\ngot  %+v\nwant %+v", got, want)
	}

	// Miss returns (nil, nil).
	miss, err := LookupTrack(ctx, "doesnotexist")
	if err != nil {
		t.Fatalf("LookupTrack miss: %v", err)
	}
	if miss != nil {
		t.Errorf("expected nil for cache miss, got %+v", miss)
	}
}

// TestLookupTrackStaleRefetch fakes a >30-day-old fetched_at and confirms
// LookupTrack tries to refresh. With no Spotify creds available in tests
// the refresh fails silently and we get the stale row back — the assertion
// here is just that we still get a non-nil result with the original fields.
// (Full refresh-success paths require live Spotify auth, which the test
// environment doesn't have.)
func TestLookupTrackStaleRefetch(t *testing.T) {
	setupTempDB(t)

	ctx := context.Background()
	want := &trackInfo{
		TrackID:  "stale123",
		Title:    "Old Hit",
		ArtistID: "artist123",
		Artist:   "Old Band",
		Album:    "Old Album",
	}
	if err := CacheTrack(ctx, want, -1); err != nil {
		t.Fatalf("CacheTrack: %v", err)
	}

	// Backdate fetched_at by 60 days.
	db, err := getDB(ctx)
	if err != nil {
		t.Fatalf("getDB: %v", err)
	}
	old := time.Now().Add(-60 * 24 * time.Hour).Unix()
	if _, err := db.ExecContext(ctx, `UPDATE tracks SET fetched_at = ? WHERE id = ?`, old, want.TrackID); err != nil {
		t.Fatalf("backdate fetched_at: %v", err)
	}

	got, err := LookupTrack(ctx, want.TrackID)
	if err != nil {
		t.Fatalf("LookupTrack stale: %v", err)
	}
	if got == nil {
		t.Fatalf("expected stale row to fall back to cached copy when refresh fails")
	}
	if got.Title != want.Title {
		t.Errorf("title: got %q, want %q", got.Title, want.Title)
	}
}

func TestHistoryFilterAndOrder(t *testing.T) {
	setupTempDB(t)

	ctx := context.Background()
	now := time.Now().Unix()

	// Cache one track so the JOIN populates the title.
	if err := CacheTrack(ctx, &trackInfo{
		TrackID: "track-A",
		Title:   "Alpha",
		Artist:  "Artist One",
	}, -1); err != nil {
		t.Fatalf("CacheTrack: %v", err)
	}

	// Three events spanning two zones, two kinds, three timestamps.
	events := []EventRow{
		{
			Kind:       "feedback",
			TrackID:    sql.NullString{String: "track-A", Valid: true},
			Zone:       sql.NullString{String: "Living Room", Valid: true},
			Source:     sql.NullString{String: "cli", Valid: true},
			Payload:    sql.NullString{String: `{"verdict":"love"}`, Valid: true},
			OccurredAt: now - 300, // oldest
		},
		{
			Kind:       "observe",
			TrackID:    sql.NullString{String: "track-A", Valid: true},
			Zone:       sql.NullString{String: "Living Room", Valid: true},
			Source:     sql.NullString{String: "now", Valid: true},
			OccurredAt: now - 100,
		},
		{
			Kind:       "note",
			TrackID:    sql.NullString{String: "track-A", Valid: true},
			Zone:       sql.NullString{String: "Kitchen", Valid: true},
			Source:     sql.NullString{String: "cli", Valid: true},
			OccurredAt: now, // newest
		},
	}
	for _, e := range events {
		if err := InsertEvent(ctx, e); err != nil {
			t.Fatalf("InsertEvent: %v", err)
		}
	}

	// No filters → all three, newest first.
	rows, err := History(ctx, HistoryFilter{})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3: %+v", len(rows), rows)
	}
	if rows[0].Kind != "note" || rows[1].Kind != "observe" || rows[2].Kind != "feedback" {
		t.Errorf("order wrong: %s, %s, %s", rows[0].Kind, rows[1].Kind, rows[2].Kind)
	}
	if rows[0].Track == nil || rows[0].Track.Title != "Alpha" {
		t.Errorf("expected joined track title Alpha, got %+v", rows[0].Track)
	}

	// Kind filter.
	rows, err = History(ctx, HistoryFilter{Kind: "feedback"})
	if err != nil {
		t.Fatalf("History kind: %v", err)
	}
	if len(rows) != 1 || rows[0].Kind != "feedback" {
		t.Fatalf("kind filter wrong: %+v", rows)
	}

	// Zone filter.
	rows, err = History(ctx, HistoryFilter{Zone: "Kitchen"})
	if err != nil {
		t.Fatalf("History zone: %v", err)
	}
	if len(rows) != 1 || rows[0].Zone != "Kitchen" {
		t.Fatalf("zone filter wrong: %+v", rows)
	}

	// Artist substring filter via cache JOIN.
	rows, err = History(ctx, HistoryFilter{Artist: "Artist One"})
	if err != nil {
		t.Fatalf("History artist: %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("artist filter expected 3 rows, got %d", len(rows))
	}

	// Since filter — only the two newest (within last 200s).
	rows, err = History(ctx, HistoryFilter{Since: 200 * time.Second})
	if err != nil {
		t.Fatalf("History since: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("since filter expected 2 rows, got %d", len(rows))
	}
}

func TestComputeStatsEmpty(t *testing.T) {
	setupTempDB(t)
	ctx := context.Background()

	for _, mode := range []string{"top", "recent", "skipped"} {
		s, err := ComputeStats(ctx, mode, 5)
		if err != nil {
			t.Fatalf("ComputeStats(%s): %v", mode, err)
		}
		if s == nil {
			t.Fatalf("ComputeStats(%s) returned nil stats", mode)
		}
	}
}
