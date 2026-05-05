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
