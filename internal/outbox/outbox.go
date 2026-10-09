// Package outbox is the queue of upstream side effects. Effects are enqueued
// in the same transaction that releases an action, then sent by a runner with
// retry and backoff. Every handler must be idempotent: a crash between a
// successful send and the "done" mark means the entry is sent again.
package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Jolls/Siftstr/internal/ids"
)

// Defaults for retry.
const (
	DefaultMaxAttempts = 10
	MaxBackoff         = 6 * time.Hour
)

// Backoff is the wait after the nth failed attempt: 1m, 2m, 4m, ... capped.
func Backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 20 {
		return MaxBackoff
	}
	d := time.Minute << (attempt - 1)
	if d > MaxBackoff {
		return MaxBackoff
	}
	return d
}

// Entry is one queued effect. Payload is opaque to the outbox.
type Entry struct {
	ID           string
	UserID       string
	ActionID     string // the action that caused it, if any
	ConnectionID string
	Op           string
	Payload      json.RawMessage
	Key          string // dedupe key
	Attempts     int
}

// Execer is satisfied by *sql.DB and *sql.Tx, so Enqueue can join a
// transaction.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Enqueue adds an entry unless the user already has one with the same
// dedupe key (done or not). It reports whether a row was added. An empty key
// is never deduplicated.
func Enqueue(ctx context.Context, x Execer, now time.Time, e Entry, key string) (bool, error) {
	id, err := ids.New("obx_")
	if err != nil {
		return false, err
	}
	if len(e.Payload) == 0 {
		e.Payload = json.RawMessage(`{}`)
	}
	r, err := x.ExecContext(ctx,
		`INSERT INTO outbox (id, user_id, action_id, connection_id, op, payload, status, attempts, created_at, next_attempt_at, dedupe_key)
		 VALUES (?, ?, ?, ?, ?, ?, 'pending', 0, ?, '', ?)
		 ON CONFLICT DO NOTHING`,
		id, e.UserID, nullable(e.ActionID), nullable(e.ConnectionID), e.Op, string(e.Payload), stamp(now), nullable(key))
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n > 0, nil
}

// Permanent is implemented by errors that retrying cannot fix, such as a
// rejected credential or a connection that was removed.
type permanent interface{ Permanent() bool }

// Stats counts what one Run did.
type Stats struct{ Done, Retried, Failed int }

// Runner sends due entries.
type Runner struct {
	DB          *sql.DB
	Handle      func(ctx context.Context, e Entry) error
	Now         func() time.Time
	MaxAttempts int // DefaultMaxAttempts if zero
}

// Run sends userID's due entries, oldest first. One entry failing does not
// stop the others. A user's entries are only ever sent by one Run at a time
// (the scheduler never overlaps a job with itself).
func (r *Runner) Run(ctx context.Context, userID string) (Stats, error) {
	var st Stats
	max := r.MaxAttempts
	if max <= 0 {
		max = DefaultMaxAttempts
	}
	now := r.Now().UTC()
	rows, err := r.DB.QueryContext(ctx,
		`SELECT id, COALESCE(action_id, ''), COALESCE(connection_id, ''), op, payload, COALESCE(dedupe_key, ''), attempts
		 FROM outbox WHERE user_id = ? AND status = 'pending' AND next_attempt_at <= ?
		 ORDER BY created_at, id`, userID, stamp(now))
	if err != nil {
		return st, err
	}
	var due []Entry
	for rows.Next() {
		e := Entry{UserID: userID}
		var payload string
		if err := rows.Scan(&e.ID, &e.ActionID, &e.ConnectionID, &e.Op, &payload, &e.Key, &e.Attempts); err != nil {
			rows.Close()
			return st, err
		}
		e.Payload = json.RawMessage(payload)
		due = append(due, e)
	}
	if err := rows.Close(); err != nil {
		return st, err
	}

	for _, e := range due {
		if ctx.Err() != nil {
			return st, ctx.Err()
		}
		herr := r.Handle(ctx, e)
		attempts := e.Attempts + 1
		var perr permanent
		switch {
		case herr == nil:
			_, err = r.DB.ExecContext(ctx, `UPDATE outbox SET status = 'done', attempts = ?, last_error = NULL, done_at = ? WHERE id = ? AND user_id = ?`,
				attempts, stamp(r.Now()), e.ID, userID)
			st.Done++
		case (errors.As(herr, &perr) && perr.Permanent()) || attempts >= max:
			_, err = r.DB.ExecContext(ctx, `UPDATE outbox SET status = 'failed', attempts = ?, last_error = ? WHERE id = ? AND user_id = ?`,
				attempts, herr.Error(), e.ID, userID)
			st.Failed++
		default:
			_, err = r.DB.ExecContext(ctx, `UPDATE outbox SET attempts = ?, last_error = ?, next_attempt_at = ? WHERE id = ? AND user_id = ?`,
				attempts, herr.Error(), stamp(r.Now().Add(Backoff(attempts))), e.ID, userID)
			st.Retried++
		}
		if err != nil {
			return st, err
		}
	}
	return st, nil
}
