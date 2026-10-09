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
func DefaultClient() *http.Client { return &http.Client{Timeout: 30 * time.Second} }

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

// redact keeps the path and drops any query string from a URL in an error.
func redact(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i]
	}
	return u
}
