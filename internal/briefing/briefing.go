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
	OutcomeDeleted  = "deleted"
	OutcomeArchived = "archived" // deleted after being promoted: the Karakeep bookmark stays
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
	Excerpt   string // what the item was triaged on; shown when there is no summary
	Summary   string // a digest's, or a promoted item's
	Outcome   string
}

// Highlight is the Karakeep highlight picked for a day. It is shown before
// the day's entries and is not an entry: nothing can be done with it.
type Highlight struct {
	Text  string
	Note  string
	Color string
	Title string // the bookmark it was marked in; empty when that could not be read
	URL   string // the original article, when the bookmark is a link
	// KarakeepURL is the bookmark in Karakeep. Empty for a pick made before it was stored.
	KarakeepURL string
	Byline      string
}

// Briefing is one day's briefing for one user.
type Briefing struct {
	Day       string // the user's local date, YYYY-MM-DD
	Final     bool   // outcomes are final: untriaged entries read "carried over"
	Highlight *Highlight
	Today     []Entry
	Backlog   []Entry
}

// Service builds and writes briefings.
type Service struct {
	DB *sql.DB
	// Dir is the root of the export folders; each user gets Dir/<username>.
	Dir string
}

// New returns a Service that writes under dir.
func New(db *sql.DB, dir string) *Service { return &Service{DB: db, Dir: dir} }

// Build assembles userID's briefing for day. Today is what arrived that day,
// with whatever outcome it has now. An item is listed with its excerpt, and
// with a summary once it has one (a digest always does, a promoted item when
// its summary has come in), so a promoted item's summary appears at the next
// rewrite rather than live (Q25). Backlog is what earlier days
// left waiting. With final set, an entry still waiting reads "carried over".
func (s *Service) Build(ctx context.Context, userID, day string, final bool) (Briefing, error) {
	b := Briefing{Day: day, Final: final}
	var err error
	if b.Highlight, err = s.highlight(ctx, userID, day); err != nil {
		return b, err
	}
	if b.Today, err = s.entries(ctx, userID, `x.batch_date = ? AND x.state IN ('light', 'deep', 'archived', 'kept', 'pending_deep', 'expired')`, day, final); err != nil {
		return b, err
	}
	if b.Backlog, err = s.entries(ctx, userID, `x.batch_date < ? AND x.state IN ('light', 'deep', 'pending_deep')`, day, final); err != nil {
		return b, err
	}
	return b, nil
}

// Highlight returns the highlight stored for day, or nil, for pages that show
// it without building the whole briefing. It reads only SQLite: the pick is
// made by the highlights timer, never while a page loads.
func (s *Service) Highlight(ctx context.Context, userID, day string) (*Highlight, error) {
	return s.highlight(ctx, userID, day)
}

// highlight returns the highlight stored for day, or nil. It reads only
// SQLite: the pick is made by the highlights timer, never while a page loads.
func (s *Service) highlight(ctx context.Context, userID, day string) (*Highlight, error) {
	var h Highlight
	err := s.DB.QueryRowContext(ctx, `SELECT text, note, color, bookmark_title, bookmark_url, karakeep_url, byline
		FROM highlight_picks WHERE user_id = ? AND day = ?`, userID, day).
		Scan(&h.Text, &h.Note, &h.Color, &h.Title, &h.URL, &h.KarakeepURL, &h.Byline)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read highlight: %w", err)
	}
	return &h, nil
}

func outcome(state string, promoted, final bool) string {
	switch state {
	case "archived":
		if promoted {
			return OutcomeArchived
		}
		return OutcomeDeleted
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
	rows, err := s.DB.QueryContext(ctx, `SELECT x.id, x.title, x.url, COALESCE(NULLIF(x.byline, ''), src.name), x.media_type, x.state,
		x.excerpt, COALESCE(NULLIF(x.deep_summary, ''), x.light_summary, ''), x.promoted_at IS NOT NULL
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
		var promoted bool
		if err := rows.Scan(&e.ID, &e.Title, &e.URL, &e.Source, &e.MediaType, &state, &e.Excerpt, &e.Summary, &promoted); err != nil {
			rows.Close()
			return nil, err
		}
		e.Outcome = outcome(state, promoted, final)
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
		e.Outcome = outcome(state, false, final)
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

// Days lists the days userID has a summarized run for, newest first.
func (s *Service) Days(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT DISTINCT day FROM runs
		WHERE user_id = ? AND summaries_at IS NOT NULL AND day != '' ORDER BY day DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var days []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		days = append(days, d)
	}
	return days, rows.Err()
}

// File returns the briefing file written for day, exactly as it is on disk. A
// day with no file gives an error that satisfies errors.Is(err, os.ErrNotExist).
// The text is only ever shown or downloaded, never parsed.
func (s *Service) File(ctx context.Context, userID, day string) ([]byte, error) {
	p, err := s.path(ctx, userID, day)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

// Refresh rewrites the live file for day if one is already there, so a change
// made after it was written (the day's highlight) shows up in it. It writes
// nothing when there is no file yet: the first submission of the day makes it.
func (s *Service) Refresh(ctx context.Context, userID, day string) error {
	p, err := s.path(ctx, userID, day)
	if err != nil {
		return err
	}
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return s.Write(ctx, userID, day, false)
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
