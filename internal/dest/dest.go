// Package dest defines the interface the outbox uses to reach an upstream
// service. The adapters live in subpackages (miniflux, karakeep, metube).
package dest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"time"
)

// Item is a snapshot of what to send, taken when the effect is enqueued, so
// sending never has to read triage state. Each adapter uses the fields it
// needs.
type Item struct {
	ID        string   `json:"id"` // Siftstr item or digest ID
	URL       string   `json:"url,omitempty"`
	Title     string   `json:"title,omitempty"`
	MediaType string   `json:"media_type,omitempty"`
	Summary   string   `json:"summary,omitempty"`
	Note      bool     `json:"note,omitempty"`     // save as a text note, not a link (digests)
	Archived  bool     `json:"archived,omitempty"` // the desired archived flag
	Tags      []string `json:"tags,omitempty"`
	EntryIDs  []int64  `json:"entry_ids,omitempty"` // Miniflux entries

	// Verb says what to do with the bookmark. The default creates or finds it
	// by URL and sets its archived flag and tags; the others work on a
	// bookmark that already exists.
	Verb string `json:"verb,omitempty"`
	// BookmarkID is the Karakeep bookmark for the verbs that need one. It is
	// filled in when the entry is sent, from what Siftstr has recorded for the
	// item, since the bookmark may have been created after the entry was queued.
	BookmarkID string `json:"bookmark_id,omitempty"`
}

// Karakeep verbs for Item.Verb.
const (
	VerbSave    = ""        // create or find by URL; set archived and tags (an action of the user, so it may take over an existing bookmark)
	VerbCreate  = "create"  // make the bookmark only if Karakeep has none for the URL; an existing one belongs to the user and is left alone
	VerbUpdate  = "update"  // set archived and tags on the item's bookmark
	VerbDelete  = "delete"  // delete the item's bookmark
	VerbSummary = "summary" // overwrite the item's bookmark summary
)

// NeedsBookmark reports whether the verb works on an existing bookmark.
func (i Item) NeedsBookmark() bool {
	return i.Verb == VerbUpdate || i.Verb == VerbDelete || i.Verb == VerbSummary
}

// BookmarkReporter is implemented by a destination that learns the bookmark ID
// of what it just sent, so the sender can record it for the item.
type BookmarkReporter interface {
	BookmarkID() string
}

// Destination sends an item to one upstream. Send must be idempotent: the
// outbox may call it again after a crash or a timeout.
type Destination interface {
	Name() string
	Send(ctx context.Context, item Item) error
}

// Factory builds a Destination from a saved connection.
type Factory func(baseURL, secret string, config json.RawMessage) Destination

// permanentError marks a failure that retrying cannot fix.
type permanentError struct{ error }

func (permanentError) Permanent() bool { return true }
func (p permanentError) Unwrap() error { return p.error }

// Permanent wraps err so the outbox gives up on it at once.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

// DefaultClient is used when an adapter is given no HTTP client.
// It does not follow redirects: a POST replayed as a GET would come back 200
// and be mistaken for success, so a redirect is reported as an error instead.
func DefaultClient() *http.Client {
	return &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Call sends a request with an optional JSON body and decodes a JSON reply
// into out (when non-nil and the reply has a body). Any status outside 2xx is
// an error. Only 400 and 422 are permanent: a rejected credential or a server
// that is down may be fixed by the user, so those retry with backoff. The
// body of an error reply is never included, since it can echo secrets.
func Call(ctx context.Context, hc *http.Client, method, url string, header map[string]string, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, Permanent(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return 0, Permanent(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%s %s: %w", method, redact(url), errors.Unwrap(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		err := fmt.Errorf("%s %s: status %d", method, redact(url), resp.StatusCode)
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			err = fmt.Errorf("%w (redirect: check that the saved base URL is the final address, e.g. https)", err)
		}
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity {
			return resp.StatusCode, Permanent(err)
		}
		return resp.StatusCode, err
	}
	if out != nil {
		data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		if err != nil {
			return resp.StatusCode, err
		}
		if len(bytes.TrimSpace(data)) > 0 {
			if err := json.Unmarshal(data, out); err != nil {
				return resp.StatusCode, fmt.Errorf("%s %s: decode: %w", method, redact(url), err)
			}
		}
	}
	return resp.StatusCode, nil
}

// redact keeps scheme, host and path, and drops any credentials and query
// string from a URL in an error.
func redact(u string) string {
	p, err := neturl.Parse(u)
	if err != nil {
		if i := strings.IndexByte(u, '?'); i >= 0 {
			u = u[:i]
		}
		if i := strings.Index(u, "@"); i >= 0 {
			u = u[i+1:]
		}
		return u
	}
	p.User, p.RawQuery, p.Fragment = nil, "", ""
	return p.String()
}
