// Package karakeep saves items as Karakeep bookmarks.
//
// A link is created with POST /api/v1/bookmarks, which Karakeep itself
// de-duplicates by URL, then patched to the wanted summary and archived flag
// and tagged. Every step sets a desired state, so running the whole sequence
// again changes nothing. A text note has no URL to de-duplicate on, so the
// note carries a marker that is searched for before creating.
package karakeep

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/Jolls/Siftstr/internal/dest"
)

const markerPrefix = "siftstr:"

// Destination creates or updates a bookmark.
type Destination struct {
	base, token string
	hc          *http.Client
}

// New returns a Destination for the Karakeep at baseURL. hc may be nil.
func New(baseURL, token string, hc *http.Client) *Destination {
	if hc == nil {
		hc = dest.DefaultClient()
	}
	return &Destination{base: strings.TrimRight(baseURL, "/"), token: token, hc: hc}
}

// Factory builds a Destination from a saved connection.
func Factory(baseURL, secret string, _ json.RawMessage) dest.Destination {
	return New(baseURL, secret, nil)
}

// Name implements dest.Destination.
func (d *Destination) Name() string { return "karakeep" }

func (d *Destination) header() map[string]string {
	return map[string]string{"Authorization": "Bearer " + d.token}
}

type bookmark struct {
	ID   string `json:"id"`
	Note string `json:"note"`
}

// Send implements dest.Destination.
func (d *Destination) Send(ctx context.Context, item dest.Item) error {
	var id string
	var err error
	if item.Note {
		id, err = d.ensureNote(ctx, item)
	} else {
		if item.URL == "" {
			return dest.Permanent(errors.New("item has no URL to bookmark"))
		}
		id, err = d.ensureLink(ctx, item)
	}
	if err != nil {
		return err
	}
	if _, err := dest.Call(ctx, d.hc, http.MethodPatch, d.base+"/api/v1/bookmarks/"+url.PathEscape(id), d.header(),
		map[string]any{"summary": item.Summary, "archived": item.Archived}, nil); err != nil {
		return err
	}
	if len(item.Tags) == 0 {
		return nil
	}
	tags := make([]map[string]string, len(item.Tags))
	for i, t := range item.Tags {
		tags[i] = map[string]string{"tagName": t}
	}
	_, err = dest.Call(ctx, d.hc, http.MethodPost, d.base+"/api/v1/bookmarks/"+url.PathEscape(id)+"/tags", d.header(),
		map[string]any{"tags": tags}, nil)
	return err
}

func (d *Destination) ensureLink(ctx context.Context, item dest.Item) (string, error) {
	var b bookmark
	_, err := dest.Call(ctx, d.hc, http.MethodPost, d.base+"/api/v1/bookmarks", d.header(),
		map[string]any{"type": "link", "url": item.URL, "title": item.Title, "archived": item.Archived, "summary": item.Summary}, &b)
	if err == nil && b.ID == "" {
		err = errors.New("karakeep returned no bookmark id")
	}
	return b.ID, err
}

func (d *Destination) ensureNote(ctx context.Context, item dest.Item) (string, error) {
	marker := markerPrefix + item.ID
	var found struct {
		Bookmarks []bookmark `json:"bookmarks"`
	}
	q := url.Values{"q": {marker}}
	if _, err := dest.Call(ctx, d.hc, http.MethodGet, d.base+"/api/v1/bookmarks/search?"+q.Encode(), d.header(), nil, &found); err != nil {
		return "", err
	}
	for _, b := range found.Bookmarks {
		if strings.Contains(b.Note, marker) {
			return b.ID, nil
		}
	}
	var b bookmark
	_, err := dest.Call(ctx, d.hc, http.MethodPost, d.base+"/api/v1/bookmarks", d.header(),
		map[string]any{"type": "text", "text": item.Summary, "title": item.Title, "note": marker, "archived": item.Archived, "summary": item.Summary}, &b)
	if err == nil && b.ID == "" {
		err = errors.New("karakeep returned no bookmark id")
	}
	return b.ID, err
}
