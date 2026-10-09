// Package sources reads and edits a user's per-source triage settings.
package sources

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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
}

// Settings is what the user may change on a source. Name and category follow
// the upstream and are not editable here.
type Settings struct {
	Granularity    string
	MaxDepth       string
	Carryover      string
	PromptOverride string
	Enabled        bool
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
		`SELECT id, kind, name, category, granularity, max_depth, carryover, COALESCE(prompt_override, ''), enabled
		 FROM sources WHERE user_id = ? ORDER BY category COLLATE NOCASE, name COLLATE NOCASE, id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		var x Source
		if err := rows.Scan(&x.ID, &x.Kind, &x.Name, &x.Category, &x.Granularity, &x.MaxDepth, &x.Carryover, &x.PromptOverride, &x.Enabled); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// Update replaces the settings of one of userID's sources.
func (s *Service) Update(ctx context.Context, userID, id string, in Settings) error {
	in.PromptOverride = strings.TrimSpace(in.PromptOverride)
	if err := in.Validate(); err != nil {
		return err
	}
	var prompt any
	if in.PromptOverride != "" {
		prompt = in.PromptOverride
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE sources SET granularity = ?, max_depth = ?, carryover = ?, prompt_override = ?, enabled = ?
		 WHERE id = ? AND user_id = ?`,
		in.Granularity, in.MaxDepth, in.Carryover, prompt, in.Enabled, id, userID)
	if err != nil {
		return fmt.Errorf("update source: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
