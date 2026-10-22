package writeback

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Jolls/Siftstr/internal/connections"
	"github.com/Jolls/Siftstr/internal/dest"
	"github.com/Jolls/Siftstr/internal/ingest"
	"github.com/Jolls/Siftstr/internal/outbox"
)

// Sender sends outbox entries through the destination for their connection.
type Sender struct {
	// DB is where an item's Karakeep bookmark is looked up when an entry is
	// sent, and recorded when a save reports one.
	DB        *sql.DB
	Conns     *connections.Service
	Factories map[string]dest.Factory // by connection kind
	Now       func() time.Time        // time.Now if nil
}

func (s *Sender) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Handle is the outbox.Runner handler. It never logs or returns a secret.
func (s *Sender) Handle(ctx context.Context, e outbox.Entry) error {
	if e.ConnectionID == "" {
		return dest.Permanent(errors.New("the connection was removed"))
	}
	conn, err := s.Conns.Get(ctx, e.UserID, e.ConnectionID)
	if errors.Is(err, connections.ErrNotFound) {
		return dest.Permanent(errors.New("the connection was removed"))
	} else if err != nil {
		return err
	}
	f, ok := s.Factories[conn.Kind]
	if !ok {
		return dest.Permanent(fmt.Errorf("no destination for %q", conn.Kind))
	}
	observe := func(err error) {
		s.Conns.Observe(ctx, e.UserID, conn, connections.ViaOutbox, s.now(), err)
	}
	secret, err := s.Conns.Secret(ctx, e.UserID, e.ConnectionID)
	switch {
	case errors.Is(err, connections.ErrNoSecret):
		// MeTube's token is optional. The others cannot work without one, and
		// sending an empty token would only earn a retried 401.
		if conn.Kind != MeTube {
			observe(err)
			return dest.Permanent(fmt.Errorf("no secret is saved for the %s connection", conn.Kind))
		}
	case err != nil:
		observe(err)
		return err // unreadable: retry, the user may re-enter it
	}
	var item dest.Item
	if err := json.Unmarshal(e.Payload, &item); err != nil {
		return dest.Permanent(fmt.Errorf("bad payload: %w", err))
	}
	if conn.Kind == Karakeep {
		skip, err := s.prepareKarakeep(ctx, e, &item)
		if err != nil || skip {
			return err
		}
	}
	d := f(conn.BaseURL, secret, conn.Config)
	err = d.Send(ctx, item)
	observe(err)
	if err != nil {
		return err
	}
	if item.Verb == dest.VerbDelete && s.DB != nil && item.BookmarkID != "" {
		// The bookmark is gone, so the item has none: a link to it would lead
		// nowhere. Matching the ID leaves a newer bookmark alone.
		if _, err := s.DB.ExecContext(ctx, `UPDATE items SET karakeep_id = NULL WHERE user_id = ? AND id = ? AND karakeep_id = ?`,
			e.UserID, item.ID, item.BookmarkID); err != nil {
			return fmt.Errorf("clear bookmark: %w", err)
		}
	}
	if r, ok := d.(dest.BookmarkReporter); ok && s.DB != nil && r.BookmarkID() != "" && (item.Verb == dest.VerbSave || item.Verb == dest.VerbCreate) {
		// A failure here returns an error, so the whole entry is sent again,
		// which is safe: a save sets a desired state.
		if _, err := s.DB.ExecContext(ctx, `UPDATE items SET karakeep_id = ? WHERE user_id = ? AND id = ?
			AND (karakeep_id IS NULL OR karakeep_id != ?)`, r.BookmarkID(), e.UserID, item.ID, r.BookmarkID()); err != nil {
			return fmt.Errorf("record bookmark: %w", err)
		}
	}
	return nil
}

// prepareKarakeep fills in what a Karakeep entry needs from the database when
// it is sent, since the bookmark may have been created after it was queued. It
// reports skip when there is nothing left to do.
func (s *Sender) prepareKarakeep(ctx context.Context, e outbox.Entry, item *dest.Item) (skip bool, err error) {
	if s.DB == nil {
		return false, nil
	}
	if item.Verb == dest.VerbHighlightColor {
		// The pick's color now, not the one the entry was queued with, so two quick
		// changes end with the latest even when the first send had to be retried.
		var color string
		err := s.DB.QueryRowContext(ctx, `SELECT color FROM highlight_picks WHERE user_id = ? AND highlight_id = ? ORDER BY day DESC LIMIT 1`,
			e.UserID, item.HighlightID).Scan(&color)
		if errors.Is(err, sql.ErrNoRows) {
			return true, nil // the pick is gone: nothing to send
		}
		if err != nil {
			return false, err
		}
		item.HighlightColor = color
		return false, nil
	}
	var state, bookmark string
	err = s.DB.QueryRowContext(ctx, `SELECT state, COALESCE(karakeep_id, '') FROM items WHERE user_id = ? AND id = ?`,
		e.UserID, item.ID).Scan(&state, &bookmark)
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}

	// The bookmark made at ingest is only wanted while the item is still
	// waiting to be triaged. Once the user has acted, the action's own entry
	// makes or fixes the bookmark, and sending this one late could archive
	// what a keep just unarchived.
	if item.Verb == dest.VerbCreate && strings.HasPrefix(e.Key, ingest.BookmarkKeyPrefix) {
		return !found || state != "light", nil
	}
	if !item.NeedsBookmark() {
		return false, nil
	}
	item.BookmarkID = bookmark
	if bookmark != "" {
		return false, nil
	}
	switch item.Verb {
	case dest.VerbDelete:
		return true, nil // never created, so nothing to delete
	case dest.VerbUpdate:
		if item.URL == "" {
			return false, dest.Permanent(errors.New("the item has no bookmark and no URL to make one from"))
		}
		item.Verb = dest.VerbSave // make it, in the wanted state
		return false, nil
	}
	// Summary: the bookmark is still to come, so wait for it without using up
	// the entry's retries.
	return false, waiting{errors.New("the item's bookmark is not created yet")}
}

// waiting marks an error as "not yet": the outbox tries again later without
// counting it as a failed attempt.
type waiting struct{ error }

func (waiting) Defer() time.Duration { return deferFor }
func (w waiting) Unwrap() error      { return w.error }

const deferFor = 5 * time.Minute
