// Package runs implements the Claude contract: the daily work package and
// the summaries that come back. It makes no LLM calls itself.
package runs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Jolls/Siftstr/internal/ids"
)

// MaxItemsPerRun caps the individual light items in one work package, newest
// first. The rest stay pending and go out in a later run.
const MaxItemsPerRun = 200

// MaxDigestEntries caps the entries bundled into one digest, newest first.
// Entries over the cap stay pending and go into the next digest.
const MaxDigestEntries = 100

// MaxSummaryLen bounds one submitted summary.
const MaxSummaryLen = 20000

// Run bookkeeping: an unfinished run younger than runReuseWindow is reused by
// the next Create instead of adding a row, and an unfinished run older than
// staleRunAge is deleted.
const (
	runReuseWindow = time.Hour
	staleRunAge    = 7 * 24 * time.Hour
)

// ErrRunNotFound is returned for a missing run or another user's run.
var ErrRunNotFound = errors.New("run not found")

// Service builds work packages and stores summaries.
type Service struct {
	DB    *sql.DB
	Now   func() time.Time
	NewID func(prefix string) (string, error) // nil uses random IDs
	// OnDeep, when set, is called inside Submit's transaction after an item
	// receives its deep summary. Write-back uses it to send a promoted item
	// to Karakeep, which waits for that summary.
	OnDeep func(ctx context.Context, tx *sql.Tx, userID, itemID string) error
}

// New returns a Service using the real clock.
func New(db *sql.DB) *Service { return &Service{DB: db, Now: time.Now} }

func (s *Service) id(prefix string) (string, error) {
	if s.NewID != nil {
		return s.NewID(prefix)
	}
	return ids.New(prefix)
}

// loadable reports whether name is an IANA zone. LoadLocation also accepts ""
// and "Local", which are not zones a user can pick.
func loadable(name string) bool {
	if name == "" || name == "Local" {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}

// day returns the user's local date (YYYY-MM-DD) and timezone name for now.
// An unreadable stored timezone falls back to UTC rather than failing a run.
func (s *Service) day(ctx context.Context, tx *sql.Tx, userID string) (string, string, error) {
	var name string
	if err := tx.QueryRowContext(ctx, `SELECT timezone FROM users WHERE id = ?`, userID).Scan(&name); err != nil {
		return "", "", err
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		name, loc = "UTC", time.UTC
	}
	return s.Now().In(loc).Format("2006-01-02"), name, nil
}

// Today returns the user's local date (YYYY-MM-DD) for now, which is the day
// pages group cards by.
func (s *Service) Today(ctx context.Context, userID string) (string, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	day, _, err := s.day(ctx, tx, userID)
	return day, err
}

// SourceRef is the source an item came from.
type SourceRef struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	PromptOverride *string `json:"prompt_override"`
}

// Item is one thing to summarize.
type Item struct {
	ID           string    `json:"id"`
	Stage        string    `json:"stage"` // light | deep
	MediaType    string    `json:"media_type"`
	Source       SourceRef `json:"source"`
	Title        string    `json:"title"`
	URL          string    `json:"url"`
	Excerpt      string    `json:"excerpt"`
	Content      *string   `json:"content"`
	LightSummary *string   `json:"light_summary,omitempty"` // deep stage only
}

// Entry is one child of a digest.
type Entry struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Excerpt string `json:"excerpt"`
}

// Digest bundles one source's pending entries into one thing to summarize.
type Digest struct {
	ID      string    `json:"id"`
	Source  SourceRef `json:"source"`
	Entries []Entry   `json:"entries"`
}

// WorkPackage is what POST /api/v1/runs returns.
type WorkPackage struct {
	RunID        string       `json:"run_id"`
	Day          string       `json:"day"` // the user's local date for this run
	Timezone     string       `json:"timezone"`
	Instructions Instructions `json:"instructions"`
	Items        []Item       `json:"items"`
	Digests      []Digest     `json:"digests"`
}

// Create starts a run for userID. It applies carryover, bundles digests and
// builds the work package from what is already in the database. It fetches
// nothing. Calling it again before summaries arrive is safe: pending work is
// simply offered again.
func (s *Service) Create(ctx context.Context, userID string) (WorkPackage, error) {
	var wp WorkPackage
	instr, err := s.Prompts(ctx, userID)
	if err != nil {
		return wp, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return wp, err
	}
	defer tx.Rollback()

	today, tzName, err := s.day(ctx, tx, userID)
	if err != nil {
		return wp, err
	}
	now := s.Now().UTC().Format(time.RFC3339)

	// Carryover. "carry" needs no change: an untriaged item with an older
	// batch_date is the Backlog, light or deep. "drop" expires it, digest
	// children with it.
	dropSources := `source_id IN (SELECT id FROM sources WHERE user_id = ? AND carryover = 'drop')`
	if _, err := tx.ExecContext(ctx,
		`UPDATE items SET state = 'expired', updated_at = ? WHERE user_id = ? AND state IN ('light', 'deep') AND batch_date < ? AND `+dropSources,
		now, userID, today, userID); err != nil {
		return wp, fmt.Errorf("carryover items: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE digests SET state = 'expired' WHERE user_id = ? AND state = 'light' AND batch_date < ? AND `+dropSources,
		userID, today, userID); err != nil {
		return wp, fmt.Errorf("carryover digests: %w", err)
	}

	if err := s.bundleDigests(ctx, tx, userID, today); err != nil {
		return wp, err
	}
	if wp.Items, err = s.pendingItems(ctx, tx, userID); err != nil {
		return wp, err
	}
	var children []string
	if wp.Digests, children, err = s.pendingDigests(ctx, tx, userID); err != nil {
		return wp, err
	}

	if wp.RunID, err = s.openRun(ctx, tx, userID, today); err != nil {
		return wp, err
	}
	offered := children
	for _, it := range wp.Items {
		offered = append(offered, it.ID)
	}
	for _, d := range wp.Digests {
		offered = append(offered, d.ID)
	}
	if err := recordOffered(ctx, tx, wp.RunID, userID, offered); err != nil {
		return wp, err
	}
	if err := tx.Commit(); err != nil {
		return wp, err
	}
	wp.Day, wp.Timezone, wp.Instructions = today, tzName, instr
	return wp, nil
}

// openRun returns the run this call belongs to. A recent unfinished run is
// reused, so a client that calls Create in a loop does not add a row per call;
// otherwise a new run starts. Runs abandoned for staleRunAge are deleted.
func (s *Service) openRun(ctx context.Context, tx *sql.Tx, userID, today string) (string, error) {
	now := s.Now().UTC()
	stamp := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM runs WHERE user_id = ? AND summaries_at IS NULL AND started_at < ?`,
		userID, stamp(now.Add(-staleRunAge))); err != nil {
		return "", fmt.Errorf("prune runs: %w", err)
	}
	var id string
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM runs WHERE user_id = ? AND summaries_at IS NULL AND started_at >= ? ORDER BY started_at DESC LIMIT 1`,
		userID, stamp(now.Add(-runReuseWindow))).Scan(&id)
	switch {
	case err == nil:
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET day = ? WHERE id = ? AND user_id = ?`, today, id, userID); err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM run_items WHERE run_id = ? AND user_id = ?`, id, userID); err != nil {
			return "", err
		}
		return id, nil
	case errors.Is(err, sql.ErrNoRows):
		if id, err = s.id("run_"); err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO runs (id, user_id, started_at, day) VALUES (?, ?, ?, ?)`,
			id, userID, stamp(now), today); err != nil {
			return "", fmt.Errorf("insert run: %w", err)
		}
		return id, nil
	default:
		return "", err
	}
}

// recordOffered remembers which items and digests a run sent to Claude.
func recordOffered(ctx context.Context, tx *sql.Tx, runID, userID string, subjects []string) error {
	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO run_items (run_id, user_id, subject_id) VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, id := range subjects {
		if _, err := stmt.ExecContext(ctx, runID, userID, id); err != nil {
			return fmt.Errorf("record offered: %w", err)
		}
	}
	return nil
}

// bundleDigests attaches pending items of digest sources to one open digest
// per source, creating it if needed.
func (s *Service) bundleDigests(ctx context.Context, tx *sql.Tx, userID, today string) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT DISTINCT i.source_id FROM items i JOIN sources s ON s.user_id = i.user_id AND s.id = i.source_id
		 WHERE i.user_id = ? AND i.state = 'pending_light' AND i.digest_id IS NULL
		   AND s.enabled = 1 AND s.granularity = 'digest' ORDER BY i.source_id`, userID)
	if err != nil {
		return err
	}
	var sourceIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		sourceIDs = append(sourceIDs, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, src := range sourceIDs {
		var digestID string
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM digests WHERE user_id = ? AND source_id = ? AND state = 'pending_light' ORDER BY batch_date, id LIMIT 1`,
			userID, src).Scan(&digestID)
		if errors.Is(err, sql.ErrNoRows) {
			if digestID, err = s.id("dig_"); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx,
				`INSERT INTO digests (id, user_id, source_id, batch_date, state) VALUES (?, ?, ?, ?, 'pending_light')`,
				digestID, userID, src, today)
		}
		if err != nil {
			return fmt.Errorf("digest for %s: %w", src, err)
		}
		var attached int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM items WHERE user_id = ? AND digest_id = ?`, userID, digestID).Scan(&attached); err != nil {
			return err
		}
		if room := MaxDigestEntries - attached; room > 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE items SET digest_id = ? WHERE user_id = ? AND id IN (
				   SELECT id FROM items WHERE user_id = ? AND source_id = ? AND state = 'pending_light' AND digest_id IS NULL
				   ORDER BY COALESCE(published_at, created_at) DESC, id LIMIT ?)`,
				digestID, userID, userID, src, room); err != nil {
				return fmt.Errorf("attach to digest: %w", err)
			}
		}
	}
	return nil
}

func (s *Service) pendingItems(ctx context.Context, tx *sql.Tx, userID string) ([]Item, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT i.id, i.state, i.media_type, s.id, s.name, s.prompt_override, i.title, i.url, i.excerpt, i.content, i.light_summary
		 FROM items i JOIN sources s ON s.user_id = i.user_id AND s.id = i.source_id
		 WHERE i.user_id = ? AND s.enabled = 1
		   AND ((i.state = 'pending_light' AND i.digest_id IS NULL AND s.granularity = 'individual') OR i.state = 'pending_deep')
		 ORDER BY (i.state = 'pending_deep') DESC, COALESCE(i.published_at, i.created_at) DESC, i.id
		 LIMIT ?`, userID, MaxItemsPerRun)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Item{}
	for rows.Next() {
		var (
			it    Item
			state string
		)
		if err := rows.Scan(&it.ID, &state, &it.MediaType, &it.Source.ID, &it.Source.Name, &it.Source.PromptOverride,
			&it.Title, &it.URL, &it.Excerpt, &it.Content, &it.LightSummary); err != nil {
			return nil, err
		}
		it.Stage = "light"
		if state == "pending_deep" {
			it.Stage = "deep"
		} else {
			it.LightSummary, it.Content = nil, nil // a light summary uses the excerpt only
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// pendingDigests returns the open digests and the IDs of their entries.
func (s *Service) pendingDigests(ctx context.Context, tx *sql.Tx, userID string) ([]Digest, []string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT d.id, s.id, s.name, s.prompt_override FROM digests d JOIN sources s ON s.user_id = d.user_id AND s.id = d.source_id
		 WHERE d.user_id = ? AND d.state = 'pending_light' AND s.enabled = 1 ORDER BY d.batch_date, d.id`, userID)
	if err != nil {
		return nil, nil, err
	}
	digests := []Digest{}
	for rows.Next() {
		var d Digest
		if err := rows.Scan(&d.ID, &d.Source.ID, &d.Source.Name, &d.Source.PromptOverride); err != nil {
			rows.Close()
			return nil, nil, err
		}
		digests = append(digests, d)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}
	var children []string
	for i := range digests {
		er, err := tx.QueryContext(ctx,
			`SELECT id, title, url, excerpt FROM items WHERE user_id = ? AND digest_id = ? AND state = 'pending_light'
			 ORDER BY COALESCE(published_at, created_at), id`, userID, digests[i].ID)
		if err != nil {
			return nil, nil, err
		}
		digests[i].Entries = []Entry{}
		for er.Next() {
			var e Entry
			var id string
			if err := er.Scan(&id, &e.Title, &e.URL, &e.Excerpt); err != nil {
				er.Close()
				return nil, nil, err
			}
			digests[i].Entries = append(digests[i].Entries, e)
			children = append(children, id)
		}
		if err := er.Close(); err != nil {
			return nil, nil, err
		}
	}
	return digests, children, nil
}

// Summary is one submitted summary. ID is an item ID, or a digest ID for a
// digest.
type Summary struct {
	ID    string `json:"id"`
	Stage string `json:"stage"`
	MD    string `json:"summary_md"`
}

// Skip says why a submitted summary was not stored.
type Skip struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// Result reports what Submit stored.
type Result struct {
	Accepted int    `json:"accepted"`
	Skipped  []Skip `json:"skipped"`
}

// Submit stores summaries for a run. A summary is stored only if its item or
// digest belongs to userID, was offered by this run, and is still waiting for
// that stage, so sending the same batch twice, or sending someone else's ID,
// changes nothing. Summaries land on the day the run was created for.
func (s *Service) Submit(ctx context.Context, userID, runID string, in []Summary) (Result, error) {
	res := Result{Skipped: []Skip{}}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	var today, oldStats string
	if err := tx.QueryRowContext(ctx, `SELECT day, stats FROM runs WHERE id = ? AND user_id = ?`, runID, userID).Scan(&today, &oldStats); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return res, ErrRunNotFound
		}
		return res, err
	}
	if today == "" { // a run from before days were recorded
		if today, _, err = s.day(ctx, tx, userID); err != nil {
			return res, err
		}
	}
	offered, err := offeredBy(ctx, tx, runID, userID)
	if err != nil {
		return res, err
	}
	now := s.Now().UTC().Format(time.RFC3339)

	for _, sm := range in {
		skip := func(reason string) { res.Skipped = append(res.Skipped, Skip{ID: sm.ID, Reason: reason}) }
		md := strings.TrimSpace(sm.MD)
		switch {
		case md == "":
			skip("empty summary")
			continue
		case len(md) > MaxSummaryLen:
			skip("summary too long")
			continue
		case sm.Stage != "light" && sm.Stage != "deep":
			skip(fmt.Sprintf("unknown stage %q", sm.Stage))
			continue
		case !offered[sm.ID]:
			skip("not part of this run")
			continue
		}
		var q string
		var args []any
		switch {
		case strings.HasPrefix(sm.ID, "dig_") && sm.Stage == "light":
			q = `UPDATE digests SET state = 'light', summary = ?, batch_date = ? WHERE id = ? AND user_id = ? AND state = 'pending_light'`
			args = []any{md, today, sm.ID, userID}
		case sm.Stage == "light":
			q = `UPDATE items SET state = 'light', light_summary = ?, batch_date = ?, updated_at = ?
			     WHERE id = ? AND user_id = ? AND state = 'pending_light' AND digest_id IS NULL`
			args = []any{md, today, now, sm.ID, userID}
		default:
			q = `UPDATE items SET state = 'deep', deep_summary = ?, batch_date = ?, updated_at = ?
			     WHERE id = ? AND user_id = ? AND state = 'pending_deep'`
			args = []any{md, today, now, sm.ID, userID}
		}
		r, err := tx.ExecContext(ctx, q, args...)
		if err != nil {
			return res, err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			skip("not waiting for a " + sm.Stage + " summary")
			continue
		}
		if strings.HasPrefix(sm.ID, "dig_") && sm.Stage == "light" {
			// Only the entries this run sent are covered by the digest summary;
			// entries that arrived since stay pending for the next one.
			if _, err := tx.ExecContext(ctx,
				`UPDATE items SET state = 'light', batch_date = ?, updated_at = ?
				 WHERE digest_id = ? AND user_id = ? AND state = 'pending_light'
				   AND id IN (SELECT subject_id FROM run_items WHERE run_id = ? AND user_id = ?)`,
				today, now, sm.ID, userID, runID, userID); err != nil {
				return res, err
			}
		}
		if sm.Stage == "deep" && s.OnDeep != nil {
			if err := s.OnDeep(ctx, tx, userID, sm.ID); err != nil {
				return res, fmt.Errorf("queue deep save: %w", err)
			}
		}
		res.Accepted++
	}

	// A retry must not erase what the run did: keep the first finish time and
	// add to the counts.
	stats := map[string]int{}
	_ = json.Unmarshal([]byte(oldStats), &stats)
	stats["accepted"] += res.Accepted
	stats["submissions"]++
	enc, _ := json.Marshal(stats)
	if _, err := tx.ExecContext(ctx,
		`UPDATE runs SET summaries_at = COALESCE(summaries_at, ?), finished_at = COALESCE(finished_at, ?), stats = ?
		 WHERE id = ? AND user_id = ?`, now, now, string(enc), runID, userID); err != nil {
		return res, err
	}
	return res, tx.Commit()
}

// offeredBy returns the set of item and digest IDs a run sent to Claude.
func offeredBy(ctx context.Context, tx *sql.Tx, runID, userID string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT subject_id FROM run_items WHERE run_id = ? AND user_id = ?`, runID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
