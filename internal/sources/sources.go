// Package sources reads and edits a user's per-source triage settings.
package sources

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrNotFound is returned for a missing source or one owned by another user.
var ErrNotFound = errors.New("source not found")

// Source is one row of the settings table.
type Source struct {
	ID             string
	Kind           string
	Name           string
	Category       string
	Granularity    string // individual | digest
	MaxDepth       string // light | deep
	Carryover      string // carry | drop
	PromptOverride string
	Enabled        bool
	SummarySource  string // agent | karakeep: where a promoted item's summary comes from
}

// Settings is what the user may change on a source. Name and category follow
// the upstream and are not editable here.
type Settings struct {
	Granularity    string
	MaxDepth       string
	Carryover      string
	PromptOverride string
	Enabled        bool
	SummarySource  string // empty means agent
}

// MaxPromptLen bounds a per-source prompt override.
const MaxPromptLen = 8000

// Validate checks the enumerated fields and the prompt length.
func (s Settings) Validate() error {
	switch {
	case s.Granularity != "individual" && s.Granularity != "digest":
		return errors.New("granularity must be individual or digest")
	case s.MaxDepth != "light" && s.MaxDepth != "deep":
		return errors.New("max depth must be light or deep")
	case s.Carryover != "carry" && s.Carryover != "drop":
		return errors.New("carryover must be carry or drop")
	case len(s.PromptOverride) > MaxPromptLen:
		return fmt.Errorf("prompt override is longer than %d characters", MaxPromptLen)
	case s.SummarySource != "" && s.SummarySource != "agent" && s.SummarySource != "karakeep":
		return errors.New("summary source must be agent or karakeep")
	case s.SummarySource == "karakeep" && s.Granularity == "digest":
		return errors.New("a digest has no page for Karakeep to summarize, so it cannot use Karakeep summaries")
	}
	return nil
}

// Service reads and writes sources.
type Service struct{ db *sql.DB }

// New returns a Service.
func New(db *sql.DB) *Service { return &Service{db: db} }

// List returns userID's sources grouped by category, then name.
func (s *Service) List(ctx context.Context, userID string) ([]Source, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, kind, name, category, granularity, max_depth, carryover, COALESCE(prompt_override, ''), enabled, summary_source
		 FROM sources WHERE user_id = ? ORDER BY category COLLATE NOCASE, name COLLATE NOCASE, id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		var x Source
		if err := rows.Scan(&x.ID, &x.Kind, &x.Name, &x.Category, &x.Granularity, &x.MaxDepth, &x.Carryover, &x.PromptOverride, &x.Enabled, &x.SummarySource); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// Update replaces the settings of one of userID's sources.
func (s *Service) Update(ctx context.Context, userID, id string, in Settings) error {
	in.PromptOverride = strings.TrimSpace(in.PromptOverride)
	if in.SummarySource == "" {
		in.SummarySource = "agent"
	}
	if err := in.Validate(); err != nil {
		return err
	}
	var kind string
	switch err := s.db.QueryRowContext(ctx, `SELECT kind FROM sources WHERE id = ? AND user_id = ?`, id, userID).Scan(&kind); {
	case errors.Is(err, sql.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return err
	}
	if kind == "karakeep" {
		// Bookmarks Karakeep collected are summarized by Karakeep, one at a time.
		if in.Granularity == "digest" {
			return errors.New("Karakeep bookmarks are triaged one at a time, so they cannot be a digest")
		}
		in.SummarySource = "karakeep"
	}
	var prompt any
	if in.PromptOverride != "" {
		prompt = in.PromptOverride
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE sources SET granularity = ?, max_depth = ?, carryover = ?, prompt_override = ?, enabled = ?, summary_source = ?
		 WHERE id = ? AND user_id = ?`,
		in.Granularity, in.MaxDepth, in.Carryover, prompt, in.Enabled, in.SummarySource, id, userID)
	if err != nil {
		return fmt.Errorf("update source: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if in.Granularity == "individual" {
		// Entries that were waiting for a digest (and are not in one) would
		// otherwise never be offered or shown: an individual source triages on
		// the excerpt, as soon as there is one.
		day, err := LocalDay(ctx, s.db, userID, time.Now())
		if err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx,
			`UPDATE items SET state = 'light', batch_date = ?, updated_at = ?
			 WHERE user_id = ? AND source_id = ? AND state = 'pending_light' AND digest_id IS NULL`,
			day, time.Now().UTC().Format(time.RFC3339), userID, id); err != nil {
			return fmt.Errorf("release waiting entries: %w", err)
		}
	}
	return nil
}

// Defaults for the per-user ingest settings stored in user_settings.
const (
	DefaultExcerptLength = 500
	DefaultMaxAgeDays    = 14

	// Karakeep summaries: how long Claude's promote summary waits after the
	// bookmark is made (so Karakeep's own summarizer cannot overwrite it), and
	// how long a promoted item waits for Karakeep's summary before it is shown
	// with its excerpt alone. Both are guesses to tune against a real instance.
	DefaultSummaryDelayMinutes = 30
	DefaultSummaryTimeoutHours = 2

	// SettingSummaryDelayMinutes and SettingSummaryTimeoutHours are the
	// user_settings keys of the two values above.
	SettingSummaryDelayMinutes = "karakeep_summary_delay_minutes"
	SettingSummaryTimeoutHours = "karakeep_summary_timeout_hours"
)

// IngestSettings are the user-level knobs that apply to every source.
type IngestSettings struct {
	ExcerptLength int // characters kept in an item's excerpt
	MaxAgeDays    int // skip entries published longer ago; 0 means no limit

	KarakeepSummaryDelayMinutes int // Claude's summary waits this long after the bookmark is made
	KarakeepSummaryTimeoutHours int // give up on Karakeep's summary after this long
}

// Validate bounds both values.
func (s IngestSettings) Validate() error {
	switch {
	case s.ExcerptLength < 50 || s.ExcerptLength > 5000:
		return errors.New("excerpt length must be between 50 and 5000")
	case s.MaxAgeDays < 0 || s.MaxAgeDays > 3650:
		return errors.New("max age must be between 0 and 3650 days")
	case s.KarakeepSummaryDelayMinutes < 0 || s.KarakeepSummaryDelayMinutes > 1440:
		return errors.New("summary delay must be between 0 and 1440 minutes")
	case s.KarakeepSummaryTimeoutHours < 1 || s.KarakeepSummaryTimeoutHours > 168:
		return errors.New("summary timeout must be between 1 and 168 hours")
	}
	return nil
}

// Ingest returns userID's ingest settings, filling defaults.
func (s *Service) Ingest(ctx context.Context, userID string) (IngestSettings, error) {
	out := IngestSettings{ExcerptLength: DefaultExcerptLength, MaxAgeDays: DefaultMaxAgeDays,
		KarakeepSummaryDelayMinutes: DefaultSummaryDelayMinutes, KarakeepSummaryTimeoutHours: DefaultSummaryTimeoutHours}
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, value FROM user_settings WHERE user_id = ?
		 AND key IN ('excerpt_length', 'max_age_days', ?, ?)`, userID, SettingSummaryDelayMinutes, SettingSummaryTimeoutHours)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return out, err
		}
		n, convErr := strconv.Atoi(v)
		if convErr != nil {
			continue
		}
		switch k {
		case "excerpt_length":
			out.ExcerptLength = n
		case "max_age_days":
			out.MaxAgeDays = n
		case SettingSummaryDelayMinutes:
			out.KarakeepSummaryDelayMinutes = n
		case SettingSummaryTimeoutHours:
			out.KarakeepSummaryTimeoutHours = n
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	// A stored value outside the valid range falls back to the default.
	if out.ExcerptLength < 1 {
		out.ExcerptLength = DefaultExcerptLength
	}
	if out.MaxAgeDays < 0 {
		out.MaxAgeDays = DefaultMaxAgeDays
	}
	if out.KarakeepSummaryDelayMinutes < 0 {
		out.KarakeepSummaryDelayMinutes = DefaultSummaryDelayMinutes
	}
	if out.KarakeepSummaryTimeoutHours < 1 {
		out.KarakeepSummaryTimeoutHours = DefaultSummaryTimeoutHours
	}
	return out, nil
}

// SetIngest saves userID's ingest settings.
func (s *Service) SetIngest(ctx context.Context, userID string, in IngestSettings) error {
	if err := in.Validate(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, v := range map[string]int{
		"excerpt_length": in.ExcerptLength, "max_age_days": in.MaxAgeDays,
		SettingSummaryDelayMinutes: in.KarakeepSummaryDelayMinutes, SettingSummaryTimeoutHours: in.KarakeepSummaryTimeoutHours,
	} {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO user_settings (user_id, key, value) VALUES (?, ?, ?)
			 ON CONFLICT (user_id, key) DO UPDATE SET value = excluded.value`, userID, k, strconv.Itoa(v)); err != nil {
			return fmt.Errorf("save setting %s: %w", k, err)
		}
	}
	return tx.Commit()
}

// LocalDay returns userID's local date (YYYY-MM-DD) at now, which is the day
// an item is grouped under. An unreadable stored timezone falls back to UTC
// rather than failing an ingest.
func LocalDay(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, userID string, now time.Time) (string, error) {
	var name string
	if err := q.QueryRowContext(ctx, `SELECT timezone FROM users WHERE id = ?`, userID).Scan(&name); err != nil {
		return "", err
	}
	loc, err := time.LoadLocation(name)
	if err != nil || name == "" || name == "Local" {
		loc = time.UTC
	}
	return now.In(loc).Format("2006-01-02"), nil
}
