// Package miniflux ingests a user's Miniflux feeds and unread entries.
package miniflux

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Feed is a Miniflux feed with its category.
type Feed struct {
	ID       int64
	Title    string
	Category string
}

// Enclosure is a media attachment on an entry.
type Enclosure struct {
	URL      string
	MimeType string
}

// Entry is one unread Miniflux entry.
type Entry struct {
	ID         int64
	FeedID     int64
	Title      string
	URL        string
	Content    string // HTML
	Enclosures []Enclosure
}

// API is the slice of Miniflux that ingest needs. Tests use a fake.
type API interface {
	Feeds(ctx context.Context) ([]Feed, error)
	// UnreadEntries returns every unread entry, oldest first.
	UnreadEntries(ctx context.Context) ([]Entry, error)
}

// Client talks to the Miniflux REST API with an API token.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// NewClient returns a Client for the Miniflux at baseURL.
func NewClient(baseURL, token string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), token: token, http: hc}
}

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Auth-Token", c.token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("miniflux %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("miniflux %s: status %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(out); err != nil {
		return fmt.Errorf("miniflux %s: decode: %w", path, err)
	}
	return nil
}

// Feeds implements API.
func (c *Client) Feeds(ctx context.Context) ([]Feed, error) {
	var raw []struct {
		ID       int64  `json:"id"`
		Title    string `json:"title"`
		Category struct {
			Title string `json:"title"`
		} `json:"category"`
	}
	if err := c.get(ctx, "/v1/feeds", nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Feed, len(raw))
	for i, f := range raw {
		out[i] = Feed{ID: f.ID, Title: f.Title, Category: f.Category.Title}
	}
	return out, nil
}

const pageSize = 100

// UnreadEntries implements API. It pages by offset, oldest first.
func (c *Client) UnreadEntries(ctx context.Context) ([]Entry, error) {
	var out []Entry
	for offset := 0; ; offset += pageSize {
		var page struct {
			Total   int `json:"total"`
			Entries []struct {
				ID         int64  `json:"id"`
				FeedID     int64  `json:"feed_id"`
				Title      string `json:"title"`
				URL        string `json:"url"`
				Content    string `json:"content"`
				Enclosures []struct {
					URL      string `json:"url"`
					MimeType string `json:"mime_type"`
				} `json:"enclosures"`
			} `json:"entries"`
		}
		q := url.Values{
			"status":    {"unread"},
			"order":     {"id"},
			"direction": {"asc"},
			"limit":     {strconv.Itoa(pageSize)},
			"offset":    {strconv.Itoa(offset)},
		}
		if err := c.get(ctx, "/v1/entries", q, &page); err != nil {
			return nil, err
		}
		for _, e := range page.Entries {
			en := Entry{ID: e.ID, FeedID: e.FeedID, Title: e.Title, URL: e.URL, Content: e.Content}
			for _, x := range e.Enclosures {
				en.Enclosures = append(en.Enclosures, Enclosure{URL: x.URL, MimeType: x.MimeType})
			}
			out = append(out, en)
		}
		if len(page.Entries) == 0 || offset+len(page.Entries) >= page.Total {
			return out, nil
		}
	}
}
