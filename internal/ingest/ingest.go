// Package ingest holds what the ingestors share: where a new item starts, and
// the Karakeep bookmark a source with summary_source = karakeep gets at ingest.
// The ingestors themselves live in subpackages (miniflux, nostr, karakeep).
package ingest

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Jolls/Siftstr/internal/dest"
	"github.com/Jolls/Siftstr/internal/outbox"
)

// BookmarkKeyPrefix starts the outbox dedupe key of an ingest-time bookmark.
const BookmarkKeyPrefix = "karakeep-ingest:"

// Summary sources.
const (
	SummaryAgent    = "agent"
	SummaryKarakeep = "karakeep"
)

// Source is the part of a source row that decides how its items start.
type Source struct {
	ID            string
	Enabled       bool
	Granularity   string // individual | digest
	SummarySource string // agent | karakeep
}

// Start returns the state and batch date a new item of this source starts
// with. An individual item is ready to triage at once, on the user's local
// day it was ingested (ARCHITECTURE.md section 5, and Q26). A digest entry
// waits for its digest's summary, which gives it its day.
func (s Source) Start(day string) (state string, batchDate any) {
	if s.Granularity == "digest" {
		return "pending_light", nil
	}
	return "light", day
}

// UsesKarakeep reports whether items of this source are bookmarked in
// Karakeep at ingest, given the user's Karakeep connection ID ("" if none).
// Without a connection the source behaves as `agent`, and a digest has no
// page to summarize.
func (s Source) UsesKarakeep(connID string) bool {
	return s.SummarySource == SummaryKarakeep && s.Granularity != "digest" && connID != ""
}

// ViaKarakeep is a SQL condition on a sources row aliased s: items of this
// source take their summary from Karakeep rather than from Claude. It holds
// for a Karakeep source and for a feed set to summary_source = karakeep, and
// only while the user has a Karakeep connection, so a source without one
// behaves as `agent` (ARCHITECTURE.md section 6).
const ViaKarakeep = `(s.kind = 'karakeep' OR s.summary_source = 'karakeep') AND s.granularity = 'individual'
	AND EXISTS (SELECT 1 FROM connections kc WHERE kc.user_id = s.user_id AND kc.kind = 'karakeep')`

// Queryer is satisfied by *sql.DB and *sql.Tx.
type Queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// KarakeepConnection returns the ID of userID's Karakeep connection, or "".
func KarakeepConnection(ctx context.Context, q Queryer, userID string) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, `SELECT id FROM connections WHERE user_id = ? AND kind = 'karakeep' ORDER BY id LIMIT 1`, userID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// QueueBookmark queues the archived Karakeep bookmark for a new item, so
// Karakeep can crawl and summarize it before the user decides anything. It
// goes straight to the outbox with no hold, since no action caused it.
func QueueBookmark(ctx context.Context, x outbox.Execer, now time.Time, userID, connID, itemID, url, title, media string) error {
	payload, err := json.Marshal(dest.Item{ID: itemID, URL: url, Title: title, MediaType: media, Archived: true, Verb: dest.VerbCreate})
	if err != nil {
		return err
	}
	_, err = outbox.Enqueue(ctx, x, now, outbox.Entry{
		UserID: userID, ConnectionID: connID, Op: "karakeep", Payload: payload,
	}, BookmarkKeyPrefix+itemID)
	return err
}
