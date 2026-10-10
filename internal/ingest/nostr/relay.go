package nostr

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// RelayFetcher reads events from relays over WebSocket. It opens one
// subscription per relay, reads until the relay sends EOSE, and closes it:
// ingest runs on a timer, so nothing stays connected between runs.
type RelayFetcher struct {
	// Timeout bounds the whole exchange with one relay. Zero means 20 seconds.
	Timeout time.Duration
	// Limit is the most events asked of one relay. Zero means 500.
	Limit int
}

// ErrNoRelay means every relay failed.
var ErrNoRelay = errors.New("no relay answered")

// Fetch implements Fetcher. Relays are queried in parallel; one that fails or
// times out is skipped.
func (f RelayFetcher) Fetch(ctx context.Context, relays, authors []string, since time.Time) ([]Event, error) {
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		all      []Event
		answered int
		firstErr error
	)
	for _, r := range relays {
		wg.Add(1)
		go func() {
			defer wg.Done()
			evs, err := f.one(ctx, r, authors, since)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			answered++
			all = append(all, evs...)
		}()
	}
	wg.Wait()
	if answered == 0 {
		if firstErr != nil {
			return nil, fmt.Errorf("%w: %v", ErrNoRelay, firstErr)
		}
		return nil, ErrNoRelay
	}
	return all, nil
}

func (f RelayFetcher) one(ctx context.Context, relay string, authors []string, since time.Time) ([]Event, error) {
	timeout, limit := f.Timeout, f.Limit
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	if limit <= 0 {
		limit = 500
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c, _, err := websocket.Dial(ctx, relay, nil)
	if err != nil {
		return nil, fmt.Errorf("relay %s: %w", hostOf(relay), unwrapURL(err))
	}
	defer c.CloseNow()
	c.SetReadLimit(4 << 20)

	var sub [8]byte
	if _, err := rand.Read(sub[:]); err != nil {
		return nil, err
	}
	subID := hex.EncodeToString(sub[:])
	filter := map[string]any{"authors": authors, "kinds": []int{KindProfile, KindNote, KindArticle}, "limit": limit}
	if !since.IsZero() {
		filter["since"] = since.Unix()
	}
	req, err := json.Marshal([]any{"REQ", subID, filter})
	if err != nil {
		return nil, err
	}
	if err := c.Write(ctx, websocket.MessageText, req); err != nil {
		return nil, fmt.Errorf("relay %s: %w", hostOf(relay), unwrapURL(err))
	}
	var out []Event
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return nil, fmt.Errorf("relay %s: %w", hostOf(relay), unwrapURL(err))
		}
		var msg []json.RawMessage
		if json.Unmarshal(data, &msg) != nil || len(msg) < 2 {
			continue
		}
		var kind, id string
		if json.Unmarshal(msg[0], &kind) != nil || json.Unmarshal(msg[1], &id) != nil || id != subID {
			continue
		}
		switch kind {
		case "EVENT":
			var e Event
			if len(msg) == 3 && json.Unmarshal(msg[2], &e) == nil {
				out = append(out, e)
			}
		case "EOSE":
			closeMsg, _ := json.Marshal([]any{"CLOSE", subID})
			_ = c.Write(ctx, websocket.MessageText, closeMsg)
			c.Close(websocket.StatusNormalClosure, "")
			return out, nil
		case "CLOSED":
			return nil, fmt.Errorf("relay %s closed the subscription", hostOf(relay))
		}
	}
}

func hostOf(relay string) string {
	if u, err := url.Parse(relay); err == nil && u.Host != "" {
		return u.Host
	}
	return "?"
}

// unwrapURL keeps a *url.Error's cause but drops the URL it carries.
func unwrapURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
