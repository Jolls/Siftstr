// Package briefing builds a user's daily briefing (Today and Backlog) from
// SQLite and renders it as markdown. The /read page renders the same
// Briefing, so the file matches what the user saw. Markdown is only ever
// written, never read back (ARCHITECTURE.md section 8).
package briefing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Outcomes an entry can show. Empty means still waiting for a decision.
const (
	OutcomeArchived = "archived"
	OutcomeKept     = "kept"
	OutcomePromoted = "promoted"
	OutcomeCarried  = "carried over"
	OutcomeExpired  = "expired"
)

// settingFinalized is the user_settings key holding the last closed day.
const settingFinalized = "briefing_finalized_day"

// Entry is one item or digest in a briefing.
type Entry struct {
	ID        string
	Digest    bool
	Title     string
	URL       string
	Source    string
	MediaType string
	Summary   string
	Outcome   string
}

// Briefing is one day's briefing for one user.
type Briefing struct {
	Day     string // the user's local date, YYYY-MM-DD
	Final   bool   // outcomes are final: untriaged entries read "carried over"
	Today   []Entry
	Backlog []Entry
}

// Service builds and writes briefings.
type Service struct {
	DB *sql.DB
	// Dir is the root of the export folders; each user gets Dir/<username>.
	Dir string
}

// New returns a Service that writes under dir.
func New(db *sql.DB, dir string) *Service { return &Service{DB: db, Dir: dir} }

// Build assembles userID's briefing for day. Today is what was summarized
// that day, with whatever outcome it has now. Backlog is what earlier days
// left waiting. With final set, an entry still waiting reads "carried over".
func (s *Service) Build(ctx context.Context, userID, day string, final bool) (Briefing, error) {
	b := Briefing{Day: day, Final: final}
	var err error
	if b.Today, err = s.entries(ctx, userID, `x.batch_date = ? AND x.state IN ('light', 'deep', 'archived', 'kept', 'pending_deep', 'expired')`, day, final); err != nil {
		return b, err
	}
	if b.Backlog, err = s.entries(ctx, userID, `x.batch_date < ? AND x.state IN ('light', 'deep', 'pending_deep')`, day, final); err != nil {
		return b, err
	}
	return b, nil
}

func outcome(state string, final bool) string {
	switch state {
	case "archived":
		return OutcomeArchived
	case "kept":
		return OutcomeKept
	case "pending_deep":
		return OutcomePromoted
	case "expired":
		return OutcomeExpired
	}
	if final {
		return OutcomeCarried
	}
	return ""
}

// entries lists items, then digests, matching cond (written against alias x).
// Entries inside a digest are covered by the digest and not listed.
func (s *Service) entries(ctx context.Context, userID, cond, day string, final bool) ([]Entry, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT x.id, x.title, x.url, src.name, x.media_type, x.state,
		COALESCE(NULLIF(x.deep_summary, ''), x.light_summary, '')
		FROM items x JOIN sources src ON src.user_id = x.user_id AND src.id = x.source_id
		WHERE x.user_id = ? AND x.digest_id IS NULL AND `+cond+`
		ORDER BY COALESCE(x.published_at, x.created_at) DESC, x.id`, userID, day)
	if err != nil {
		return nil, fmt.Errorf("list items: %w", err)
	}
	var out []Entry
	for rows.Next() {
		var e Entry
		var state string
		if err := rows.Scan(&e.ID, &e.Title, &e.URL, &e.Source, &e.MediaType, &state, &e.Summary); err != nil {
			rows.Close()
			return nil, err
		}
		e.Outcome = outcome(state, final)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	drows, err := s.DB.QueryContext(ctx, `SELECT x.id, src.name, x.state, COALESCE(x.summary, '')
		FROM digests x JOIN sources src ON src.user_id = x.user_id AND src.id = x.source_id
		WHERE x.user_id = ? AND `+cond+`
		ORDER BY x.batch_date DESC, x.id`, userID, day)
	if err != nil {
		return nil, fmt.Errorf("list digests: %w", err)
	}
	defer drows.Close()
	for drows.Next() {
		e := Entry{Digest: true, MediaType: "digest"}
		var state string
		if err := drows.Scan(&e.ID, &e.Source, &state, &e.Summary); err != nil {
			return nil, err
		}
		e.Title = e.Source + " digest"
		e.Outcome = outcome(state, final)
		out = append(out, e)
	}
	return out, drows.Err()
}

// path is where userID's file for day lives: Dir/<username>/<day>.md.
func (s *Service) path(ctx context.Context, userID, day string) (string, error) {
	var name string
	if err := s.DB.QueryRowContext(ctx, `SELECT username FROM users WHERE id = ?`, userID).Scan(&name); err != nil {
		return "", err
	}
	// Usernames are lowercased text; refuse anything that could leave the folder.
	if name == "" || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") ||
		strings.ContainsAny(name, `/\:*?"<>|`+"\x00") {
		return "", fmt.Errorf("username %q is not usable as a folder name", name)
	}
	if _, err := time.Parse("2006-01-02", day); err != nil {
		return "", fmt.Errorf("bad day %q", day)
	}
	return filepath.Join(s.Dir, name, day+".md"), nil
}

// Write renders the briefing for day to the user's export folder. The file is
// replaced atomically, so a sync client never sees half a file.
func (s *Service) Write(ctx context.Context, userID, day string, final bool) error {
	if !final {
		// A closed day keeps its final outcomes; a late or repeated submission
		// must not turn it back into a live file.
		closed, err := s.finalizedDay(ctx, userID)
		if err != nil {
			return err
		}
		if day <= closed {
			return nil
		}
	}
	b, err := s.Build(ctx, userID, day, final)
	if err != nil {
		return err
	}
	p, err := s.path(ctx, userID, day)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".briefing-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(b.Markdown()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// Finalize rewrites the file of every earlier day that was summarized and
// not yet closed, with final outcomes. today is the user's current local
// date. Call it at the start of a morning run, after carryover has been
// applied and before the new day's file is written.
func (s *Service) Finalize(ctx context.Context, userID, today string) error {
	last, err := s.finalizedDay(ctx, userID)
	if err != nil {
		return err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT DISTINCT day FROM runs
		WHERE user_id = ? AND summaries_at IS NOT NULL AND day != '' AND day > ? AND day < ? ORDER BY day`, userID, last, today)
	if err != nil {
		return err
	}
	var days []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			rows.Close()
			return err
		}
		days = append(days, d)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	// A day that fails to write must not block the days after it. The marker
	// only moves up to the last day before the first failure, so the failed
	// day is tried again next time.
	var first error
	closed := last
	for _, d := range days {
		if err := s.Write(ctx, userID, d, true); err != nil {
			if first == nil {
				first = fmt.Errorf("finalize %s: %w", d, err)
			}
			continue
		}
		if first == nil {
			closed = d
		}
	}
	if closed != last {
		if _, err := s.DB.ExecContext(ctx, `INSERT INTO user_settings (user_id, key, value) VALUES (?, ?, ?)
			ON CONFLICT(user_id, key) DO UPDATE SET value = excluded.value`, userID, settingFinalized, closed); err != nil {
			return err
		}
	}
	return first
}

// finalizedDay is the last day whose file was closed, or empty.
func (s *Service) finalizedDay(ctx context.Context, userID string) (string, error) {
	var last string
	err := s.DB.QueryRowContext(ctx, `SELECT value FROM user_settings WHERE user_id = ? AND key = ?`, userID, settingFinalized).Scan(&last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	return last, nil
}
