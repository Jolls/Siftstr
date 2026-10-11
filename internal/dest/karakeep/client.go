package karakeep

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Jolls/Siftstr/internal/dest"
)

// Client is a thin wrapper over the Karakeep REST API. It holds no state, so
// it is safe to build per call. The destination, the ingestor and the summary
// poller each use only the methods they need (through their own interfaces).
type Client struct {
	base, token string
	hc          *http.Client
}

// NewClient returns a Client for the Karakeep at baseURL. hc may be nil.
func NewClient(baseURL, token string, hc *http.Client) *Client {
	if hc == nil {
		hc = dest.DefaultClient()
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), token: token, hc: hc}
}

func (c *Client) header() map[string]string {
	return map[string]string{"Authorization": "Bearer " + c.token}
}

func (c *Client) path(id string, rest ...string) string {
	return c.base + "/api/v1/bookmarks/" + url.PathEscape(id) + strings.Join(rest, "")
}

// Summarization states Karakeep reports for a bookmark.
const (
	SummaryPending = "pending"
	SummaryDone    = "success"
	SummaryFailed  = "failure"
)

// Bookmark is the part of a Karakeep bookmark that Siftstr reads.
type Bookmark struct {
	ID        string
	Archived  bool
	CreatedAt time.Time
	Title     string // the title set on the bookmark itself, which may be empty
	Note      string
	Summary   string
	// SummaryStatus is Karakeep's summarizationStatus: pending until its AI
	// has run, then success or failure. It is empty when AI summarization is
	// not configured, which Siftstr cannot otherwise tell from "not yet".
	SummaryStatus string
	Tags          []string // tag names, whoever attached them
	Type          string   // link, text or asset
	URL           string   // a link's address
	PageTitle     string   // the crawled page's title
	Publisher     string   // the site or publisher Karakeep found when it crawled the page
	Description   string
	// AlreadyExisted is set on a create reply when Karakeep found the URL.
	AlreadyExisted bool
}

// UnmarshalJSON flattens Karakeep's nested bookmark shape.
func (b *Bookmark) UnmarshalJSON(data []byte) error {
	var raw struct {
		ID                  string  `json:"id"`
		Archived            bool    `json:"archived"`
		CreatedAt           string  `json:"createdAt"`
		Title               *string `json:"title"`
		Note                *string `json:"note"`
		Summary             *string `json:"summary"`
		SummarizationStatus *string `json:"summarizationStatus"`
		AlreadyExists       bool    `json:"alreadyExists"`
		Tags                []struct {
			Name string `json:"name"`
		} `json:"tags"`
		Content *struct {
			Type        string  `json:"type"`
			URL         string  `json:"url"`
			Title       *string `json:"title"`
			Description *string `json:"description"`
			Publisher   *string `json:"publisher"`
		} `json:"content"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	str := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	*b = Bookmark{ID: raw.ID, Archived: raw.Archived, Title: str(raw.Title), Note: str(raw.Note), Summary: str(raw.Summary),
		SummaryStatus: str(raw.SummarizationStatus), AlreadyExisted: raw.AlreadyExists}
	if raw.CreatedAt != "" {
		if t, err := time.Parse(time.RFC3339Nano, raw.CreatedAt); err == nil {
			b.CreatedAt = t
		}
	}
	for _, t := range raw.Tags {
		b.Tags = append(b.Tags, t.Name)
	}
	if raw.Content != nil {
		b.Type, b.URL, b.PageTitle, b.Description = raw.Content.Type, raw.Content.URL, str(raw.Content.Title), str(raw.Content.Description)
		b.Publisher = str(raw.Content.Publisher)
	}
	return nil
}

// HasTag reports whether the bookmark carries the named tag.
func (b Bookmark) HasTag(name string) bool {
	for _, t := range b.Tags {
		if t == name {
			return true
		}
	}
	return false
}

// Page is one page of a bookmark listing.
type Page struct {
	Bookmarks  []Bookmark
	NextCursor string // empty on the last page
}

// ListUnarchived returns one page of the user's unarchived bookmarks, newest
// first. Pass the previous page's NextCursor to continue; keep limit small,
// since a page of full bookmarks is large.
func (c *Client) ListUnarchived(ctx context.Context, cursor string, limit int) (Page, error) {
	q := url.Values{"archived": {"false"}, "sortOrder": {"desc"}, "limit": {strconv.Itoa(limit)}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	var out struct {
		Bookmarks  []Bookmark `json:"bookmarks"`
		NextCursor *string    `json:"nextCursor"`
	}
	if _, err := dest.Call(ctx, c.hc, http.MethodGet, c.base+"/api/v1/bookmarks?"+q.Encode(), c.header(), nil, &out); err != nil {
		return Page{}, err
	}
	p := Page{Bookmarks: out.Bookmarks}
	if out.NextCursor != nil {
		p.NextCursor = *out.NextCursor
	}
	return p, nil
}

// ErrNotFound means Karakeep has no such bookmark.
var ErrNotFound = errors.New("karakeep: bookmark not found")

// Get returns one bookmark.
func (c *Client) Get(ctx context.Context, id string) (Bookmark, error) {
	var b Bookmark
	status, err := dest.Call(ctx, c.hc, http.MethodGet, c.path(id), c.header(), nil, &b)
	if status == http.StatusNotFound {
		return b, ErrNotFound
	}
	return b, err
}

// CreateLink creates a link bookmark with the wanted archived flag. Karakeep
// de-duplicates by URL: an existing bookmark is returned (AlreadyExisted) and
// takes the flag, so it must always be sent, or a re-save would unarchive it.
func (c *Client) CreateLink(ctx context.Context, link, title string, archived bool) (Bookmark, error) {
	body := map[string]any{"type": "link", "url": link, "archived": archived}
	if title != "" {
		body["title"] = title
	}
	var b Bookmark
	if _, err := dest.Call(ctx, c.hc, http.MethodPost, c.base+"/api/v1/bookmarks", c.header(), body, &b); err != nil {
		return b, err
	}
	if b.ID == "" {
		return b, errors.New("karakeep returned no bookmark id")
	}
	return b, nil
}

// CheckURL returns the ID of the bookmark Karakeep already has for a URL, or
// "" if there is none. It changes nothing.
func (c *Client) CheckURL(ctx context.Context, link string) (string, error) {
	var out struct {
		BookmarkID *string `json:"bookmarkId"`
	}
	if _, err := dest.Call(ctx, c.hc, http.MethodGet, c.base+"/api/v1/bookmarks/check-url?"+url.Values{"url": {link}}.Encode(), c.header(), nil, &out); err != nil {
		return "", err
	}
	if out.BookmarkID == nil {
		return "", nil
	}
	return *out.BookmarkID, nil
}

// CreateNote creates a text bookmark.
func (c *Client) CreateNote(ctx context.Context, text, title, note string) (Bookmark, error) {
	var b Bookmark
	if _, err := dest.Call(ctx, c.hc, http.MethodPost, c.base+"/api/v1/bookmarks", c.header(),
		map[string]any{"type": "text", "text": text, "title": title, "note": note}, &b); err != nil {
		return b, err
	}
	if b.ID == "" {
		return b, errors.New("karakeep returned no bookmark id")
	}
	return b, nil
}

// SearchNotes returns bookmarks matching q (used to find a note by its marker).
func (c *Client) SearchNotes(ctx context.Context, q string) ([]Bookmark, error) {
	var out struct {
		Bookmarks []Bookmark `json:"bookmarks"`
	}
	_, err := dest.Call(ctx, c.hc, http.MethodGet, c.base+"/api/v1/bookmarks/search?"+url.Values{"q": {q}}.Encode(), c.header(), nil, &out)
	return out.Bookmarks, err
}

// Patch changes the given fields of a bookmark (for example archived or
// summary). Fields left out are untouched.
func (c *Client) Patch(ctx context.Context, id string, fields map[string]any) error {
	status, err := dest.Call(ctx, c.hc, http.MethodPatch, c.path(id), c.header(), fields, nil)
	if status == http.StatusNotFound {
		return dest.Permanent(ErrNotFound)
	}
	return err
}

// Delete removes a bookmark. One that is already gone counts as deleted.
func (c *Client) Delete(ctx context.Context, id string) error {
	status, err := dest.Call(ctx, c.hc, http.MethodDelete, c.path(id), c.header(), nil, nil)
	if status == http.StatusNotFound {
		return nil
	}
	return err
}

func tagBody(names []string) map[string]any {
	tags := make([]map[string]string, len(names))
	for i, t := range names {
		tags[i] = map[string]string{"tagName": t}
	}
	return map[string]any{"tags": tags}
}

// AttachTags adds tags by name, creating them if needed. Attaching one that
// is already there changes nothing.
func (c *Client) AttachTags(ctx context.Context, id string, names []string) error {
	if len(names) == 0 {
		return nil
	}
	status, err := dest.Call(ctx, c.hc, http.MethodPost, c.path(id, "/tags"), c.header(), tagBody(names), nil)
	if status == http.StatusNotFound {
		return dest.Permanent(ErrNotFound)
	}
	return err
}

// DetachTags removes tags by name from a bookmark. The tags themselves stay.
func (c *Client) DetachTags(ctx context.Context, id string, names []string) error {
	if len(names) == 0 {
		return nil
	}
	status, err := dest.Call(ctx, c.hc, http.MethodDelete, c.path(id, "/tags"), c.header(), tagBody(names), nil)
	if status == http.StatusNotFound {
		return nil
	}
	return err
}
