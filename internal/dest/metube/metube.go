// Package metube asks a MeTube instance to download a video. Siftstr never
// downloads media itself.
package metube

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Jolls/Siftstr/internal/dest"
)

// Destination adds the item's URL to MeTube's queue unless MeTube already
// knows it.
type Destination struct {
	base, secret string
	hc           *http.Client
}

// New returns a Destination for the MeTube at baseURL. secret is optional and
// sent as a bearer token, for MeTube behind an authenticating proxy.
func New(baseURL, secret string, hc *http.Client) *Destination {
	if hc == nil {
		hc = dest.DefaultClient()
	}
	return &Destination{base: strings.TrimRight(baseURL, "/"), secret: secret, hc: hc}
}

// Factory builds a Destination from a saved connection.
func Factory(baseURL, secret string, _ json.RawMessage) dest.Destination {
	return New(baseURL, secret, nil)
}

// Name implements dest.Destination.
func (d *Destination) Name() string { return "metube" }

func (d *Destination) header() map[string]string {
	if d.secret == "" {
		return nil
	}
	return map[string]string{"Authorization": "Bearer " + d.secret}
}

// Send implements dest.Destination.
func (d *Destination) Send(ctx context.Context, item dest.Item) error {
	if item.URL == "" {
		return dest.Permanent(errNoURL)
	}
	var h struct {
		Done []struct {
			URL string `json:"url"`
		} `json:"done"`
		Queue []struct {
			URL string `json:"url"`
		} `json:"queue"`
		Pending []struct {
			URL string `json:"url"`
		} `json:"pending"`
	}
	if _, err := dest.Call(ctx, d.hc, http.MethodGet, d.base+"/history", d.header(), nil, &h); err != nil {
		return err
	}
	for _, list := range [][]struct {
		URL string `json:"url"`
	}{h.Done, h.Queue, h.Pending} {
		for _, e := range list {
			if e.URL == item.URL {
				return nil // already added
			}
		}
	}
	_, err := dest.Call(ctx, d.hc, http.MethodPost, d.base+"/add", d.header(),
		map[string]any{"url": item.URL, "quality": "best", "format": "any", "auto_start": true}, nil)
	return err
}
