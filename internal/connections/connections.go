// Package connections stores each user's upstream credentials (Miniflux,
// Karakeep, MeTube, ...), encrypted at rest.
//
// The Connection type deliberately has no secret field, so nothing that
// lists, renders or serializes a connection can leak one. Code that must call
// an upstream asks for the plaintext with Service.Secret, which is the only
// way to read it back.
package connections

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/Jolls/Siftstr/internal/ids"
)

// kindSchemes lists the URL schemes of kinds that don't use http(s).
var kindSchemes = map[string][]string{"nostr": {"ws", "wss"}}

// Kinds are the upstream types a connection can have (see migration 001).
var Kinds = []string{"miniflux", "karakeep", "metube", "youtube", "nextcloud", "nostr"}

var (
	// ErrBaseURL wraps every base URL validation failure, so callers can tell bad
	// input from a database error. Its text starts the user-facing message.
	ErrBaseURL = errors.New("base URL")
	// ErrNotFound is returned for a missing connection or one owned by
	// another user; the two are indistinguishable on purpose.
	ErrNotFound = errors.New("connection not found")
	// ErrUnreadable means the stored secret cannot be decrypted, usually
	// because secret.key changed. The user has to enter it again.
	ErrUnreadable = errors.New("saved secret cannot be decrypted; re-enter it")
	ErrNoSecret   = errors.New("connection has no saved secret")
)

// Connection is a saved upstream, without its secret.
type Connection struct {
	ID        string
	Kind      string
	BaseURL   string
	Config    json.RawMessage // non-secret settings, a JSON object
	HasSecret bool
}

// Input is what a user submits to Create.
type Input struct {
	Kind    string
	BaseURL string
	Secret  *string
	Config  json.RawMessage // optional; defaults to {}
}

// UpdateInput is a partial update: a nil field keeps the saved value. A
// connection's kind never changes. An empty Secret also keeps the saved one,
// so a blank password field in a form cannot wipe a credential; use
// ClearSecret to remove it on purpose. ClearSecret wins over Secret.
type UpdateInput struct {
	BaseURL     *string
	Config      json.RawMessage
	Secret      *string
	ClearSecret bool
}

// Service reads and writes connections.
type Service struct {
	db   *sql.DB
	aead cipher.AEAD
}

// New returns a Service. key must be 32 bytes (see secret.Derive).
func New(db *sql.DB, key []byte) (*Service, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("connections key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("connections key: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Service{db: db, aead: aead}, nil
}

// aad ties a ciphertext to one row and one owner, so copying it elsewhere
// (another row, another user) makes it undecryptable.
func aad(userID, id string) []byte { return []byte(userID + "\x00" + id) }

func (s *Service) seal(userID, id, plaintext string) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, []byte(plaintext), aad(userID, id)), nil
}

func (s *Service) open(userID, id string, blob []byte) (string, error) {
	n := s.aead.NonceSize()
	if len(blob) < n {
		return "", ErrUnreadable
	}
	plain, err := s.aead.Open(nil, blob[:n], blob[n:], aad(userID, id))
	if err != nil {
		return "", ErrUnreadable
	}
	return string(plain), nil
}

func validateURL(kind, baseURL string) error {
	if baseURL == "" {
		return nil
	}
	schemes := kindSchemes[kind]
	if schemes == nil {
		schemes = []string{"http", "https"}
	}
	u, err := url.Parse(baseURL)
	if err != nil || !slices.Contains(schemes, u.Scheme) || u.Host == "" {
		return fmt.Errorf("%w must be a %s URL", ErrBaseURL, strings.Join(schemes, "/"))
	}
	// Credentials belong in the encrypted secret, not in a column that Get and
	// List return as-is.
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%w must not contain credentials, a query string or a fragment", ErrBaseURL)
	}
	return nil
}

func validateConfig(config json.RawMessage) (json.RawMessage, error) {
	if len(config) == 0 {
		return json.RawMessage("{}"), nil
	}
	var obj map[string]any
	if err := json.Unmarshal(config, &obj); err != nil || obj == nil {
		return nil, errors.New("config must be a JSON object")
	}
	return config, nil
}

// Create saves a new connection for userID.
func (s *Service) Create(ctx context.Context, userID string, in Input) (Connection, error) {
	if !slices.Contains(Kinds, in.Kind) {
		return Connection{}, fmt.Errorf("unknown connection kind %q", in.Kind)
	}
	if err := validateURL(in.Kind, in.BaseURL); err != nil {
		return Connection{}, err
	}
	cfg, err := validateConfig(in.Config)
	if err != nil {
		return Connection{}, err
	}
	id, err := ids.New("con_")
	if err != nil {
		return Connection{}, err
	}
	var blob []byte
	if in.Secret != nil && *in.Secret != "" {
		if blob, err = s.seal(userID, id, *in.Secret); err != nil {
			return Connection{}, err
		}
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO connections (id, user_id, kind, base_url, secret_enc, config_json) VALUES (?, ?, ?, ?, ?, ?)`,
		id, userID, in.Kind, in.BaseURL, blob, string(cfg))
	if err != nil {
		return Connection{}, fmt.Errorf("create connection: %w", err)
	}
	return Connection{ID: id, Kind: in.Kind, BaseURL: in.BaseURL, Config: cfg, HasSecret: blob != nil}, nil
}

// Update applies a partial update to one of userID's connections.
func (s *Service) Update(ctx context.Context, userID, id string, in UpdateInput) (Connection, error) {
	cur, err := s.Get(ctx, userID, id)
	if err != nil {
		return Connection{}, err
	}
	if in.BaseURL != nil {
		if err := validateURL(cur.Kind, *in.BaseURL); err != nil {
			return Connection{}, err
		}
		cur.BaseURL = *in.BaseURL
	}
	if in.Config != nil {
		if cur.Config, err = validateConfig(in.Config); err != nil {
			return Connection{}, err
		}
	}
	set, args := "base_url = ?, config_json = ?", []any{cur.BaseURL, string(cur.Config)}
	switch {
	case in.ClearSecret:
		set += ", secret_enc = NULL"
		cur.HasSecret = false
	case in.Secret != nil && *in.Secret != "":
		blob, err := s.seal(userID, id, *in.Secret)
		if err != nil {
			return Connection{}, err
		}
		set += ", secret_enc = ?"
		args = append(args, blob)
		cur.HasSecret = true
	}
	args = append(args, id, userID)
	res, err := s.db.ExecContext(ctx,
		"UPDATE connections SET "+set+" WHERE id = ? AND user_id = ?", args...)
	if err != nil {
		return Connection{}, fmt.Errorf("update connection: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Connection{}, ErrNotFound
	}
	return cur, nil
}

// Get returns one of userID's connections.
func (s *Service) Get(ctx context.Context, userID, id string) (Connection, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, kind, base_url, config_json, secret_enc IS NOT NULL FROM connections WHERE id = ? AND user_id = ?`,
		id, userID)
	c, err := scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Connection{}, ErrNotFound
	}
	return c, err
}

// List returns userID's connections ordered by kind.
func (s *Service) List(ctx context.Context, userID string) ([]Connection, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, kind, base_url, config_json, secret_enc IS NOT NULL FROM connections WHERE user_id = ? ORDER BY kind, id`,
		userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Connection
	for rows.Next() {
		c, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type scanner interface{ Scan(dest ...any) error }

func scan(r scanner) (Connection, error) {
	var (
		c   Connection
		cfg string
	)
	if err := r.Scan(&c.ID, &c.Kind, &c.BaseURL, &cfg, &c.HasSecret); err != nil {
		return Connection{}, err
	}
	c.Config = json.RawMessage(cfg)
	return c, nil
}

// Delete removes one of userID's connections.
func (s *Service) Delete(ctx context.Context, userID, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM connections WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Secret decrypts the saved secret. It exists for adapters that call the
// upstream; never pass its result to a template, log line or API response.
func (s *Service) Secret(ctx context.Context, userID, id string) (string, error) {
	var blob []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT secret_enc FROM connections WHERE id = ? AND user_id = ?`, id, userID).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if blob == nil {
		return "", ErrNoSecret
	}
	return s.open(userID, id, blob)
}
