package miniflux

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Jolls/Siftstr/internal/connections"
	"github.com/Jolls/Siftstr/internal/ids"
	"github.com/Jolls/Siftstr/internal/sources"
)

// DefaultExcerptLength is used when the user has not set excerpt_length.
const DefaultExcerptLength = sources.DefaultExcerptLength

// DefaultMaxAgeDays is used when the user has not set max_age_days. Unread
// entries published longer ago than this are not ingested.
const DefaultMaxAgeDays = sources.DefaultMaxAgeDays

// ErrNoConnection means the user has not saved a Miniflux connection.
var ErrNoConnection = errors.New("no miniflux connection")

// Factory builds an API for one connection. Production code passes
// ClientFactory; tests pass a fake.
type Factory func(baseURL, token string) API

// ClientFactory builds the real HTTP client.
func ClientFactory(baseURL, token string) API { return NewClient(baseURL, token, nil) }

// Ingester mirrors feeds into sources and unread entries into items.
type Ingester struct {
	DB    *sql.DB
	Conns *connections.Service
	New   Factory
	Now   func() time.Time
}

// Result counts what one run changed.
type Result struct {
	Sources int // feeds mirrored into sources
	Items   int // new items inserted
	TooOld  int // entries skipped for being older than max_age_days
}

// Run ingests for one user. It never reads another user's rows. Feeds and
// entries are written in one transaction, so a failed run leaves nothing
// half-applied and a first sync of thousands of entries is one commit.
func (g *Ingester) Run(ctx context.Context, userID string) (Result, error) {
	var res Result
	conn, err := g.connection(ctx, userID)
	if err != nil {
		return res, err
	}
	token, err := g.Conns.Secret(ctx, userID, conn.ID)
	if err != nil {
		return res, fmt.Errorf("miniflux token: %w", err)
	}
	api := g.New(conn.BaseURL, token)

	feeds, err := api.Feeds(ctx)
	if err != nil {
		return res, err
	}
	entries, err := api.UnreadEntries(ctx)
	if err != nil {
		return res, err
	}
	cfg, err := sources.New(g.DB).Ingest(ctx, userID)
	if err != nil {
		return res, err
	}
	now := g.Now().UTC()
	var cutoff time.Time // zero means no age limit
	if cfg.MaxAgeDays > 0 {
		cutoff = now.AddDate(0, 0, -cfg.MaxAgeDays)
	}
	stamp := now.Format(time.RFC3339)

	tx, err := g.DB.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	refs, err := syncSources(ctx, tx, userID, feeds)
	if err != nil {
		return res, err
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
	for _, e := range entries {
		ref, ok := refs[strconv.FormatInt(e.FeedID, 10)]
		if !ok || !ref.enabled {
			continue
		}
		if !cutoff.IsZero() && !e.Published.IsZero() && e.Published.Before(cutoff) {
			res.TooOld++
			continue
		}
		id, err := ids.New("itm_")
		if err != nil {
			return res, err
		}
		var published any
		if !e.Published.IsZero() {
			published = e.Published.UTC().Format(time.RFC3339)
		}
		r, err := insert.ExecContext(ctx, id, userID, ref.id, strconv.FormatInt(e.ID, 10), MediaType(e), e.URL,
			strings.TrimSpace(e.Title), Excerpt(e.Content, cfg.ExcerptLength), nullIfEmpty(e.Content), published, stamp, stamp)
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
		if c.Kind == "miniflux" {
			return c, nil
		}
	}
	return connections.Connection{}, ErrNoConnection
}

type sourceRef struct {
	id      string
	enabled bool
}

// syncSources upserts one source per feed. Name and category follow
// Miniflux; the user's per-source settings are never touched.
func syncSources(ctx context.Context, tx *sql.Tx, userID string, feeds []Feed) (map[string]sourceRef, error) {
	out := make(map[string]sourceRef, len(feeds))
	for _, f := range feeds {
		ext := strconv.FormatInt(f.ID, 10)
		id, err := ids.New("src_")
		if err != nil {
			return nil, err
		}
		var ref sourceRef
		err = tx.QueryRowContext(ctx,
			`INSERT INTO sources (id, user_id, kind, external_id, name, category) VALUES (?, ?, 'miniflux_feed', ?, ?, ?)
			 ON CONFLICT (user_id, kind, external_id) DO UPDATE SET name = excluded.name, category = excluded.category
			 RETURNING id, enabled`,
			id, userID, ext, f.Title, f.Category).Scan(&ref.id, &ref.enabled)
		if err != nil {
			return nil, fmt.Errorf("upsert source: %w", err)
		}
		out[ext] = ref
	}
	return out, nil
}

func nullIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// MediaType derives article/video/podcast/post from the entry's enclosures
// and URL host. Ingest is the only place that looks at hosts; triage and
// write-back use the derived value.
func MediaType(e Entry) string {
	for _, x := range e.Enclosures {
		mt := strings.ToLower(x.MimeType)
		switch {
		case strings.HasPrefix(mt, "audio/"):
			return "podcast"
		case strings.HasPrefix(mt, "video/"):
			return "video"
		}
	}
	u, err := url.Parse(e.URL)
	if err != nil {
		return "article"
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "youtu.be", hostIs(host, "youtube.com"), hostIs(host, "vimeo.com"):
		return "video"
	case hostIs(host, "reddit.com"):
		return "post"
	}
	return "article"
}

func hostIs(host, domain string) bool { return host == domain || strings.HasSuffix(host, "."+domain) }

var (
	tagRE   = regexp.MustCompile(`(?s)<(script|style)[^>]*>.*?</(script|style)>|<[^>]*>`)
	spaceRE = regexp.MustCompile(`\s+`)
)

// Excerpt turns HTML content into plain text cut to at most max runes. A
// cut ends in an ellipsis.
//
// Miniflux's entry API exposes only the content, not the feed's separate
// summary field, so the truncation branch of the excerpt rule always applies.
func Excerpt(content string, max int) string {
	text := html.UnescapeString(tagRE.ReplaceAllString(content, " "))
	text = strings.TrimSpace(spaceRE.ReplaceAllString(text, " "))
	if utf8.RuneCountInString(text) <= max {
		return text
	}
	return strings.TrimSpace(string([]rune(text)[:max])) + "…"
}
