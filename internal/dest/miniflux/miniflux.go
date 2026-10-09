// Package miniflux marks entries read in Miniflux.
package miniflux

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Jolls/Siftstr/internal/dest"
)

// Destination marks the item's Miniflux entries read. Marking an entry that
// is already read changes nothing, so a repeat is harmless.
type Destination struct {
	base, token string
	hc          *http.Client
}

// New returns a Destination for the Miniflux at baseURL. hc may be nil.
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
func (d *Destination) Name() string { return "miniflux" }

// Send implements dest.Destination.
func (d *Destination) Send(ctx context.Context, item dest.Item) error {
	if len(item.EntryIDs) == 0 {
		return nil
	}
	_, err := dest.Call(ctx, d.hc, http.MethodPut, d.base+"/v1/entries",
		map[string]string{"X-Auth-Token": d.token},
		map[string]any{"entry_ids": item.EntryIDs, "status": "read"}, nil)
	return err
}
