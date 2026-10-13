package connections

import (
	"context"
	"fmt"
)

// Seed is a connection supplied by instance config.
type Seed struct {
	Kind    string
	BaseURL string
	Secret  string // empty means no key supplied
}

// SeedResult says what ApplySeed changed. It never carries the secret.
type SeedResult struct {
	Created, URLUpdated, SecretSet bool
}

// ApplySeed makes userID's connection of in.Kind match instance config: it
// creates the connection when the user has none of that kind; otherwise it
// sets the base URL when it differs and saves the secret only when the
// connection has none, so a key entered in the UI is never replaced.
// It never creates a second connection of a kind.
func (s *Service) ApplySeed(ctx context.Context, userID string, in Seed) (SeedResult, error) {
	if in.BaseURL == "" {
		return SeedResult{}, fmt.Errorf("%w is required", ErrBaseURL)
	}
	list, err := s.List(ctx, userID)
	if err != nil {
		return SeedResult{}, err
	}
	for _, c := range list {
		if c.Kind != in.Kind {
			continue
		}
		var upd UpdateInput
		var res SeedResult
		if c.BaseURL != in.BaseURL {
			upd.BaseURL, res.URLUpdated = &in.BaseURL, true
		}
		if !c.HasSecret && in.Secret != "" {
			upd.Secret, res.SecretSet = &in.Secret, true
		}
		if res == (SeedResult{}) {
			return res, nil
		}
		if _, err := s.Update(ctx, userID, c.ID, upd); err != nil {
			return SeedResult{}, err
		}
		if res.URLUpdated {
			// The last check described the old address.
			if _, err := s.db.ExecContext(ctx, `UPDATE connections SET check_state = NULL, check_detail = '', check_via = '', checked_at = NULL WHERE id = ? AND user_id = ?`, c.ID, userID); err != nil {
				return SeedResult{}, err
			}
		}
		return res, nil
	}
	if _, err := s.Create(ctx, userID, Input{Kind: in.Kind, BaseURL: in.BaseURL, Secret: &in.Secret}); err != nil {
		return SeedResult{}, err
	}
	return SeedResult{Created: true, SecretSet: in.Secret != ""}, nil
}
