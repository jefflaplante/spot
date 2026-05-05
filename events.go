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
