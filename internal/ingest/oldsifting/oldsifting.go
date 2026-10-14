// Package oldsifting works through the old pile in Karakeep: unarchived
// bookmarks older than the age limit, which the Karakeep ingestor never reaches
// (ARCHITECTURE.md sections 6 and 8). Each run crawls a few pages of the
// unarchived list into a small ID cache, and once a day the oldest cached
// bookmarks are brought into the Backlog as ordinary Karakeep-source items. It
// is read-only towards Karakeep: nothing goes through the outbox, because the
// bookmark already exists and triage writes back as for any Karakeep item.
package oldsifting

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Jolls/Siftstr/internal/connections"
	kk "github.com/Jolls/Siftstr/internal/dest/karakeep"
	"github.com/Jolls/Siftstr/internal/ids"
	ingestkarakeep "github.com/Jolls/Siftstr/internal/ingest/karakeep"
	"github.com/Jolls/Siftstr/internal/ingest/miniflux"
	"github.com/Jolls/Siftstr/internal/sources"
)

// ErrNoConnection means the user has not saved a Karakeep connection.
var ErrNoConnection = errors.New("no karakeep connection")

const (
	pageSize        = 20 // bookmarks per list request
	learnPages      = 2  // pages that add to the cache, per run
	maxReads        = 10 // list requests per run, however little they teach
	maxPullAttempts = 25 // candidates one pull may read from Karakeep

	// The job's state, in user_settings like karakeep_walked_back_to.
	settingCursor   = "old_sifting_cursor"    // where the walk resumes; empty means the newest end
	settingWalkedOn = "old_sifting_walked_on" // the local day a walk last reached the oldest bookmark
	settingPulledOn = "old_sifting_pulled_on" // the local day the pull last completed
)

// Syncer crawls and pulls for one user at a time.
type Syncer struct {
	DB    *sql.DB
	Conns *connections.Service
	New   ingestkarakeep.Factory
	Now   func() time.Time
}

// Result counts what one run did.
type Result struct {
	Reads     int  // list requests made
	Cached    int  // bookmarks newly added to the cache
	Pulled    int  // bookmarks brought into the Backlog
	Dropped   int  // cached bookmarks that no longer qualified
	Restarted bool // a stored cursor failed and the walk began again from the newest
	Passed    bool // the walk reached the oldest bookmark
}

// Run crawls, then pulls, for one user. If the crawl fails the pull still runs
// from what is cached, and both errors are returned.
func (g *Syncer) Run(ctx context.Context, userID string) (Result, error) {
	var res Result
	cfg, err := sources.New(g.DB).Ingest(ctx, userID)
	if err != nil {
		return res, err
	}
	if cfg.OldSiftingDaily <= 0 || cfg.MaxAgeDays <= 0 {
		return res, nil // off, or no age limit so the Karakeep ingestor already covers everything
	}
	var enabled bool
	err = g.DB.QueryRowContext(ctx, `SELECT enabled FROM sources WHERE user_id = ? AND kind = 'karakeep' AND external_id = ?`,
		userID, ingestkarakeep.SourceExternalID).Scan(&enabled)
	if err == nil && !enabled {
		return res, nil // the user turned the source off
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return res, err
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
	now := g.Now().UTC()
	day, err := sources.LocalDay(ctx, g.DB, userID, now)
	if err != nil {
		return res, err
	}
	cerr := g.crawl(ctx, userID, api, cfg, now, day, &res)
	perr := g.pull(ctx, userID, api, cfg, now, day, &res)
	return res, errors.Join(cerr, perr)
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

// qualifies reports whether b may be brought in: an old link that is not
// tagged sifted and is not already in Siftstr.
func qualifies(b kk.Bookmark, cutoff time.Time, known ingestkarakeep.Known) bool {
	return b.Type == "link" && b.URL != "" && !b.HasTag(ingestkarakeep.TagSifted) && !known.Has(b) &&
		!b.CreatedAt.IsZero() && b.CreatedAt.Before(cutoff)
}

func (g *Syncer) loadKnown(ctx context.Context, userID string) (ingestkarakeep.Known, error) {
	tx, err := g.DB.BeginTx(ctx, nil)
	if err != nil {
		return ingestkarakeep.Known{}, err
	}
	defer tx.Rollback()
	return ingestkarakeep.LoadKnown(ctx, tx, userID)
}

// crawl walks the unarchived list from where the last run stopped and caches
// the old bookmarks it has not seen. Only pages that taught something count
// against the budget, so a page that is all recent, cached, known, sifted or
// not a link is passed over.
func (g *Syncer) crawl(ctx context.Context, userID string, api ingestkarakeep.API, cfg sources.IngestSettings, now time.Time, day string, res *Result) error {
	cursor, err := g.setting(ctx, userID, settingCursor)
	if err != nil {
		return err
	}
	walked, err := g.setting(ctx, userID, settingWalkedOn)
	if err != nil {
		return err
	}
	if cursor == "" && walked == day {
		return nil // a pass already ended today; the next starts on a later day
	}
	known, err := g.loadKnown(ctx, userID)
	if err != nil {
		return err
	}
	cutoff := now.AddDate(0, 0, -cfg.MaxAgeDays)
	reads, learned := 0, 0
	defer func() { res.Reads = reads }()
	for reads < maxReads && learned < learnPages {
		p, err := api.ListUnarchived(ctx, cursor, pageSize)
		reads++
		if err != nil {
			if cursor == "" || res.Restarted {
				return fmt.Errorf("list karakeep bookmarks: %w", err)
			}
			// The stored cursor may no longer work. If the newest page loads, the
			// walk starts again from there; what was cached stays.
			front, ferr := api.ListUnarchived(ctx, "", pageSize)
			reads++
			if ferr != nil {
				return fmt.Errorf("list karakeep bookmarks: %w", err)
			}
			p, res.Restarted = front, true
		}
		n, err := g.store(ctx, userID, p, cutoff, known, day)
		if err != nil {
			return err
		}
		res.Cached += n
		if n > 0 {
			learned++
		}
		cursor = p.NextCursor
		if cursor == "" {
			res.Passed = true
			break
		}
	}
	return nil
}

// store caches the qualifying bookmarks of one page and saves where the walk
// is, in one transaction. It returns how many rows were new.
func (g *Syncer) store(ctx context.Context, userID string, p kk.Page, cutoff time.Time, known ingestkarakeep.Known, day string) (int, error) {
	tx, err := g.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	n := 0
	for _, b := range p.Bookmarks {
		if !qualifies(b, cutoff, known) {
			continue
		}
		r, err := tx.ExecContext(ctx, `INSERT INTO old_bookmark_cache (user_id, bookmark_id, bookmark_created_at) VALUES (?, ?, ?)
			ON CONFLICT (user_id, bookmark_id) DO NOTHING`, userID, b.ID, b.CreatedAt.UTC().Format(time.RFC3339))
		if err != nil {
			return 0, fmt.Errorf("cache bookmark: %w", err)
		}
		if c, _ := r.RowsAffected(); c > 0 {
			n++
		}
	}
	if err := setSetting(ctx, tx, userID, settingCursor, p.NextCursor); err != nil {
		return 0, err
	}
	if p.NextCursor == "" {
		if err := setSetting(ctx, tx, userID, settingWalkedOn, day); err != nil {
			return 0, err
		}
	}
	return n, tx.Commit()
}

// pull brings the oldest cached bookmarks into the Backlog, at most the daily
// count and never past the Backlog limit, and at most one completed pull per
// local day.
func (g *Syncer) pull(ctx context.Context, userID string, api ingestkarakeep.API, cfg sources.IngestSettings, now time.Time, day string, res *Result) error {
	if done, err := g.setting(ctx, userID, settingPulledOn); err != nil || done == day {
		return err
	}
	var pulledToday, backlog int
	if err := g.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM old_bookmark_cache WHERE user_id = ? AND pulled_on = ?`, userID, day).Scan(&pulledToday); err != nil {
		return err
	}
	// What /backlog shows as work left: untriaged cards from earlier days.
	if err := g.DB.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM items WHERE user_id = ? AND digest_id IS NULL AND batch_date < ? AND state IN ('light', 'deep'))
		      + (SELECT COUNT(*) FROM digests WHERE user_id = ? AND batch_date < ? AND state IN ('light', 'deep'))`,
		userID, day, userID, day).Scan(&backlog); err != nil {
		return err
	}
	room := min(cfg.OldSiftingDaily-pulledToday, cfg.OldSiftingBacklogCap-backlog)
	if room <= 0 {
		return setSetting(ctx, g.DB, userID, settingPulledOn, day) // nothing is added today
	}
	known, err := g.loadKnown(ctx, userID)
	if err != nil {
		return err
	}
	cutoff := now.AddDate(0, 0, -cfg.MaxAgeDays)
	added := 0
	for attempts := 0; added < room && attempts < maxPullAttempts; attempts++ {
		var id string
		err := g.DB.QueryRowContext(ctx, `SELECT bookmark_id FROM old_bookmark_cache WHERE user_id = ? AND pulled_on IS NULL
			ORDER BY bookmark_created_at, bookmark_id LIMIT 1`, userID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // out of candidates; a later run may find more
		} else if err != nil {
			return err
		}
		b, err := api.Get(ctx, id)
		gone := errors.Is(err, kk.ErrNotFound)
		if err != nil && !gone {
			return fmt.Errorf("read karakeep bookmark: %w", err)
		}
		if gone || b.Archived || !qualifies(b, cutoff, known) {
			if _, err := g.DB.ExecContext(ctx, `DELETE FROM old_bookmark_cache WHERE user_id = ? AND bookmark_id = ?`, userID, id); err != nil {
				return err
			}
			res.Dropped++
			continue
		}
		inserted, err := g.insert(ctx, userID, b, cfg, now, day)
		if err != nil {
			return err
		}
		if inserted {
			added++
			res.Pulled++
			known.Add(b)
		}
	}
	if added == room {
		return setSetting(ctx, g.DB, userID, settingPulledOn, day)
	}
	return nil
}

// insert adds b to the Backlog as a light item of the Karakeep source and marks
// its cache row pulled, in one transaction. The bookmark's creation day is its
// batch day, so it lands in the Backlog and not in Today.
func (g *Syncer) insert(ctx context.Context, userID string, b kk.Bookmark, cfg sources.IngestSettings, now time.Time, day string) (bool, error) {
	tx, err := g.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	sourceID, err := ingestkarakeep.UpsertSource(ctx, tx, userID)
	if err != nil {
		return false, err
	}
	id, err := ids.New("itm_")
	if err != nil {
		return false, err
	}
	batch, err := sources.LocalDay(ctx, tx, userID, b.CreatedAt)
	if err != nil {
		return false, err
	}
	stamp := now.Format(time.RFC3339)
	var byline any
	if by := ingestkarakeep.Byline(b); by != "" {
		byline = by
	}
	r, err := tx.ExecContext(ctx,
		`INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, excerpt, published_at, state, batch_date, karakeep_id, byline, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'light', ?, ?, ?, ?, ?)
		 ON CONFLICT (user_id, source_id, external_id) DO NOTHING`,
		id, userID, sourceID, b.ID, miniflux.MediaType(miniflux.Entry{URL: b.URL}), b.URL,
		ingestkarakeep.Title(b), excerptFor(b, cfg.ExcerptLength), b.CreatedAt.UTC().Format(time.RFC3339), batch, b.ID, byline, stamp, stamp)
	if err != nil {
		return false, fmt.Errorf("insert item: %w", err)
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		// An item for this bookmark already exists, so it is not old pile: drop
		// it from the cache rather than count it as pulled today.
		if _, err := tx.ExecContext(ctx, `DELETE FROM old_bookmark_cache WHERE user_id = ? AND bookmark_id = ?`, userID, b.ID); err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE old_bookmark_cache SET pulled_on = ? WHERE user_id = ? AND bookmark_id = ?`, day, userID, b.ID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// excerptFor is Karakeep's summary when it has one, else what its ingestor
// would show: the bookmark's description, then its note.
func excerptFor(b kk.Bookmark, n int) string {
	if s := b.Summary; strings.TrimSpace(s) != "" {
		return miniflux.Excerpt(s, n)
	}
	return ingestkarakeep.Excerpt(b, n)
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func setSetting(ctx context.Context, x execer, userID, key, value string) error {
	_, err := x.ExecContext(ctx, `INSERT INTO user_settings (user_id, key, value) VALUES (?, ?, ?)
		ON CONFLICT (user_id, key) DO UPDATE SET value = excluded.value`, userID, key, value)
	return err
}

func (g *Syncer) setting(ctx context.Context, userID, key string) (string, error) {
	var v string
	err := g.DB.QueryRowContext(ctx, `SELECT value FROM user_settings WHERE user_id = ? AND key = ?`, userID, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}
