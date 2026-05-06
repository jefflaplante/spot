package spot

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// logEvent is the best-effort write path used by every state-mutating
// command in the package. Errors from InsertEvent are swallowed: a
// failure to record an event must never fail the user's command.
//
// Callers populate Kind/Source/etc. and leave OccurredAt zero — logEvent
// stamps the timestamp itself so every call site doesn't have to repeat
// time.Now().Unix().
func logEvent(ctx context.Context, e EventRow) {
	if e.OccurredAt == 0 {
		e.OccurredAt = time.Now().Unix()
	}
	_ = InsertEvent(ctx, e)
}

// marshalPayload encodes an arbitrary map/struct as a JSON sql.NullString
// suitable for the events.payload column. On marshal failure it returns
// {Valid:false}; instrumentation must never crash the caller.
func marshalPayload(v any) sql.NullString {
	if v == nil {
		return sql.NullString{}
	}
	buf, err := json.Marshal(v)
	if err != nil {
		return sql.NullString{}
	}
	return sql.NullString{String: string(buf), Valid: true}
}

// logQueueAdvance writes a play_started event with source="queue-advance"
// when the zone's currently-playing track differs from the most-recent
// track recorded for that zone. This is how queue auto-advances (Sonos
// progressing through a queue without us calling Play) get captured —
// the next `spot now` invocation observes the change and synthesizes
// the row. Best-effort; silent on any DB error.
//
// Returns true iff a row was written (useful for tests).
func logQueueAdvance(ctx context.Context, zone, trackID, artistID string) bool {
	if zone == "" || trackID == "" {
		return false
	}
	db, err := getDB(ctx)
	if err != nil {
		return false
	}
	var prev sql.NullString
	err = db.QueryRowContext(ctx,
		`SELECT track_id FROM events
		 WHERE zone = ? AND track_id IS NOT NULL
		 ORDER BY occurred_at DESC, id DESC LIMIT 1`,
		zone).Scan(&prev)
	if err != nil && err != sql.ErrNoRows {
		return false
	}
	if prev.Valid && prev.String == trackID {
		return false // already the most-recent track for this zone
	}
	logEvent(ctx, EventRow{
		Kind:     "play_started",
		TrackID:  nullable(trackID),
		ArtistID: nullable(artistID),
		Zone:     nullable(zone),
		Source:   nullable("queue-advance"),
		Payload:  marshalPayload(map[string]any{"detected_via": "now"}),
	})
	return true
}
