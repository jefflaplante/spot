package spot

import (
	"context"
	"database/sql"
	"testing"
)

// TestLogEventInsertsRow verifies the instrumentation path: logEvent
// writes a row to the events table that downstream code (history/stats)
// can read back. It uses a synthetic EventRow so the test doesn't need
// real Spotify or Sonos credentials.
func TestLogEventInsertsRow(t *testing.T) {
	setupTempDB(t)

	ctx := context.Background()
	db, err := getDB(ctx)
	if err != nil {
		t.Fatalf("getDB: %v", err)
	}

	logEvent(ctx, EventRow{
		Kind:    "pause",
		Zone:    sql.NullString{String: "Living Room", Valid: true},
		Source:  sql.NullString{String: "pause", Valid: true},
		Payload: marshalPayload(map[string]any{}),
	})

	var (
		gotKind, gotZone, gotSource sql.NullString
		gotPayload                  sql.NullString
		occurredAt                  int64
	)
	err = db.QueryRowContext(ctx,
		`SELECT kind, zone, source, payload, occurred_at
		   FROM events ORDER BY id DESC LIMIT 1`,
	).Scan(&gotKind, &gotZone, &gotSource, &gotPayload, &occurredAt)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if gotKind.String != "pause" {
		t.Errorf("kind: got %q want %q", gotKind.String, "pause")
	}
	if gotZone.String != "Living Room" {
		t.Errorf("zone: got %q want %q", gotZone.String, "Living Room")
	}
	if gotSource.String != "pause" {
		t.Errorf("source: got %q want %q", gotSource.String, "pause")
	}
	if !gotPayload.Valid || gotPayload.String != "{}" {
		t.Errorf("payload: got %+v, want {}", gotPayload)
	}
	if occurredAt == 0 {
		t.Errorf("occurred_at: got 0, want logEvent to stamp time.Now().Unix()")
	}
}

// TestMarshalPayloadShapes covers the payload combos the instrumentation
// actually emits, so a regression in marshalPayload (e.g. dropping the
// JSON object braces) shows up here rather than in downstream history
// queries.
func TestMarshalPayloadShapes(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string // empty means only check Valid + non-empty
	}{
		{"empty", map[string]any{}, "{}"},
		{"play_started", map[string]any{
			"transport":    "sonos",
			"continuation": true,
			"queue_size":   5,
		}, ""}, // map key order isn't stable
		{"library", map[string]any{"action": "like"}, `{"action":"like"}`},
		{"volume", map[string]any{"level": 42}, `{"level":42}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := marshalPayload(tc.in)
			if !got.Valid {
				t.Fatalf("marshalPayload returned invalid NullString")
			}
			if tc.want != "" && got.String != tc.want {
				t.Errorf("got %q, want %q", got.String, tc.want)
			}
			if got.String == "" {
				t.Errorf("empty payload string")
			}
		})
	}

	// nil should produce an absent NullString (Valid=false).
	if got := marshalPayload(nil); got.Valid {
		t.Errorf("marshalPayload(nil): want Valid=false, got %+v", got)
	}
}

// TestLogQueueAdvanceWritesOnTransition verifies the queue-advance
// detection used by NowPlaying. Captures the bug fix: tracks 2..N of a
// queue weren't landing in the events table because Sonos drives queue
// progression on its own. Now, when `spot now` observes a different
// track than the most-recent recorded one for the same zone, a
// play_started row gets synthesized.
func TestLogQueueAdvanceWritesOnTransition(t *testing.T) {
	setupTempDB(t)

	ctx := context.Background()
	db, err := getDB(ctx)
	if err != nil {
		t.Fatalf("getDB: %v", err)
	}

	// Simulate the seed track of a `play -c` invocation.
	logEvent(ctx, EventRow{
		Kind:     "play_started",
		TrackID:  sql.NullString{String: "track-A", Valid: true},
		ArtistID: sql.NullString{String: "artist-1", Valid: true},
		Zone:     sql.NullString{String: "Parlor", Valid: true},
		Source:   sql.NullString{String: "play", Valid: true},
		Payload:  marshalPayload(map[string]any{"continuation": true, "queue_size": 10}),
	})

	// Same track observed → no advance row.
	if logQueueAdvance(ctx, "Parlor", "track-A", "artist-1") {
		t.Errorf("logQueueAdvance: wrote a row when track hadn't changed")
	}

	// Different track observed → advance row written.
	if !logQueueAdvance(ctx, "Parlor", "track-B", "artist-1") {
		t.Errorf("logQueueAdvance: didn't write a row for a track transition")
	}

	// Verify the row landed with the expected fields.
	var (
		kind, trackID, source sql.NullString
	)
	err = db.QueryRowContext(ctx,
		`SELECT kind, track_id, source FROM events
		 ORDER BY id DESC LIMIT 1`,
	).Scan(&kind, &trackID, &source)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if kind.String != "play_started" {
		t.Errorf("kind: got %q want play_started", kind.String)
	}
	if trackID.String != "track-B" {
		t.Errorf("track_id: got %q want track-B", trackID.String)
	}
	if source.String != "queue-advance" {
		t.Errorf("source: got %q want queue-advance", source.String)
	}

	// Re-observing track-B → no duplicate row.
	if logQueueAdvance(ctx, "Parlor", "track-B", "artist-1") {
		t.Errorf("logQueueAdvance: wrote a duplicate row for the same track")
	}

	// Different zone, same track → still considered a transition for THAT zone.
	if !logQueueAdvance(ctx, "Kitchen", "track-B", "artist-1") {
		t.Errorf("logQueueAdvance: should write on first observation in a new zone")
	}
}
