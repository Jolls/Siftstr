package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// KeyPrefix starts every API key, so leaked keys are easy to spot and scan for.
const KeyPrefix = "sft_"

// ErrNotFound is returned when a row does not exist for the given user. It is
// the same error whether the row is missing or belongs to someone else.
var ErrNotFound = errors.New("not found")

// APIKey describes a key without its secret or hash.
type APIKey struct {
	ID         string
	Label      string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// UserByUsername looks a user up by name.
func (s *Service) UserByUsername(ctx context.Context, username string) (User, error) {
	var u User
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, role, timezone FROM users WHERE username = ?`, normalize(username)).
		Scan(&u.ID, &u.Username, &u.Role, &u.Timezone)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrUnknownUser
	}
	return u, err
}

// CreateAPIKey makes a key for userID. The returned secret is shown once;
// only its hash is stored.
func (s *Service) CreateAPIKey(ctx context.Context, userID, label string) (APIKey, string, error) {
	secret, err := randomToken(32)
	if err != nil {
		return APIKey{}, "", err
	}
	secret = KeyPrefix + secret
	id, err := newID("key_")
	if err != nil {
		return APIKey{}, "", err
	}
	now := s.now().UTC()
	label = strings.TrimSpace(label)
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO api_keys (id, user_id, key_hash, label, created_at) VALUES (?, ?, ?, ?, ?)`,
		id, userID, hashToken(secret), label, now.Format(time.RFC3339))
	if err != nil {
		return APIKey{}, "", fmt.Errorf("create api key: %w", err)
	}
	return APIKey{ID: id, Label: label, CreatedAt: now}, secret, nil
}

// ListAPIKeys returns userID's keys, newest first.
func (s *Service) ListAPIKeys(ctx context.Context, userID string) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, label, created_at, last_used_at, revoked_at
		   FROM api_keys WHERE user_id = ? ORDER BY created_at DESC, id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []APIKey
	for rows.Next() {
		var (
			k                 APIKey
			created           string
			lastUsed, revoked sql.NullString
		)
		if err := rows.Scan(&k.ID, &k.Label, &created, &lastUsed, &revoked); err != nil {
			return nil, err
		}
		k.CreatedAt, _ = time.Parse(time.RFC3339, created)
		k.LastUsedAt = parseNullTime(lastUsed)
		k.RevokedAt = parseNullTime(revoked)
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func parseNullTime(ns sql.NullString) *time.Time {
	if !ns.Valid {
		return nil
	}
	t, err := time.Parse(time.RFC3339, ns.String)
	if err != nil {
		return nil
	}
	return &t
}

// RevokeAPIKey revokes keyID if it belongs to userID. Another user's key, or
// an unknown one, yields ErrNotFound. Revoking twice is harmless.
func (s *Service) RevokeAPIKey(ctx context.Context, userID, keyID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE api_keys SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ? AND user_id = ?`,
		s.stamp(), keyID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UserForAPIKey resolves a presented key to its owner. Revoked keys, unknown
// keys and disabled users all yield ErrNoSession.
func (s *Service) UserForAPIKey(ctx context.Context, key string) (User, error) {
	if !strings.HasPrefix(key, KeyPrefix) {
		return User{}, ErrNoSession
	}
	hash := hashToken(key)
	var (
		u        User
		disabled bool
		revoked  sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT u.id, u.username, u.role, u.timezone, u.disabled, k.revoked_at
		   FROM api_keys k JOIN users u ON u.id = k.user_id
		  WHERE k.key_hash = ?`, hash).
		Scan(&u.ID, &u.Username, &u.Role, &u.Timezone, &disabled, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNoSession
	}
	if err != nil {
		return User{}, err
	}
	if revoked.Valid || disabled {
		return User{}, ErrNoSession
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE api_keys SET last_used_at = ? WHERE key_hash = ?`, s.stamp(), hash)
	return u, nil
}
