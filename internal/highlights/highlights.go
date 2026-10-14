// Package highlights puts one of the user's Karakeep highlights at the top of
// each day's briefing (ARCHITECTURE.md sections 8 and 10). Karakeep has no
// count or random pick for highlights, so a walk of its highlight list caches
// their IDs, and the day's pick is chosen from the cache and stored with the
// day. It is read-only towards Karakeep: nothing goes through the outbox.
package highlights

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Jolls/Siftstr/internal/connections"
	kk "github.com/Jolls/Siftstr/internal/dest/karakeep"
	ingestkarakeep "github.com/Jolls/Siftstr/internal/ingest/karakeep"
	"github.com/Jolls/Siftstr/internal/sources"
)

// ErrNoConnection means the user has not saved a Karakeep connection.
var ErrNoConnection = errors.New("no karakeep connection")

const (
	// RefreshEvery is how often the cache is walked again while Siftstr keeps
	// running. It is also walked on the first run after a start.
	RefreshEvery = 7 * 24 * time.Hour

	// maxPages caps one walk at 10,000 highlights. A walk that stops there is
	// incomplete, so it adds highlights but never removes any.
	maxPages = 100

	// maxAttempts bounds how many candidates one pick may discard (deleted
	// highlights, or bookmarks) before it gives up until the next run.
	maxAttempts = 5
)

// API is the part of Karakeep this package uses. *karakeep.Client satisfies it.
type API interface {
	ListHighlights(ctx context.Context, cursor string, limit int) (kk.HighlightPage, error)
	GetHighlight(ctx context.Context, id string) (kk.Highlight, error)
	Get(ctx context.Context, id string) (kk.Bookmark, error)
}

// Factory builds an API for one connection. Production code passes
// ClientFactory; tests pass a fake.
type Factory func(baseURL, token string) API

// ClientFactory builds the real HTTP client.
func ClientFactory(baseURL, token string) API { return kk.NewClient(baseURL, token, nil) }

// Syncer refreshes each user's highlight cache and picks the day's highlight.
// Every method is scoped to one user.
type Syncer struct {
	DB    *sql.DB
	Conns *connections.Service
	New   Factory
	Now   func() time.Time
	// Rand returns a number in [0, n). It defaults to math/rand/v2.
	Rand func(n int) int
	// Briefing, if set, is asked to rewrite the day's briefing file after a pick,
	// since the file may have been written before the pick was made.
	Briefing interface {
		Refresh(ctx context.Context, userID, day string) error
	}

	mu        sync.Mutex
	refreshed map[string]time.Time // when each user's cache was last walked, since this start
}

// Result counts what one run did.
type Result struct {
	Refreshed bool // the cache was walked
	Added     int  // highlights newly cached
	Removed   int  // cached highlights Karakeep no longer has
	Picked    bool // a highlight was picked for today
}

// Run walks the cache if it is due, then picks today's highlight if there is
// none yet. It does nothing unless the user has turned highlights on. If the
// walk fails the pick still comes from what is stored, and a failed pick
// leaves the briefing as it was: the next run tries again.
func (g *Syncer) Run(ctx context.Context, userID string) (Result, error) {
	var res Result
	cfg, err := sources.New(g.DB).Ingest(ctx, userID)
	if err != nil {
		return res, err
	}
	if !cfg.KarakeepHighlights {
		return res, nil
	}
	conn, err := g.connection(ctx, userID)
	if err != nil {
		return res, err
	}
	token, err := g.Conns.Secret(ctx, userID, conn.ID)
	if err != nil {
		return res, fmt.Errorf("karakeep token: %w", err)
	}
	api := g.New(conn.BaseURL, token)

	var rerr error
	if g.refreshDue(userID) {
		if rerr = g.refresh(ctx, userID, api, &res); rerr == nil {
			g.markRefreshed(userID)
		}
	}
	perr := g.ensurePick(ctx, userID, conn.BaseURL, api, &res)
	return res, errors.Join(rerr, perr)
}

func (g *Syncer) connection(ctx context.Context, userID string) (connections.Connection, error) {
	list, err := g.Conns.List(ctx, userID)
	if err != nil {
		return connections.Connection{}, err
	}
	for _, c := range list {
		if c.Kind == "karakeep" {
			return c, nil
		}
	}
	return connections.Connection{}, ErrNoConnection
}

func (g *Syncer) refreshDue(userID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	last, ok := g.refreshed[userID]
	return !ok || g.Now().Sub(last) >= RefreshEvery
}

func (g *Syncer) markRefreshed(userID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.refreshed == nil {
		g.refreshed = map[string]time.Time{}
	}
	g.refreshed[userID] = g.Now()
}

// refresh walks every highlight and brings the cache in line with it. The
// whole list is read before anything is written, so a failed walk changes
// nothing. A highlight with no text is left out: there is nothing to show.
func (g *Syncer) refresh(ctx context.Context, userID string, api API, res *Result) error {
	type entry struct{ id, bookmark string }
	var found []entry
	cursor, complete := "", false
	for page := 0; page < maxPages; page++ {
		p, err := api.ListHighlights(ctx, cursor, kk.MaxHighlightsPerPage)
		if err != nil {
			return fmt.Errorf("list karakeep highlights: %w", err)
		}
		for _, h := range p.Highlights {
			if h.ID != "" && h.BookmarkID != "" && strings.TrimSpace(h.Text) != "" {
				found = append(found, entry{h.ID, h.BookmarkID})
			}
		}
		if cursor = p.NextCursor; cursor == "" {
			complete = true
			break
		}
	}

	tx, err := g.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	insert, err := tx.PrepareContext(ctx, `INSERT INTO highlight_cache (user_id, highlight_id, bookmark_id) VALUES (?, ?, ?)
		ON CONFLICT (user_id, highlight_id) DO NOTHING`)
	if err != nil {
		return err
	}
	defer insert.Close()
	live := make(map[string]bool, len(found))
	for _, e := range found {
		live[e.id] = true
		r, err := insert.ExecContext(ctx, userID, e.id, e.bookmark)
		if err != nil {
			return fmt.Errorf("cache highlight: %w", err)
		}
		if n, _ := r.RowsAffected(); n > 0 {
			res.Added++
		}
	}
	if complete {
		stale, err := cachedIDs(ctx, tx, userID)
		if err != nil {
			return err
		}
		for _, id := range stale {
			if live[id] {
				continue
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM highlight_cache WHERE user_id = ? AND highlight_id = ?`, userID, id); err != nil {
				return err
			}
			res.Removed++
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	res.Refreshed = true
	return nil
}

func cachedIDs(ctx context.Context, tx *sql.Tx, userID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT highlight_id FROM highlight_cache WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ensurePick chooses today's highlight from the cache, unless one is already
// stored. A highlight Karakeep no longer has is dropped from the cache and
// another is tried. A failure to read Karakeep leaves the day without one.
func (g *Syncer) ensurePick(ctx context.Context, userID, baseURL string, api API, res *Result) error {
	day, err := sources.LocalDay(ctx, g.DB, userID, g.Now().UTC())
	if err != nil {
		return err
	}
	var have int
	if err := g.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM highlight_picks WHERE user_id = ? AND day = ?`, userID, day).Scan(&have); err != nil {
		return err
	}
	if have > 0 {
		return nil
	}
	for attempt := 0; attempt < maxAttempts; attempt++ {
		id, bookmarkID, ok, err := g.candidate(ctx, userID)
		if err != nil {
			return err
		}
		if !ok {
			return nil // nothing cached
		}
		h, err := api.GetHighlight(ctx, id)
		if errors.Is(err, kk.ErrHighlightNotFound) {
			if err := g.drop(ctx, userID, id); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("read karakeep highlight: %w", err)
		}
		if strings.TrimSpace(h.Text) == "" {
			if err := g.drop(ctx, userID, id); err != nil {
				return err
			}
			continue
		}
		if h.BookmarkID != "" {
			bookmarkID = h.BookmarkID
		}
		// The bookmark only names the highlight's source. If it cannot be read,
		// the highlight is shown without one rather than not at all.
		var title, link, byline string
		b, err := api.Get(ctx, bookmarkID)
		switch {
		case errors.Is(err, kk.ErrNotFound):
			if err := g.drop(ctx, userID, id); err != nil {
				return err
			}
			continue
		case err == nil:
			title = firstNonEmpty(b.Title, b.PageTitle)
			if b.Type == "link" {
				link = b.URL
			}
			byline = ingestkarakeep.Byline(b)
		}
		if err := g.store(ctx, userID, day, h, title, link, byline, karakeepLink(baseURL, bookmarkID)); err != nil {
			return err
		}
		res.Picked = true
		if g.Briefing != nil {
			if err := g.Briefing.Refresh(ctx, userID, day); err != nil {
				return fmt.Errorf("refresh briefing file: %w", err)
			}
		}
		return nil
	}
	return nil
}

// candidate picks a random cached highlight that has not been shown yet. When
// every one has been shown, the record is cleared and they all become
// candidates again.
func (g *Syncer) candidate(ctx context.Context, userID string) (id, bookmarkID string, ok bool, err error) {
	var n int
	if err = g.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM highlight_cache WHERE user_id = ? AND shown_on IS NULL`, userID).Scan(&n); err != nil {
		return
	}
	if n == 0 {
		if err = g.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM highlight_cache WHERE user_id = ?`, userID).Scan(&n); err != nil || n == 0 {
			return
		}
		if _, err = g.DB.ExecContext(ctx, `UPDATE highlight_cache SET shown_on = NULL WHERE user_id = ?`, userID); err != nil {
			return
		}
	}
	pick := rand.IntN
	if g.Rand != nil {
		pick = g.Rand
	}
	err = g.DB.QueryRowContext(ctx, `SELECT highlight_id, bookmark_id FROM highlight_cache
		WHERE user_id = ? AND shown_on IS NULL ORDER BY highlight_id LIMIT 1 OFFSET ?`, userID, pick(n)).Scan(&id, &bookmarkID)
	return id, bookmarkID, err == nil, err
}

func (g *Syncer) drop(ctx context.Context, userID, id string) error {
	_, err := g.DB.ExecContext(ctx, `DELETE FROM highlight_cache WHERE user_id = ? AND highlight_id = ?`, userID, id)
	return err
}

// karakeepLink is the bookmark's page in the user's Karakeep.
func karakeepLink(baseURL, bookmarkID string) string {
	if bookmarkID == "" {
		return ""
	}
	return strings.TrimRight(baseURL, "/") + "/dashboard/preview/" + url.PathEscape(bookmarkID)
}

// store records the day's pick and marks the highlight as shown, together.
func (g *Syncer) store(ctx context.Context, userID, day string, h kk.Highlight, title, link, byline, karakeepURL string) error {
	tx, err := g.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO highlight_picks
		(user_id, day, highlight_id, text, note, color, bookmark_title, bookmark_url, karakeep_url, byline, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id, day) DO NOTHING`,
		userID, day, h.ID, strings.TrimSpace(h.Text), strings.TrimSpace(h.Note), h.Color, title, link, karakeepURL, byline,
		g.Now().UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("store highlight: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE highlight_cache SET shown_on = ? WHERE user_id = ? AND highlight_id = ?`, day, userID, h.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}
