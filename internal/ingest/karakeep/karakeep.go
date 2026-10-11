// Package karakeep is Siftstr's side of Karakeep as a source: it offers the
// bookmarks Karakeep collected itself for triage, and reads Karakeep's own AI
// summary back for promoted items that use it (ARCHITECTURE.md sections 5 and
// 6). It runs on one timer, ingest first and then summaries.
package karakeep

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Jolls/Siftstr/internal/connections"
	kk "github.com/Jolls/Siftstr/internal/dest/karakeep"
	"github.com/Jolls/Siftstr/internal/ids"
	"github.com/Jolls/Siftstr/internal/ingest"
	"github.com/Jolls/Siftstr/internal/ingest/miniflux"
	"github.com/Jolls/Siftstr/internal/sources"
)

// ErrNoConnection means the user has not saved a Karakeep connection.
var ErrNoConnection = errors.New("no karakeep connection")

// TagSifted marks a bookmark Siftstr has handled. A bookmark carrying it is
// never offered for triage, which is what breaks the loop with Karakeep as a
// destination: a kept item is unarchived, so it would otherwise reappear.
const TagSifted = "sifted"

// SourceExternalID identifies the one Karakeep source row each user has.
const SourceExternalID = "karakeep"

// Paging limits. A page of full bookmarks is large, so pages stay small, and
// the walk is capped so that a user with no age limit cannot make one tick
// read an enormous library.
const (
	pageSize = 20
	maxPages = 100
)

// DefaultSummaryTimeoutHours is how long a promoted item waits for Karakeep's
// summary before it is shown with its excerpt alone.
const DefaultSummaryTimeoutHours = sources.DefaultSummaryTimeoutHours

// API is the part of Karakeep this package uses. *karakeep.Client satisfies it.
type API interface {
	ListUnarchived(ctx context.Context, cursor string, limit int) (kk.Page, error)
	Get(ctx context.Context, id string) (kk.Bookmark, error)
}

// Factory builds an API for one connection. Production code passes
// ClientFactory; tests pass a fake.
type Factory func(baseURL, token string) API

// ClientFactory builds the real HTTP client.
func ClientFactory(baseURL, token string) API { return kk.NewClient(baseURL, token, nil) }

// Syncer ingests bookmarks and resolves summaries. Every method is scoped to
// one user.
type Syncer struct {
	DB    *sql.DB
	Conns *connections.Service
	New   Factory
	Now   func() time.Time
}

// Result counts what one run did.
type Result struct {
	Items    int // bookmarks offered for triage
	TooOld   int // 1 when the walk reached a bookmark older than max_age_days
	Skipped  int // already known, already sifted, or not a link
	Resolved int // promoted items that received Karakeep's summary
	GaveUp   int // promoted items shown with their excerpt because no summary came
}

// Run ingests, then resolves summaries, for one user. A failure in either part
// does not stop the other, and the first error is returned with the counts.
func (g *Syncer) Run(ctx context.Context, userID string) (Result, error) {
	var res Result
	conn, err := g.connection(ctx, userID)
	if err != nil {
		return res, err
	}
	token, err := g.Conns.Secret(ctx, userID, conn.ID)
	if err != nil {
		return res, fmt.Errorf("karakeep token: %w", err)
	}
	api := g.New(conn.BaseURL, token)
	ierr := g.ingest(ctx, userID, api, &res)
	rerr := g.resolve(ctx, userID, api, &res)
	return res, errors.Join(ierr, rerr)
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

// ingest lists unarchived bookmarks newest first and offers each new one.
func (g *Syncer) ingest(ctx context.Context, userID string, api API, res *Result) error {
	cfg, err := sources.New(g.DB).Ingest(ctx, userID)
	if err != nil {
		return err
	}
	now := g.Now().UTC()
	var cutoff time.Time // zero means no age limit
	if cfg.MaxAgeDays > 0 {
		cutoff = now.AddDate(0, 0, -cfg.MaxAgeDays)
	}

	var enabled bool
	err = g.DB.QueryRowContext(ctx, `SELECT enabled FROM sources WHERE user_id = ? AND kind = 'karakeep' AND external_id = ?`,
		userID, SourceExternalID).Scan(&enabled)
	if err == nil && !enabled {
		return nil // the user turned the source off
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	// Read everything first, so a failed listing writes nothing. The walk stops
	// at the first page with nothing to do (every bookmark on it is already
	// known and named), since newer bookmarks come first: in steady state that
	// is one page, however big the library.
	before, err := g.knownBefore(ctx, userID)
	if err != nil {
		return err
	}
	// A page with nothing to do ends the walk only if an earlier walk already
	// went back as far as this one would: raising the age limit, or removing it,
	// must still bring in the older bookmarks.
	mayStop, err := g.walkedBack(ctx, userID, cutoff)
	if err != nil {
		return err
	}
	var fresh []kk.Bookmark
	cursor := ""
	complete := false // the walk reached the age limit or the end of the library
	for page := 0; page < maxPages; page++ {
		p, err := api.ListUnarchived(ctx, cursor, pageSize)
		if err != nil {
			return fmt.Errorf("list karakeep bookmarks: %w", err)
		}
		reachedOld, work := false, false
		for _, b := range p.Bookmarks {
			if !cutoff.IsZero() && !b.CreatedAt.IsZero() && b.CreatedAt.Before(cutoff) {
				reachedOld = true // newest first, so everything after is older still
				break
			}
			fresh = append(fresh, b)
			if before.needsWork(b) {
				work = true
			}
		}
		if reachedOld {
			res.TooOld, complete = 1, true
			break
		}
		if cursor = p.NextCursor; cursor == "" {
			complete = true
			break
		}
		if !work && mayStop {
			break
		}
	}
	if len(fresh) == 0 {
		if complete {
			return g.markWalked(ctx, g.DB, userID, cutoff)
		}
		return nil
	}

	tx, err := g.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	known, err := knownItems(ctx, tx, userID)
	if err != nil {
		return err
	}
	day, err := sources.LocalDay(ctx, tx, userID, now)
	if err != nil {
		return err
	}
	sourceID := ""
	insert, err := tx.PrepareContext(ctx,
		`INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, excerpt, published_at, state, batch_date, karakeep_id, byline, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'light', ?, ?, ?, ?, ?)
		 ON CONFLICT (user_id, source_id, external_id) DO NOTHING`)
	if err != nil {
		return err
	}
	defer insert.Close()
	stamp := now.Format(time.RFC3339)
	// Items that came in before bylines existed get one from the listing, whatever
	// they have become since. An existing byline is never overwritten.
	name, err := tx.PrepareContext(ctx, `UPDATE items SET byline = ?
		WHERE user_id = ? AND karakeep_id = ? AND byline IS NULL
		  AND source_id IN (SELECT id FROM sources WHERE user_id = ? AND kind = 'karakeep')`)
	if err != nil {
		return err
	}
	defer name.Close()
	// Oldest first, so the newest bookmark gets the newest row.
	for i := len(fresh) - 1; i >= 0; i-- {
		b := fresh[i]
		if by := Byline(b); by != "" && b.Type == "link" {
			if _, err := name.ExecContext(ctx, by, userID, b.ID, userID); err != nil {
				return fmt.Errorf("set byline: %w", err)
			}
		}
		switch {
		case b.Type != "link" || b.URL == "", b.HasTag(TagSifted), known.has(b):
			res.Skipped++
			continue
		}
		if sourceID == "" {
			if sourceID, err = upsertSource(ctx, tx, userID); err != nil {
				return err
			}
		}
		id, err := ids.New("itm_")
		if err != nil {
			return err
		}
		title := firstNonEmpty(b.Title, b.PageTitle, b.URL)
		excerpt := miniflux.Excerpt(firstNonEmpty(b.Description, b.Note), cfg.ExcerptLength)
		r, err := insert.ExecContext(ctx, id, userID, sourceID, b.ID, miniflux.MediaType(miniflux.Entry{URL: b.URL}), b.URL,
			title, excerpt, b.CreatedAt.UTC().Format(time.RFC3339), day, b.ID, nullIfEmpty(Byline(b)), stamp, stamp)
		if err != nil {
			return fmt.Errorf("insert item: %w", err)
		}
		if n, _ := r.RowsAffected(); n > 0 {
			res.Items++
			known.add(b)
		}
	}
	if complete {
		if err := g.markWalked(ctx, tx, userID, cutoff); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const settingWalked = "karakeep_walked_back_to"

// walkedBack reports whether an earlier walk went back at least as far as one
// with this cutoff (the zero time meaning no limit) would.
func (g *Syncer) walkedBack(ctx context.Context, userID string, cutoff time.Time) (bool, error) {
	var v string
	err := g.DB.QueryRowContext(ctx, `SELECT value FROM user_settings WHERE user_id = ? AND key = ?`, userID, settingWalked).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if v == "none" {
		return true, nil // went back through the whole library
	}
	if cutoff.IsZero() {
		return false, nil
	}
	before, perr := time.Parse(time.RFC3339, v)
	return perr == nil && !before.After(cutoff), nil
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// markWalked records how far back a walk that reached its limit went. The
// limit moves forward with the clock, so a day later the same setting still
// counts as covered.
func (g *Syncer) markWalked(ctx context.Context, x execer, userID string, cutoff time.Time) error {
	v := "none"
	if !cutoff.IsZero() {
		v = cutoff.UTC().Format(time.RFC3339)
	}
	_, err := x.ExecContext(ctx, `INSERT INTO user_settings (user_id, key, value) VALUES (?, ?, ?)
		ON CONFLICT (user_id, key) DO UPDATE SET value = excluded.value`, userID, settingWalked, v)
	return err
}

func upsertSource(ctx context.Context, tx *sql.Tx, userID string) (string, error) {
	id, err := ids.New("src_")
	if err != nil {
		return "", err
	}
	// The row's settings are the user's to change: only a new row is written.
	err = tx.QueryRowContext(ctx,
		`INSERT INTO sources (id, user_id, kind, external_id, name, category, summary_source)
		 VALUES (?, ?, 'karakeep', ?, 'Karakeep', 'Karakeep', 'karakeep')
		 ON CONFLICT (user_id, kind, external_id) DO UPDATE SET external_id = external_id
		 RETURNING id`, id, userID, SourceExternalID).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("upsert source: %w", err)
	}
	return id, nil
}

// Byline names where a bookmark came from: the publisher Karakeep found, else
// the site of the link without a leading "www.".
func Byline(b kk.Bookmark) string {
	if p := strings.Join(strings.Fields(b.Publisher), " "); p != "" {
		return p
	}
	u, err := url.Parse(strings.TrimSpace(b.URL))
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// known is what the user's items already cover: bookmark IDs, and normalized
// URLs (which also covers the short window in which a bookmark Siftstr just
// made for a keep exists but is not tagged yet).
type known struct {
	ids  map[string]bool
	urls map[string]bool
}

func (k known) has(b kk.Bookmark) bool { return k.ids[b.ID] || k.urls[NormalizeURL(b.URL)] }

func (k known) add(b kk.Bookmark) {
	k.ids[b.ID] = true
	k.urls[NormalizeURL(b.URL)] = true
}

func knownItems(ctx context.Context, tx *sql.Tx, userID string) (known, error) {
	k := known{ids: map[string]bool{}, urls: map[string]bool{}}
	rows, err := tx.QueryContext(ctx,
		`SELECT i.url, COALESCE(i.karakeep_id, ''), CASE WHEN s.kind = 'karakeep' THEN i.external_id ELSE '' END
		 FROM items i JOIN sources s ON s.user_id = i.user_id AND s.id = i.source_id WHERE i.user_id = ?`, userID)
	if err != nil {
		return k, err
	}
	defer rows.Close()
	for rows.Next() {
		var u, kid, ext string
		if err := rows.Scan(&u, &kid, &ext); err != nil {
			return k, err
		}
		k.urls[NormalizeURL(u)] = true
		for _, id := range []string{kid, ext} {
			if id != "" {
				k.ids[id] = true
			}
		}
	}
	return k, rows.Err()
}

// knownState is what the user's items already hold, read before listing so the
// walk can tell when a page has nothing in it to do.
type knownState struct {
	known   known
	unnamed map[string]bool // bookmark IDs of Karakeep-source items that have no byline yet
}

// needsWork reports whether a listed bookmark would be ingested or named.
func (k knownState) needsWork(b kk.Bookmark) bool {
	if b.Type != "link" || b.URL == "" || b.HasTag(TagSifted) {
		return k.unnamed[b.ID] && Byline(b) != ""
	}
	return !k.known.has(b) || (k.unnamed[b.ID] && Byline(b) != "")
}

func (g *Syncer) knownBefore(ctx context.Context, userID string) (knownState, error) {
	tx, err := g.DB.BeginTx(ctx, nil)
	if err != nil {
		return knownState{}, err
	}
	defer tx.Rollback()
	k, err := knownItems(ctx, tx, userID)
	if err != nil {
		return knownState{}, err
	}
	ks := knownState{known: k, unnamed: map[string]bool{}}
	rows, err := tx.QueryContext(ctx,
		`SELECT i.karakeep_id FROM items i JOIN sources s ON s.user_id = i.user_id AND s.id = i.source_id
		 WHERE i.user_id = ? AND s.kind = 'karakeep' AND i.karakeep_id IS NOT NULL AND i.byline IS NULL`, userID)
	if err != nil {
		return knownState{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return knownState{}, err
		}
		ks.unnamed[id] = true
	}
	return ks, rows.Err()
}

// NormalizeURL reduces a link to a form that compares equal across the
// spellings one page commonly has: the scheme and host in lower case, no
// fragment, no tracking parameters, no trailing slash.
func NormalizeURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return strings.TrimSpace(raw)
	}
	u.Scheme, u.Host, u.Fragment, u.User = strings.ToLower(u.Scheme), strings.ToLower(u.Host), "", nil
	q := u.Query()
	for k := range q {
		if strings.HasPrefix(strings.ToLower(k), "utm_") || k == "fbclid" || k == "gclid" {
			q.Del(k)
		}
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	enc := url.Values{}
	for _, k := range keys {
		enc[k] = q[k]
	}
	u.RawQuery = enc.Encode()
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String()
}

// resolve gives each promoted item that uses Karakeep summaries its summary
// once Karakeep has written one, and gives up on one that never comes: a
// failed crawl, a bookmark that is gone, or Karakeep's AI being off must not
// strand an item, so it is shown with its excerpt after the timeout.
//
// An item whose promote can still be undone is left alone, so that an undo is
// never refused because the summary happened to arrive meanwhile.
func (g *Syncer) resolve(ctx context.Context, userID string, api API, res *Result) error {
	cfg, err := sources.New(g.DB).Ingest(ctx, userID)
	if err != nil {
		return err
	}
	timeout := time.Duration(cfg.KarakeepSummaryTimeoutHours) * time.Hour
	now := g.Now().UTC()

	rows, err := g.DB.QueryContext(ctx,
		`SELECT i.id, COALESCE(i.karakeep_id, ''), COALESCE(i.promoted_at, i.updated_at)
		 FROM items i JOIN sources s ON s.user_id = i.user_id AND s.id = i.source_id
		 WHERE i.user_id = ? AND i.state = 'pending_deep' AND `+ingest.ViaKarakeep+`
		   AND NOT EXISTS (SELECT 1 FROM actions a WHERE a.user_id = i.user_id AND a.subject_type = 'item'
		                   AND a.subject_id = i.id AND a.status = 'held')
		 ORDER BY i.promoted_at, i.id`, userID)
	if err != nil {
		return err
	}
	type waiting struct {
		id, bookmark string
		since        time.Time
	}
	var queue []waiting
	for rows.Next() {
		var w waiting
		var since string
		if err := rows.Scan(&w.id, &w.bookmark, &since); err != nil {
			rows.Close()
			return err
		}
		w.since, _ = time.Parse(time.RFC3339, since)
		queue = append(queue, w)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(queue) == 0 {
		return nil
	}
	day, err := g.today(ctx, userID, now)
	if err != nil {
		return err
	}

	var first error
	for _, w := range queue {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		expired := !w.since.IsZero() && now.Sub(w.since) >= timeout
		var summary string
		giveUp := false
		if w.bookmark == "" {
			giveUp = expired // no bookmark yet: its creation may still be queued
		} else {
			b, err := api.Get(ctx, w.bookmark)
			switch {
			case errors.Is(err, kk.ErrNotFound):
				giveUp = true // deleted in Karakeep: nothing will come
			case err != nil:
				if first == nil {
					first = fmt.Errorf("read karakeep bookmark: %w", err)
				}
				continue
			case b.SummaryStatus == kk.SummaryFailed:
				giveUp = true
			case b.SummaryStatus != kk.SummaryPending && strings.TrimSpace(b.Summary) != "":
				summary = strings.TrimSpace(b.Summary)
			case b.SummaryStatus == kk.SummaryDone:
				giveUp = true // finished with nothing to show
			default:
				giveUp = expired
			}
		}
		if summary == "" && !giveUp {
			continue
		}
		ok, err := g.finish(ctx, userID, w.id, summary, day, now)
		if err != nil {
			return err
		}
		switch {
		case ok && summary != "":
			res.Resolved++
		case ok:
			res.GaveUp++
		}
	}
	return first
}

// finish moves a waiting item to deep, with Karakeep's summary if there is
// one. It does nothing if the item has moved on or an action is being held.
func (g *Syncer) finish(ctx context.Context, userID, itemID, summary, day string, now time.Time) (bool, error) {
	var sum any
	if summary != "" {
		sum = summary
	}
	r, err := g.DB.ExecContext(ctx,
		`UPDATE items SET state = 'deep', deep_summary = ?, batch_date = ?, updated_at = ?
		 WHERE user_id = ? AND id = ? AND state = 'pending_deep'
		   AND NOT EXISTS (SELECT 1 FROM actions a WHERE a.user_id = items.user_id AND a.subject_type = 'item'
		                   AND a.subject_id = items.id AND a.status = 'held')`,
		sum, day, now.Format(time.RFC3339), userID, itemID)
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n > 0, nil
}

func (g *Syncer) today(ctx context.Context, userID string, now time.Time) (string, error) {
	return sources.LocalDay(ctx, g.DB, userID, now)
}
