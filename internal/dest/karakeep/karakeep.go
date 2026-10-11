// Package karakeep saves items as Karakeep bookmarks.
//
// A link is created with POST /api/v1/bookmarks, which Karakeep itself
// de-duplicates by URL, and is sent with its archived flag every time (a
// re-save without one unarchives the bookmark). Tags are then attached. Every
// step sets a desired state, so running the whole sequence again changes
// nothing. Other verbs work on a bookmark the item already has: update its
// flag and tags, delete it, or overwrite its summary. A text note has no URL
// to de-duplicate on, so the note carries a marker that is searched for
// before creating.
package karakeep

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/Jolls/Siftstr/internal/dest"
)

const markerPrefix = "siftstr:"

// Destination creates or updates a bookmark.
type Destination struct {
	c  *Client
	id string // the bookmark of the last save
}

// New returns a Destination for the Karakeep at baseURL. hc may be nil.
func New(baseURL, token string, hc *http.Client) *Destination {
	return &Destination{c: NewClient(baseURL, token, hc)}
}

// Factory builds a Destination from a saved connection.
func Factory(baseURL, secret string, _ json.RawMessage) dest.Destination {
	return New(baseURL, secret, nil)
}

// Name implements dest.Destination.
func (d *Destination) Name() string { return "karakeep" }

// BookmarkID implements dest.BookmarkReporter: the bookmark the last Send
// created or found, or "" if it did not involve one.
func (d *Destination) BookmarkID() string { return d.id }

// Send implements dest.Destination.
func (d *Destination) Send(ctx context.Context, item dest.Item) error {
	d.id = ""
	switch item.Verb {
	case dest.VerbDelete:
		if item.BookmarkID == "" {
			return dest.Permanent(errors.New("no bookmark to delete"))
		}
		return d.c.Delete(ctx, item.BookmarkID)
	case dest.VerbSummary:
		if item.BookmarkID == "" || strings.TrimSpace(item.Summary) == "" {
			return dest.Permanent(errors.New("no bookmark or summary to write"))
		}
		return d.c.Patch(ctx, item.BookmarkID, map[string]any{"summary": item.Summary})
	case dest.VerbUpdate:
		if item.BookmarkID == "" {
			return dest.Permanent(errors.New("no bookmark to update"))
		}
		if err := d.c.Patch(ctx, item.BookmarkID, map[string]any{"archived": item.Archived}); err != nil {
			return err
		}
		d.id = item.BookmarkID
		return d.c.AttachTags(ctx, item.BookmarkID, item.Tags)
	case dest.VerbSave:
		return d.save(ctx, item)
	case dest.VerbCreate:
		if item.URL == "" {
			return dest.Permanent(errors.New("item has no URL to bookmark"))
		}
		// A bookmark made with no action from the user must not touch one the
		// user already has: it is theirs, and Karakeep would take the archived
		// flag we send. Nothing is recorded for it either, so a later delete
		// can never remove it.
		if existing, err := d.c.CheckURL(ctx, item.URL); err != nil || existing != "" {
			return err
		}
		return d.save(ctx, item)
	}
	return dest.Permanent(errors.New("unknown verb " + item.Verb))
}

func (d *Destination) save(ctx context.Context, item dest.Item) error {
	var id string
	var err error
	if item.Note {
		id, err = d.ensureNote(ctx, item)
	} else {
		if item.URL == "" {
			return dest.Permanent(errors.New("item has no URL to bookmark"))
		}
		var b Bookmark
		b, err = d.c.CreateLink(ctx, item.URL, item.Title, item.Archived)
		id = b.ID
	}
	if err != nil {
		return err
	}
	d.id = id
	if item.Note && item.Summary != "" {
		// Karakeep does not summarize a text note, so ours is written with it.
		if err := d.c.Patch(ctx, id, map[string]any{"summary": item.Summary, "archived": item.Archived}); err != nil {
			return err
		}
	}
	return d.c.AttachTags(ctx, id, item.Tags)
}

func (d *Destination) ensureNote(ctx context.Context, item dest.Item) (string, error) {
	marker := markerPrefix + item.ID
	found, err := d.c.SearchNotes(ctx, marker)
	if err != nil {
		return "", err
	}
	for _, b := range found {
		if strings.Contains(b.Note, marker) {
			return b.ID, nil
		}
	}
	b, err := d.c.CreateNote(ctx, item.Summary, item.Title, marker)
	return b.ID, err
}
