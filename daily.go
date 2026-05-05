package spot

// Daily — a Spotify-Daily replacement built from the user's top long-term
// tracks minus what they've heard in the last 30 days, weighted by local
// love/hate feedback recorded via the events DB.
//
// The pipeline is intentionally small and deterministic:
//
//   1. TopTracks(long_term, 50)            — the long-term taste profile.
//   2. RecentlyPlayed(30d, 50)             — what to skip past for novelty.
//   3. Drop tracks with skip-forever/hate  — local "never play again" signal.
//   4. Boost tracks with love feedback     — surface known winners.
//   5. Truncate to opts.Length (default 20).
//
// The output is a slice of *trackInfo, the same in-package shape returned by
// Like / Feedback / Mark, so the CLI can read its TrackID/Title/Artist/Album
// fields directly.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// DailyOptions configures Daily.
type DailyOptions struct {
	// Length caps the size of the returned slice. <=0 means use the
	// default (20).
	Length int
}

const (
	dailyDefaultLength = 20
	dailyTopFetch      = 50 // Spotify-side max for /me/top/tracks.
	dailyRecentWindow  = 30 * 24 * time.Hour
	dailyRecentFetch   = 50 // Spotify-side max for /me/player/recently-played.

	// Feedback weights. love floats a track to the top of the candidate
	// pool; hate / skip-forever filter it out entirely. Neutral tracks
	// keep their long-term-rank ordering.
	dailyLoveWeight = 10
)

// Daily returns up to opts.Length top long-term tracks the user has not
// played in the last 30 days, with love/hate feedback applied as filter
// (hate/skip-forever) and boost (love).
func Daily(ctx context.Context, opts DailyOptions) ([]*trackInfo, error) {
	length := opts.Length
	if length <= 0 {
		length = dailyDefaultLength
	}

	tops, err := TopTracks(ctx, TopLong, dailyTopFetch)
	if err != nil {
		return nil, err
	}
	if len(tops) == 0 {
		return nil, nil
	}

	recent, err := RecentlyPlayed(ctx, dailyRecentWindow, dailyRecentFetch)
	if err != nil {
		return nil, err
	}
	recentSet := make(map[string]struct{}, len(recent))
	for _, it := range recent {
		id := string(it.Track.ID)
		if id != "" {
			recentSet[id] = struct{}{}
		}
	}

	// One pass over the events DB to grab feedback verdicts for the
	// candidate IDs. Returns map[trackID]verdict where verdict is the
	// most recent love/hate/skip-forever for the track.
	candidateIDs := make([]string, 0, len(tops))
	for _, t := range tops {
		id := string(t.ID)
		if id == "" {
			continue
		}
		candidateIDs = append(candidateIDs, id)
	}
	verdicts, err := dailyFeedbackVerdicts(ctx, candidateIDs)
	if err != nil {
		return nil, err
	}

	type scored struct {
		info  *trackInfo
		rank  int // original long-term rank (lower is better)
		boost int // feedback boost (love=+10)
	}
	var pool []scored
	for i, t := range tops {
		id := string(t.ID)
		if id == "" {
			continue
		}
		if _, played := recentSet[id]; played {
			continue
		}
		switch verdicts[id] {
		case "hate", "skip-forever":
			continue
		}
		info := makeTrackInfo(&t)
		boost := 0
		if verdicts[id] == "love" {
			boost = dailyLoveWeight
		}
		pool = append(pool, scored{info: info, rank: i, boost: boost})
	}

	// Sort: higher boost first, then lower (better) original rank.
	sort.SliceStable(pool, func(i, j int) bool {
		if pool[i].boost != pool[j].boost {
			return pool[i].boost > pool[j].boost
		}
		return pool[i].rank < pool[j].rank
	})

	if len(pool) > length {
		pool = pool[:length]
	}
	out := make([]*trackInfo, 0, len(pool))
	for _, s := range pool {
		out = append(out, s.info)
	}
	return out, nil
}

// dailyFeedbackVerdicts returns the most recent love/hate/skip-forever
// verdict for each track ID in `ids`, drawn from the events table. Track
// IDs with no feedback are absent from the result.
func dailyFeedbackVerdicts(ctx context.Context, ids []string) (map[string]string, error) {
	out := make(map[string]string)
	if len(ids) == 0 {
		return out, nil
	}
	db, err := getDB(ctx)
	if err != nil {
		return nil, err
	}

	// SQLite has a default 999-parameter cap; chunk to be safe even
	// though Daily today calls this with at most 50 IDs.
	const chunk = 500
	for start := 0; start < len(ids); start += chunk {
		end := start + chunk
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[start:end]
		placeholders := make([]string, len(batch))
		args := make([]any, 0, len(batch))
		for i, id := range batch {
			placeholders[i] = "?"
			args = append(args, id)
		}
		// Pick the latest feedback row per track. SQLite's "GROUP BY +
		// MAX(occurred_at)" trick lets us avoid a window function while
		// still picking the verdict that goes with the chosen row.
		q := fmt.Sprintf(`
			SELECT track_id, payload
			  FROM events
			 WHERE kind = 'feedback'
			   AND track_id IN (%s)
			   AND id IN (
			       SELECT MAX(id) FROM events
			        WHERE kind = 'feedback' AND track_id IN (%s)
			        GROUP BY track_id
			   )`, strings.Join(placeholders, ","), strings.Join(placeholders, ","))
		// args repeats for the inner IN clause.
		args = append(args, args...)
		rows, err := db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("daily feedback: %w", err)
		}
		if err := scanVerdicts(rows, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// scanVerdicts reads (track_id, payload) rows produced by
// dailyFeedbackVerdicts and stuffs the verdict into `out`. Closes rows
// before returning.
func scanVerdicts(rows *sql.Rows, out map[string]string) error {
	defer rows.Close()
	for rows.Next() {
		var trackID sql.NullString
		var payload sql.NullString
		if err := rows.Scan(&trackID, &payload); err != nil {
			return fmt.Errorf("scan feedback row: %w", err)
		}
		if !trackID.Valid || trackID.String == "" {
			continue
		}
		v := extractVerdict(payload.String)
		if v == "" {
			continue
		}
		out[trackID.String] = v
	}
	return rows.Err()
}

// extractVerdict pulls the "verdict" key out of a feedback payload JSON
// blob. Feedback payloads are always small ({"verdict":"love",...}), so
// substring matching is enough and avoids dragging json into this path.
func extractVerdict(payload string) string {
	switch {
	case strings.Contains(payload, `"verdict":"love"`):
		return "love"
	case strings.Contains(payload, `"verdict":"hate"`):
		return "hate"
	case strings.Contains(payload, `"verdict":"skip-forever"`):
		return "skip-forever"
	}
	return ""
}
