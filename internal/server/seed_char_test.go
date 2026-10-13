package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// A saved Destinations form edits an existing row of its kind in place, which
// is what lets a URL-only seeded connection get its key from the UI.
func TestDestinationSaveUpdatesExistingRowOfKind(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	e.exec(t, `INSERT INTO connections (id, user_id, kind, base_url) VALUES ('con_seed', ?, 'miniflux', 'https://mf.example.com')`, e.userID(t, "alice"))
	resp := e.post(t, c, "/settings/destinations/miniflux", url.Values{"csrf": {csrf}, "base_url": {"https://mf2.example.com"}, "secret": {"tok-KEY"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save = %d", resp.StatusCode)
	}
	var n int
	var id, base string
	var has bool
	_ = e.st.DB().QueryRow(`SELECT COUNT(*) FROM connections WHERE kind = 'miniflux'`).Scan(&n)
	_ = e.st.DB().QueryRow(`SELECT id, base_url, secret_enc IS NOT NULL FROM connections WHERE kind = 'miniflux'`).Scan(&id, &base, &has)
	if n != 1 || id != "con_seed" || base != "https://mf2.example.com" || !has {
		t.Fatalf("%d %q %q %v", n, id, base, has)
	}
	if _, body := e.get(t, c, "/settings/destinations"); strings.Contains(body, "tok-KEY") {
		t.Error("key rendered")
	}
}
