package nostr

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Jolls/Siftstr/internal/connections"
	"github.com/Jolls/Siftstr/internal/ids"
	"github.com/Jolls/Siftstr/internal/sources"
)

// Limits on what one user can configure.
const (
	MaxRelays = 10
	MaxNpubs  = 200
)

// Category groups every Nostr source on the sources page.
const Category = "Nostr"

// viewerBase turns a note identifier into a link a browser or Karakeep can
// open. Items need an http(s) URL, and Nostr events have none of their own.
const viewerBase = "https://njump.me/"

// ErrNoConnection means the user has not saved a Nostr connection.
var ErrNoConnection = errors.New("no nostr connection")

// Config is the non-secret part of a user's Nostr connection.
type Config struct {
	Relays []string `json:"relays"` // ws:// or wss:// URLs, shared by every source
	Npubs  []string `json:"npubs"`  // followed public keys, lowercase hex
}

// ParseConfig reads a connection's config JSON.
func ParseConfig(raw json.RawMessage) Config {
	var c Config
	_ = json.Unmarshal(raw, &c)
	return c
}

// JSON returns the config as stored on the connection.
func (c Config) JSON() json.RawMessage {
	b, _ := json.Marshal(c)
	return b
}

// NewConfig validates what a user typed: relay URLs and npubs, one per line
// or separated by spaces or commas. Duplicates are dropped, order is kept.
func NewConfig(relays, npubs string) (Config, error) {
	var c Config
	for _, r := range splitList(relays) {
		u, err := url.Parse(r)
		if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" || u.User != nil || u.Fragment != "" {
			return Config{}, fmt.Errorf("%q is not a ws:// or wss:// relay URL", r)
		}
		if !slices.Contains(c.Relays, r) {
			c.Relays = append(c.Relays, r)
		}
	}
	for _, n := range splitList(npubs) {
		hexKey, err := ParsePubKey(n)
		if err != nil {
			return Config{}, fmt.Errorf("%q is not a valid npub", n)
		}
		if !slices.Contains(c.Npubs, hexKey) {
			c.Npubs = append(c.Npubs, hexKey)
		}
	}
	switch {
	case len(c.Relays) > MaxRelays:
		return Config{}, fmt.Errorf("at most %d relays", MaxRelays)
	case len(c.Npubs) > MaxNpubs:
		return Config{}, fmt.Errorf("at most %d npubs", MaxNpubs)
	}
	return c, nil
}

func splitList(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t' })
}

// Fetcher reads the events of some authors from some relays. Production code
// uses RelayFetcher; tests pass a fake, so no test needs a network.
type Fetcher interface {
	// Fetch returns kind 0, 1 and 30023 events by the given authors (hex)
	// created at or after since (zero means no limit). Relays that fail are
	// skipped; an error means none answered.
	Fetch(ctx context.Context, relays, authors []string, since time.Time) ([]Event, error)
}

// Ingester mirrors followed npubs into sources and their posts into items.
type Ingester struct {
	DB    *sql.DB
	Conns *connections.Service
	Fetch Fetcher
	Now   func() time.Time
}

// Result counts what one run changed.
type Result struct {
	Sources int // npubs mirrored into sources
	Items   int // new items inserted
	TooOld  int // events skipped for being older than max_age_days
	Invalid int // events dropped for a bad ID, bad signature or unexpected author
}

// Run ingests for one user. It never reads another user's rows.
func (g *Ingester) Run(ctx context.Context, userID string) (Result, error) {
	var res Result
	conn, err := g.connection(ctx, userID)
	if err != nil {
		return res, err
	}
	cfg := ParseConfig(conn.Config)
	if len(cfg.Relays) == 0 || len(cfg.Npubs) == 0 {
		return res, nil
	}
	ing, err := sources.New(g.DB).Ingest(ctx, userID)
	if err != nil {
		return res, err
	}
	now := g.Now().UTC()
	var cutoff time.Time // zero means no age limit
	if ing.MaxAgeDays > 0 {
		cutoff = now.AddDate(0, 0, -ing.MaxAgeDays)
	}
	events, err := g.Fetch.Fetch(ctx, cfg.Relays, cfg.Npubs, cutoff)
	if err != nil {
		return res, err
	}
	stamp := now.Format(time.RFC3339)

	tx, err := g.DB.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	names := map[string]string{} // hex pubkey -> newest profile name
	named := map[string]int64{}  // hex pubkey -> created_at of that profile
	var posts []Event
	seen := map[string]bool{}
	for _, e := range events {
		if !slices.Contains(cfg.Npubs, e.PubKey) || e.Verify() != nil {
			res.Invalid++
			continue
		}
		if seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		switch e.Kind {
		case KindProfile:
			if n := profileName(e.Content); n != "" && e.CreatedAt >= named[e.PubKey] {
				names[e.PubKey], named[e.PubKey] = n, e.CreatedAt
			}
		case KindNote, KindArticle:
			posts = append(posts, e)
		}
	}

	refs := make(map[string]sourceRef, len(cfg.Npubs))
	for _, pk := range cfg.Npubs {
		ref, err := upsertSource(ctx, tx, userID, pk, names[pk])
		if err != nil {
			return res, err
		}
		refs[pk] = ref
	}
	res.Sources = len(refs)

	insert, err := tx.PrepareContext(ctx,
		`INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, excerpt, content, published_at, state, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending_light', ?, ?)
		 ON CONFLICT (user_id, source_id, external_id) DO NOTHING`)
	if err != nil {
		return res, err
	}
	defer insert.Close()
	for _, e := range posts {
		ref := refs[e.PubKey]
		if !ref.enabled || strings.TrimSpace(e.Content) == "" {
			continue
		}
		if e.Kind == KindNote && e.IsReply() {
			continue
		}
		published := time.Unix(e.CreatedAt, 0).UTC()
		if !cutoff.IsZero() && published.Before(cutoff) {
			res.TooOld++
			continue
		}
		id, err := ids.New("itm_")
		if err != nil {
			return res, err
		}
		media := "post"
		if e.Kind == KindArticle {
			media = "article"
		}
		r, err := insert.ExecContext(ctx, id, userID, ref.id, e.ID, media, viewerBase+Note(e.ID),
			title(e), truncate(e.Content, ing.ExcerptLength), e.Content, published.Format(time.RFC3339), stamp, stamp)
		if err != nil {
			return res, fmt.Errorf("insert item: %w", err)
		}
		if n, _ := r.RowsAffected(); n > 0 {
			res.Items++
		}
	}
	return res, tx.Commit()
}

func (g *Ingester) connection(ctx context.Context, userID string) (connections.Connection, error) {
	list, err := g.Conns.List(ctx, userID)
	if err != nil {
		return connections.Connection{}, err
	}
	for _, c := range list {
		if c.Kind == "nostr" {
			return c, nil
		}
	}
	return connections.Connection{}, ErrNoConnection
}

type sourceRef struct {
	id      string
	enabled bool
}

// upsertSource creates the source for one npub. Its name is the profile name
// when one was seen, else the shortened npub; a name seen earlier is kept.
// The user's per-source settings are never touched.
func upsertSource(ctx context.Context, tx *sql.Tx, userID, pubkey, profile string) (sourceRef, error) {
	id, err := ids.New("src_")
	if err != nil {
		return sourceRef{}, err
	}
	name := profile
	if name == "" {
		name = shortNpub(pubkey)
	}
	var ref sourceRef
	err = tx.QueryRowContext(ctx,
		`INSERT INTO sources (id, user_id, kind, external_id, name, category) VALUES (?, ?, 'nostr', ?, ?, ?)
		 ON CONFLICT (user_id, kind, external_id) DO UPDATE SET category = excluded.category,
		   name = CASE WHEN ? != '' THEN excluded.name ELSE sources.name END
		 RETURNING id, enabled`,
		id, userID, pubkey, name, Category, profile).Scan(&ref.id, &ref.enabled)
	if err != nil {
		return sourceRef{}, fmt.Errorf("upsert source: %w", err)
	}
	return ref, nil
}

func shortNpub(pubkey string) string {
	n := Npub(pubkey)
	if len(n) <= 16 {
		return n
	}
	return n[:9] + "…" + n[len(n)-4:]
}

// title is the article's title tag, else the first line of the post.
func title(e Event) string {
	if t := strings.TrimSpace(e.Tag("title")); t != "" {
		return truncate(t, 120)
	}
	first, _, _ := strings.Cut(strings.TrimSpace(e.Content), "\n")
	return truncate(first, 80)
}

// truncate cuts plain text to at most max runes, ending a cut in an ellipsis.
func truncate(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return strings.TrimSpace(string([]rune(s)[:max])) + "…"
}
