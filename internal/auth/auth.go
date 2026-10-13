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

	"github.com/Jolls/Siftstr/internal/ids"
)

const (
	// SessionLifetime is long so an expiring session mid-triage is rare.
	SessionLifetime = 30 * 24 * time.Hour

	// SessionIdleTimeout ends a session that has not been used for this long,
	// even before SessionLifetime. A phone used once a day stays signed in.
	SessionIdleTimeout = 7 * 24 * time.Hour

	minPasswordLen = 8

	loginWindow = 15 * time.Minute
	// maxPairFailures blocks one (username, client IP) pair, so failures from
	// one source cannot lock a user out everywhere.
	maxPairFailures = 5
	maxIPFailures   = 20

	// Failures for one username from every source slow its logins down
	// instead of blocking them: from userDelayAfter failures in the window each
	// attempt waits userDelayBase, doubling per further failure up to
	// userDelayMax, which it reaches at userDelayKeep failures.
	userDelayAfter = 10
	userDelayBase  = time.Second
	userDelayMax   = 30 * time.Second
	userDelayKeep  = 15

	// maxUsernameLen caps usernames in bytes, at creation and before a login
	// name is used as a limiter key.
	maxUsernameLen = 64

	// maxConcurrentHashes caps password checks running at once. Each argon2id
	// check takes 64 MiB, so this bounds login memory.
	maxConcurrentHashes = 2
)

var (
	ErrInvalidCredentials = errors.New("invalid username or password")
	ErrRateLimited        = errors.New("too many login attempts; try again later")
	ErrNoSession          = errors.New("no valid session")
	ErrUserExists         = errors.New("user already exists")
	ErrUnknownUser        = errors.New("unknown user")
	ErrWeakPassword       = fmt.Errorf("password must be at least %d characters", minPasswordLen)
	ErrUsernameTooLong    = fmt.Errorf("username must be at most %d bytes", maxUsernameLen)
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
	byPair    *limiter                                     // failures per (username, client IP key); blocks at maxPairFailures
	byIP      *limiter                                     // failures per client IP key (IPv6 by /64); blocks at maxIPFailures
	byUser    *limiter                                     // failures per username from every source; drives userDelay
	sleep     func(context.Context, time.Duration) error   // sleepCtx; replaced in tests
	verify    func(password, encoded string) (bool, error) // VerifyPassword; replaced in tests
	hashSlots chan struct{}                                // one token per running password check
	dummyHash string                                       // verified against for unknown users, to even out timing
	defaultTZ string                                       // timezone given to new users
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
		byPair:    newLimiter(loginWindow, maxPairFailures),
		byIP:      newLimiter(loginWindow, maxIPFailures),
		byUser:    newLimiter(loginWindow, userDelayKeep),
		sleep:     sleepCtx,
		verify:    VerifyPassword,
		hashSlots: make(chan struct{}, maxConcurrentHashes),
		dummyHash: dummy,
		defaultTZ: "UTC",
	}, nil
}

// SetDefaultTimezone sets the timezone new users start with (the TZ setting).
// An unknown name is ignored and UTC stays, so a typo in TZ cannot stop
// users from being created.
func (s *Service) SetDefaultTimezone(name string) {
	if name == "" || name == "Local" {
		return
	}
	if _, err := time.LoadLocation(name); err == nil {
		s.defaultTZ = name
	}
}

func normalize(username string) string { return strings.ToLower(strings.TrimSpace(username)) }

// loginName normalizes a submitted username and caps it at maxUsernameLen
// bytes. tooLong means no user can have this name. At most 4*maxUsernameLen
// bytes are lowercased, so a huge field costs nothing.
func loginName(raw string) (name string, tooLong bool) {
	raw = strings.TrimSpace(raw)
	if len(raw) > 4*maxUsernameLen {
		raw = raw[:4*maxUsernameLen]
	}
	name = strings.ToLower(raw)
	if len(name) > maxUsernameLen {
		return name[:maxUsernameLen], true
	}
	return name, false
}

// userDelay is how long a login waits after n recent failures for its
// username from every source.
func userDelay(n int) time.Duration {
	if n < userDelayAfter {
		return 0
	}
	if n >= userDelayKeep {
		return userDelayMax
	}
	return userDelayBase << (n - userDelayAfter)
}

// sleepCtx waits for d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
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
	if len(username) > maxUsernameLen {
		return User{}, ErrUsernameTooLong
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
	id, err := ids.New("usr_")
	if err != nil {
		return User{}, err
	}
	u := User{ID: id, Username: username, Role: role, Timezone: s.defaultTZ}
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

// Authenticate checks a login. Failures are counted per (username, client
// IP) pair and per client IP, where an IPv6 client counts as its /64; either
// limit refuses the attempt with ErrRateLimited. Failures for a username from
// every source never refuse it: they delay each attempt (userDelay), before a
// hash slot is taken, so no one can lock a user out. At most
// maxConcurrentHashes checks run at once; when all are busy the attempt fails
// at once with ErrRateLimited and is not counted. Each attempt counts before
// its password is checked. The error is the same for unknown users, wrong
// passwords and disabled accounts.
func (s *Service) Authenticate(ctx context.Context, username, password, ip string) (User, error) {
	username, tooLong := loginName(username)
	ipk := ipKey(ip)
	pair := username + "\x00" + ipk
	now := s.now()
	if s.byPair.blocked(pair, now) || s.byIP.blocked(ipk, now) {
		return User{}, ErrRateLimited
	}
	if d := userDelay(s.byUser.count(username, now)); d > 0 {
		if err := s.sleep(ctx, d); err != nil {
			return User{}, err
		}
	}
	select {
	case s.hashSlots <- struct{}{}:
		defer func() { <-s.hashSlots }()
	default:
		return User{}, ErrRateLimited
	}
	now = s.now()
	if !s.byPair.reserve(pair, now) {
		return User{}, ErrRateLimited
	}
	if !s.byIP.reserve(ipk, now) {
		s.byPair.release(pair, now)
		return User{}, ErrRateLimited
	}
	s.byUser.record(username, now)

	var (
		u        User
		hash     string
		disabled bool
		known    bool
	)
	if !tooLong {
		err := s.db.QueryRowContext(ctx,
			`SELECT id, username, role, timezone, password_hash, disabled FROM users WHERE username = ?`, username).
			Scan(&u.ID, &u.Username, &u.Role, &u.Timezone, &hash, &disabled)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			s.byPair.release(pair, now)
			s.byIP.release(ipk, now)
			s.byUser.release(username, now)
			return User{}, err
		}
		known = err == nil
	}
	if !known {
		hash = s.dummyHash
	}
	ok, verr := s.verify(password, hash)
	if verr != nil || !ok || !known || disabled {
		return User{}, ErrInvalidCredentials // the reservations stand as the failures
	}
	s.byPair.reset(pair)
	s.byIP.release(ipk, now)
	s.byUser.release(username, now)
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

// UserForSession resolves a cookie token to its user. Expired sessions,
// sessions unused for SessionIdleTimeout, and disabled users yield
// ErrNoSession.
func (s *Service) UserForSession(ctx context.Context, token string) (User, error) {
	if token == "" {
		return User{}, ErrNoSession
	}
	var (
		u        User
		expires  string
		lastSeen string
		disabled bool
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT u.id, u.username, u.role, u.timezone, u.disabled, s.expires_at, s.last_seen_at
		   FROM sessions s JOIN users u ON u.id = s.user_id
		  WHERE s.id = ?`, hashToken(token)).
		Scan(&u.ID, &u.Username, &u.Role, &u.Timezone, &disabled, &expires, &lastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNoSession
	}
	if err != nil {
		return User{}, err
	}
	now := s.now()
	exp, perr := time.Parse(time.RFC3339, expires)
	seen, serr := time.Parse(time.RFC3339, lastSeen)
	if perr != nil || serr != nil || !now.Before(exp) || !now.Before(seen.Add(SessionIdleTimeout)) || disabled {
		return User{}, ErrNoSession
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE id = ?`, s.stamp(), hashToken(token))
	return u, nil
}

// EndAllSessions deletes every session of userID, including the caller's,
// and reports how many it ended.
func (s *Service) EndAllSessions(ctx context.Context, userID string) (int64, error) {
	if userID == "" {
		return 0, errors.New("auth: user ID required")
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	if err != nil {
		return 0, fmt.Errorf("end sessions: %w", err)
	}
	return res.RowsAffected()
}

// Session describes one of a user's active sessions. It never carries the
// token or its hash.
type Session struct {
	CreatedAt, LastSeenAt, ExpiresAt time.Time
	Current                          bool // the session whose token was passed in
}

// ListSessions returns userID's sessions that are neither expired nor idle,
// most recently seen first. currentToken marks the caller's own session.
func (s *Service) ListSessions(ctx context.Context, userID, currentToken string) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, created_at, last_seen_at, expires_at FROM sessions
		  WHERE user_id = ? AND expires_at > ? AND last_seen_at > ?
		  ORDER BY last_seen_at DESC, created_at DESC`,
		userID, s.stamp(), s.now().Add(-SessionIdleTimeout).UTC().Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	cur := hashToken(currentToken)
	var out []Session
	for rows.Next() {
		var id, created, seen, exp string
		if err := rows.Scan(&id, &created, &seen, &exp); err != nil {
			return nil, err
		}
		ss := Session{Current: id == cur}
		ss.CreatedAt, _ = time.Parse(time.RFC3339, created)
		ss.LastSeenAt, _ = time.Parse(time.RFC3339, seen)
		ss.ExpiresAt, _ = time.Parse(time.RFC3339, exp)
		out = append(out, ss)
	}
	return out, rows.Err()
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
