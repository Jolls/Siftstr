// Package auth handles users, passwords, sessions and CSRF tokens.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// SessionLifetime is long so an expiring session mid-triage is rare.
	SessionLifetime = 30 * 24 * time.Hour

	minPasswordLen = 8

	loginWindow     = 15 * time.Minute
	maxUserFailures = 5
	maxIPFailures   = 20
)

var (
	ErrInvalidCredentials = errors.New("invalid username or password")
	ErrRateLimited        = errors.New("too many failed logins; try again later")
	ErrNoSession          = errors.New("no valid session")
	ErrUserExists         = errors.New("user already exists")
	ErrUnknownUser        = errors.New("unknown user")
	ErrWeakPassword       = fmt.Errorf("password must be at least %d characters", minPasswordLen)
)

// Roles.
const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

// User is an account. It never carries the password hash.
type User struct {
	ID       string
	Username string
	Role     string
	Timezone string
}

// Service is the auth API. Every method is scoped by the user it is given.
type Service struct {
	db        *sql.DB
	csrfKey   []byte
	now       func() time.Time
	byUser    *limiter
	byIP      *limiter
	dummyHash string // verified against for unknown users, to even out timing
}

// New returns a Service. csrfKey signs CSRF tokens (see secret.Derive).
func New(db *sql.DB, csrfKey []byte) (*Service, error) {
	dummy, err := HashPassword("not-a-real-password")
	if err != nil {
		return nil, err
	}
	return &Service{
		db:        db,
		csrfKey:   csrfKey,
		now:       time.Now,
		byUser:    newLimiter(loginWindow, maxUserFailures),
		byIP:      newLimiter(loginWindow, maxIPFailures),
		dummyHash: dummy,
	}, nil
}

func normalize(username string) string { return strings.ToLower(strings.TrimSpace(username)) }

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func newID(prefix string) (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Service) stamp() string { return s.now().UTC().Format(time.RFC3339) }

// CreateUser adds a user. Usernames are case-insensitive.
func (s *Service) CreateUser(ctx context.Context, username, password, role string) (User, error) {
	username = normalize(username)
	if username == "" {
		return User{}, errors.New("username is required")
	}
	if role != RoleAdmin && role != RoleUser {
		return User{}, fmt.Errorf("unknown role %q", role)
	}
	if len(password) < minPasswordLen {
		return User{}, ErrWeakPassword
	}
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, err
	}
	id, err := newID("usr_")
	if err != nil {
		return User{}, err
	}
	u := User{ID: id, Username: username, Role: role, Timezone: "UTC"}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO users (id, username, password_hash, role, timezone, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		u.ID, u.Username, hash, u.Role, u.Timezone, s.stamp())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return User{}, ErrUserExists
		}
		return User{}, fmt.Errorf("create user: %w", err)
	}
	return u, nil
}

// SetPassword replaces a user's password and ends all of their sessions.
func (s *Service) SetPassword(ctx context.Context, username, password string) error {
	if len(password) < minPasswordLen {
		return ErrWeakPassword
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM users WHERE username = ?`, normalize(username)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUnknownUser
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, hash, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// Bootstrap creates the first admin when the users table is empty. It does
// nothing, and reports false, when any user exists or no credentials are set.
func (s *Service) Bootstrap(ctx context.Context, username, password string) (bool, error) {
	if username == "" || password == "" {
		return false, nil
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	if _, err := s.CreateUser(ctx, username, password, RoleAdmin); err != nil {
		return false, fmt.Errorf("bootstrap admin: %w", err)
	}
	return true, nil
}

// Authenticate checks a login. Failures are counted per username and per IP.
// The error is the same for unknown users, wrong passwords and disabled
// accounts.
func (s *Service) Authenticate(ctx context.Context, username, password, ip string) (User, error) {
	username = normalize(username)
	now := s.now()
	if s.byUser.blocked(username, now) || s.byIP.blocked(ip, now) {
		return User{}, ErrRateLimited
	}

	var (
		u        User
		hash     string
		disabled bool
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, role, timezone, password_hash, disabled FROM users WHERE username = ?`, username).
		Scan(&u.ID, &u.Username, &u.Role, &u.Timezone, &hash, &disabled)
	known := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return User{}, err
	}
	if !known {
		hash = s.dummyHash
	}
	ok, verr := VerifyPassword(password, hash)
	if verr != nil || !ok || !known || disabled {
		s.byUser.fail(username, now)
		s.byIP.fail(ip, now)
		return User{}, ErrInvalidCredentials
	}
	s.byUser.reset(username)
	return u, nil
}

// NewSession starts a session and returns the token for the cookie. Only a
// hash of the token is stored.
func (s *Service) NewSession(ctx context.Context, userID string) (string, time.Time, error) {
	token, err := randomToken(32)
	if err != nil {
		return "", time.Time{}, err
	}
	now := s.now().UTC()
	expires := now.Add(SessionLifetime)
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, user_id, created_at, expires_at, last_seen_at) VALUES (?, ?, ?, ?, ?)`,
		hashToken(token), userID, now.Format(time.RFC3339), expires.Format(time.RFC3339), now.Format(time.RFC3339))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("create session: %w", err)
	}
	return token, expires, nil
}

// UserForSession resolves a cookie token to its user. Expired sessions and
// disabled users yield ErrNoSession.
func (s *Service) UserForSession(ctx context.Context, token string) (User, error) {
	if token == "" {
		return User{}, ErrNoSession
	}
	var (
		u        User
		expires  string
		disabled bool
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT u.id, u.username, u.role, u.timezone, u.disabled, s.expires_at
		   FROM sessions s JOIN users u ON u.id = s.user_id
		  WHERE s.id = ?`, hashToken(token)).
		Scan(&u.ID, &u.Username, &u.Role, &u.Timezone, &disabled, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNoSession
	}
	if err != nil {
		return User{}, err
	}
	exp, perr := time.Parse(time.RFC3339, expires)
	if perr != nil || !s.now().Before(exp) || disabled {
		return User{}, ErrNoSession
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE id = ?`, s.stamp(), hashToken(token))
	return u, nil
}

// EndSession deletes the session for token, if any.
func (s *Service) EndSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, hashToken(token))
	return err
}

// CSRFToken returns the CSRF token bound to a session token.
func (s *Service) CSRFToken(sessionToken string) string {
	m := hmac.New(sha256.New, s.csrfKey)
	m.Write([]byte(hashToken(sessionToken)))
	return hex.EncodeToString(m.Sum(nil))
}

// CheckCSRF reports whether got is the CSRF token for the session.
func (s *Service) CheckCSRF(sessionToken, got string) bool {
	if sessionToken == "" || got == "" {
		return false
	}
	return hmac.Equal([]byte(s.CSRFToken(sessionToken)), []byte(got))
}
