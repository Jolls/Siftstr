// Package janitor deletes old workflow rows and writes the daily database
// snapshot. Siftstr keeps only workflow state, and only for six months; the
// source services stay the system of record.
package janitor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// RetentionMonths is how long workflow rows are kept, measured from created_at.
const RetentionMonths = 6

// KeepSnapshots is how many daily snapshots stay on disk.
const KeepSnapshots = 7

const (
	snapshotPrefix = "siftstr-"
	snapshotSuffix = ".db"
)

// Janitor prunes one user's rows and snapshots the whole database.
type Janitor struct {
	DB *sql.DB
	// BackupDir receives the daily snapshots (/data/backups).
	BackupDir string
	Now       func() time.Time
}

// Result counts the rows a prune deleted.
type Result struct {
	Items, Digests, Actions, Outbox, Runs int64
}

func (j *Janitor) now() time.Time {
	if j.Now != nil {
		return j.Now().UTC()
	}
	return time.Now().UTC()
}

// Prune deletes userID's workflow rows older than the retention period, by
// created_at (received_at for actions, started_at for runs, batch_date for
// digests, which have no created_at). It never touches another user's rows or
// the user's sources, connections and settings. Running it twice is harmless.
func (j *Janitor) Prune(ctx context.Context, userID string) (Result, error) {
	if userID == "" {
		return Result{}, errors.New("janitor: user ID required")
	}
	now := j.now()
	cutoff := now.AddDate(0, -RetentionMonths, 0)
	ts := cutoff.Format(time.RFC3339)
	day := cutoff.Format("2006-01-02")

	tx, err := j.DB.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, fmt.Errorf("janitor: begin: %w", err)
	}
	defer tx.Rollback()

	var res Result
	// Order matters: foreign keys reference actions from outbox, and digests
	// from items, so children go first.
	steps := []struct {
		dst   *int64
		query string
		args  []any
	}{
		{&res.Outbox, `DELETE FROM outbox WHERE user_id = ? AND created_at < ?`, []any{userID, ts}},
		{&res.Actions, `DELETE FROM actions WHERE user_id = ? AND received_at < ?`, []any{userID, ts}},
		{&res.Runs, `DELETE FROM runs WHERE user_id = ? AND started_at < ?`, []any{userID, ts}},
		{&res.Items, `DELETE FROM items WHERE user_id = ? AND created_at < ?`, []any{userID, ts}},
		{&res.Digests, `DELETE FROM digests WHERE user_id = ? AND batch_date < ?
			AND NOT EXISTS (SELECT 1 FROM items WHERE items.user_id = digests.user_id AND items.digest_id = digests.id)`,
			[]any{userID, day}},
	}
	for _, s := range steps {
		r, err := tx.ExecContext(ctx, s.query, s.args...)
		if err != nil {
			return Result{}, fmt.Errorf("janitor: %w", err)
		}
		*s.dst, _ = r.RowsAffected()
	}
	// Expired sessions are dead weight for every user; this one is scoped too.
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND expires_at < ?`,
		userID, now.Format(time.RFC3339)); err != nil {
		return Result{}, fmt.Errorf("janitor: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Result{}, fmt.Errorf("janitor: commit: %w", err)
	}
	return res, nil
}

// Snapshot writes a consistent copy of the database with VACUUM INTO, so file
// backup tools never see a half-written WAL database. There is one snapshot
// per UTC day; a second call the same day does nothing. Only the newest
// KeepSnapshots files stay. It reports whether a new file was written.
func (j *Janitor) Snapshot(ctx context.Context) (bool, error) {
	if j.BackupDir == "" {
		return false, errors.New("janitor: no backup directory")
	}
	if err := os.MkdirAll(j.BackupDir, 0o750); err != nil {
		return false, fmt.Errorf("janitor: create backup dir: %w", err)
	}
	name := snapshotPrefix + j.now().Format("2006-01-02") + snapshotSuffix
	final := filepath.Join(j.BackupDir, name)

	wrote := false
	if _, err := os.Stat(final); errors.Is(err, os.ErrNotExist) {
		// Write beside the target and rename, so a crash leaves no partial
		// file under the real name. VACUUM INTO needs a path that is absent.
		tmp := final + ".tmp"
		_ = os.Remove(tmp)
		if _, err := j.DB.ExecContext(ctx, `VACUUM INTO ?`, filepath.ToSlash(tmp)); err != nil {
			_ = os.Remove(tmp)
			return false, fmt.Errorf("janitor: snapshot: %w", err)
		}
		_ = os.Chmod(tmp, 0o600) // holds encrypted credentials; keep it owner-only
		if err := os.Rename(tmp, final); err != nil {
			_ = os.Remove(tmp)
			return false, fmt.Errorf("janitor: snapshot: %w", err)
		}
		wrote = true
	} else if err != nil {
		return false, fmt.Errorf("janitor: snapshot: %w", err)
	}
	return wrote, j.prune()
}

// prune removes all but the newest KeepSnapshots snapshot files. Names sort
// by date, so the lexical order is the age order.
func (j *Janitor) prune() error {
	entries, err := os.ReadDir(j.BackupDir)
	if err != nil {
		return fmt.Errorf("janitor: list backups: %w", err)
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() && strings.HasPrefix(n, snapshotPrefix) && strings.HasSuffix(n, snapshotSuffix) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for len(names) > KeepSnapshots {
		if err := os.Remove(filepath.Join(j.BackupDir, names[0])); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("janitor: remove old backup: %w", err)
		}
		names = names[1:]
	}
	return nil
}
