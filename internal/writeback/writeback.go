// Package writeback turns a released action into outbox entries (the plan),
// and sends outbox entries to the user's destinations (the sender). The table
// it implements is in ARCHITECTURE.md section 7.
package writeback

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Jolls/Siftstr/internal/dest"
	"github.com/Jolls/Siftstr/internal/outbox"
	"github.com/Jolls/Siftstr/internal/triage"
)

// Destination kinds, which are also outbox ops.
const (
	Miniflux = "miniflux"
	Karakeep = "karakeep"
	MeTube   = "metube"
)

// User settings (user_settings keys) holding a comma-separated list of
// destination kinds for kept items of that media type. Unset means the
// default below.
const (
	SettingVideoDestinations   = "video_destinations"
	SettingPodcastDestinations = "podcast_destinations"
)

// Tags added in Karakeep.
const (
	TagSifted   = "sifted"
	TagPromoted = "promoted"
	TagKept     = "kept"
)

// VideoCapable lists the kinds a user can pick as video or podcast
// destinations.
var VideoCapable = []string{Karakeep, MeTube}

// Planner queues the upstream effects of triage outcomes.
type Planner struct {
	Now func() time.Time
}

// NewPlanner returns a Planner using the real clock.
func NewPlanner() *Planner { return &Planner{Now: time.Now} }

type subject struct {
	typ, id    string
	url, title string
	media      string
	state      string
	light      string
	deep       string
	entryIDs   []int64
}

// OnRelease is the triage.Service.OnRelease hook.
func (p *Planner) OnRelease(ctx context.Context, tx *sql.Tx, userID string, rel []triage.Released) error {
	conns, err := connectionIDs(ctx, tx, userID)
	if err != nil {
		return err
	}
	for _, r := range rel {
		sub, err := load(ctx, tx, userID, r.SubjectType, r.SubjectID)
		if errors.Is(err, sql.ErrNoRows) {
			continue // the subject was deleted since
		} else if err != nil {
			return err
		}
		if err := p.plan(ctx, tx, userID, conns, r, sub); err != nil {
			return fmt.Errorf("action %s: %w", r.ActionID, err)
		}
	}
	return nil
}

func (p *Planner) plan(ctx context.Context, tx *sql.Tx, userID string, conns map[string]string, r triage.Released, sub subject) error {
	send := func(kind string, item dest.Item) error {
		id, ok := conns[kind]
		if !ok {
			return nil // the user has not connected this destination
		}
		payload, err := json.Marshal(item)
		if err != nil {
			return err
		}
		_, err = outbox.Enqueue(ctx, tx, p.Now(), outbox.Entry{
			UserID: userID, ActionID: r.ActionID, ConnectionID: id, Op: kind, Payload: payload,
		}, kind+":"+r.ActionID)
		return err
	}

	// Every outcome marks the entries read (promote included: ARCHITECTURE.md
	// section 14, Q10). Nostr items have none, so nothing is sent for them.
	if len(sub.entryIDs) > 0 {
		if err := send(Miniflux, dest.Item{ID: sub.id, EntryIDs: sub.entryIDs}); err != nil {
			return err
		}
	}

	switch r.Outcome {
	case "kept":
		item := dest.Item{ID: sub.id, URL: sub.url, Title: sub.title, MediaType: sub.media, Summary: sub.summary(),
			Tags: []string{TagSifted, TagKept}}
		if sub.typ == triage.SubjectDigest {
			item.Note = true // a digest has no URL of its own
			return send(Karakeep, item)
		}
		kinds, err := keepDestinations(ctx, tx, userID, sub.media)
		if err != nil {
			return err
		}
		for _, k := range kinds {
			it := item
			if k != Karakeep {
				it.Tags = nil
			}
			if err := send(k, it); err != nil {
				return err
			}
		}
	case "pending_deep":
		// Karakeep waits for the deep summary. If it already arrived during
		// the hold, send now; otherwise OnDeep sends it.
		return p.deepSave(ctx, tx, userID, conns, sub.id)
	}
	return nil
}

// OnDeep is the runs.Service.OnDeep hook: an item has just received its deep
// summary.
func (p *Planner) OnDeep(ctx context.Context, tx *sql.Tx, userID, itemID string) error {
	conns, err := connectionIDs(ctx, tx, userID)
	if err != nil {
		return err
	}
	return p.deepSave(ctx, tx, userID, conns, itemID)
}

// deepSave queues the archived, tagged Karakeep bookmark for a promoted item,
// once both halves have happened: the promote's hold has ended and the deep
// summary exists. Whichever happens second calls this and queues it; the
// dedupe key keeps it to one.
func (p *Planner) deepSave(ctx context.Context, tx *sql.Tx, userID string, conns map[string]string, itemID string) error {
	id, ok := conns[Karakeep]
	if !ok {
		return nil
	}
	var actionID string
	err := tx.QueryRowContext(ctx,
		`SELECT a.action_id FROM actions a JOIN items i ON i.user_id = a.user_id AND i.id = a.subject_id
		 WHERE a.user_id = ? AND a.subject_type = 'item' AND a.subject_id = ? AND a.kind = 'promote'
		   AND a.status = 'released' AND a.prev_state = 'light' AND i.state = 'deep' AND i.deep_summary IS NOT NULL
		 ORDER BY a.client_at DESC LIMIT 1`, userID, itemID).Scan(&actionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // not both halves yet
	} else if err != nil {
		return err
	}
	sub, err := load(ctx, tx, userID, triage.SubjectItem, itemID)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(dest.Item{ID: sub.id, URL: sub.url, Title: sub.title, MediaType: sub.media,
		Summary: sub.summary(), Archived: true, Tags: []string{TagSifted, TagPromoted}})
	if err != nil {
		return err
	}
	_, err = outbox.Enqueue(ctx, tx, p.Now(), outbox.Entry{
		UserID: userID, ActionID: actionID, ConnectionID: id, Op: Karakeep, Payload: payload,
	}, "karakeep-promote:"+itemID)
	return err
}

func (s subject) summary() string {
	if s.deep != "" {
		return s.deep
	}
	return s.light
}

func load(ctx context.Context, tx *sql.Tx, userID, typ, id string) (subject, error) {
	sub := subject{typ: typ, id: id}
	switch typ {
	case triage.SubjectItem:
		var light, deep sql.NullString
		var kind, ext string
		err := tx.QueryRowContext(ctx,
			`SELECT i.url, i.title, i.media_type, i.state, i.light_summary, i.deep_summary, s.kind, i.external_id
			 FROM items i JOIN sources s ON s.user_id = i.user_id AND s.id = i.source_id
			 WHERE i.user_id = ? AND i.id = ?`, userID, id).Scan(&sub.url, &sub.title, &sub.media, &sub.state, &light, &deep, &kind, &ext)
		if err != nil {
			return sub, err
		}
		sub.light, sub.deep = light.String, deep.String
		if n, err := strconv.ParseInt(ext, 10, 64); err == nil && kind == "miniflux_feed" {
			sub.entryIDs = []int64{n}
		}
		return sub, nil
	case triage.SubjectDigest:
		var summary sql.NullString
		err := tx.QueryRowContext(ctx,
			`SELECT d.state, d.summary, s.name FROM digests d JOIN sources s ON s.user_id = d.user_id AND s.id = d.source_id
			 WHERE d.user_id = ? AND d.id = ?`, userID, id).Scan(&sub.state, &summary, &sub.title)
		if err != nil {
			return sub, err
		}
		sub.light = summary.String
		sub.media = "article"
		rows, err := tx.QueryContext(ctx,
			`SELECT i.external_id FROM items i JOIN sources s ON s.user_id = i.user_id AND s.id = i.source_id
			 WHERE i.user_id = ? AND i.digest_id = ? AND s.kind = 'miniflux_feed' ORDER BY i.id`, userID, id)
		if err != nil {
			return sub, err
		}
		defer rows.Close()
		for rows.Next() {
			var ext string
			if err := rows.Scan(&ext); err != nil {
				return sub, err
			}
			if n, err := strconv.ParseInt(ext, 10, 64); err == nil {
				sub.entryIDs = append(sub.entryIDs, n)
			}
		}
		return sub, rows.Err()
	}
	return sub, sql.ErrNoRows
}

// connectionIDs maps each kind to the user's connection of that kind. If a
// user somehow has two, the lowest ID wins, so the choice is stable.
func connectionIDs(ctx context.Context, tx *sql.Tx, userID string) (map[string]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT kind, id FROM connections WHERE user_id = ? ORDER BY id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, id string
		if err := rows.Scan(&k, &id); err != nil {
			return nil, err
		}
		out[k] = id // DESC order, so the last write is the lowest ID
	}
	return out, rows.Err()
}

// keepDestinations returns the kinds a kept item of this media type goes to,
// in a stable order. Articles and posts always go to Karakeep. Videos and
// podcasts follow the user's multi-select; unset, a video goes to every
// connected video-capable destination and a podcast to Karakeep.
func keepDestinations(ctx context.Context, tx *sql.Tx, userID, media string) ([]string, error) {
	var key string
	var def []string
	switch media {
	case "video":
		key, def = SettingVideoDestinations, VideoCapable
	case "podcast":
		key, def = SettingPodcastDestinations, []string{Karakeep}
	default:
		return []string{Karakeep}, nil
	}
	var v string
	err := tx.QueryRowContext(ctx, `SELECT value FROM user_settings WHERE user_id = ? AND key = ?`, userID, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return def, nil
	} else if err != nil {
		return nil, err
	}
	return ParseKinds(v), nil
}

// ParseKinds reads a stored comma-separated list, keeping only capable kinds.
func ParseKinds(v string) []string {
	var out []string
	for _, k := range strings.Split(v, ",") {
		k = strings.TrimSpace(k)
		if slices.Contains(VideoCapable, k) && !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out
}
