package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// ErrNotFound is returned for a missing entry, another user's entry, or one
// that is not stopped.
var ErrNotFound = errors.New("outbox entry not found")

// What the Activity page calls an entry.
const (
	StatusQueued   = "queued"   // waiting for its first send
	StatusRetrying = "retrying" // failed at least once, will try again
	StatusDone     = "done"
	StatusPaused   = "paused" // ran out of retries; Retry sends it again
	StatusFailed   = "failed" // a retry cannot fix it until settings change
)

// Row is one entry as the Activity page shows it.
type Row struct {
	ID        string
	Op        string // the destination: miniflux, karakeep, metube
	Title     string
	Status    string
	Attempts  int
	LastError string
	CreatedAt time.Time
	// Stopped is true for paused and failed entries, which the user can retry
	// or dismiss.
	Stopped bool
}

// Log reads and manages a user's outbox entries.
type Log struct{ DB *sql.DB }

// Recent lists userID's newest entries, leaving out dismissed ones.
func (l *Log) Recent(ctx context.Context, userID string, limit int) ([]Row, error) {
	rows, err := l.DB.QueryContext(ctx,
		`SELECT id, op, payload, status, permanent, attempts, COALESCE(last_error, ''), created_at
		 FROM outbox WHERE user_id = ? AND dismissed_at IS NULL
		 ORDER BY created_at DESC, rowid DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		var payload, status, created string
		var permanent bool
		if err := rows.Scan(&r.ID, &r.Op, &payload, &status, &permanent, &r.Attempts, &r.LastError, &created); err != nil {
			return nil, err
		}
		var p struct{ Title, ID string }
		_ = json.Unmarshal([]byte(payload), &p)
		if r.Title = p.Title; r.Title == "" {
			r.Title = p.ID
		}
		r.CreatedAt, _ = time.Parse(time.RFC3339, created)
		switch {
		case status == "done":
			r.Status = StatusDone
		case status == "failed" && permanent:
			r.Status, r.Stopped = StatusFailed, true
		case status == "failed":
			r.Status, r.Stopped = StatusPaused, true
		case r.Attempts > 0:
			r.Status = StatusRetrying
		default:
			r.Status = StatusQueued
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Problems counts userID's stopped entries that were not dismissed. The pages
// show it as an alert.
func (l *Log) Problems(ctx context.Context, userID string) (int, error) {
	var n int
	err := l.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM outbox WHERE user_id = ? AND status = 'failed' AND dismissed_at IS NULL`, userID).Scan(&n)
	return n, err
}

// Retry puts a stopped entry back in the queue with a fresh retry budget. The
// entry keeps its dedupe key, so it is still sent at most once.
func (l *Log) Retry(ctx context.Context, userID, id string) error {
	return l.change(ctx, `UPDATE outbox SET status = 'pending', attempts = 0, permanent = 0, next_attempt_at = ''
		WHERE id = ? AND user_id = ? AND status = 'failed' AND dismissed_at IS NULL`, id, userID)
}

// Dismiss hides a stopped entry and drops it from the alert. Nothing is sent.
func (l *Log) Dismiss(ctx context.Context, userID, id string, now time.Time) error {
	return l.change(ctx, `UPDATE outbox SET dismissed_at = ?
		WHERE id = ? AND user_id = ? AND status = 'failed' AND dismissed_at IS NULL`, stamp(now), id, userID)
}

func (l *Log) change(ctx context.Context, q string, args ...any) error {
	r, err := l.DB.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
