package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

const testNpub = "npub10elfcs4fr0l0r8af98jlmgdh9c8tcxjvz9qkw038js35mp4dma8qzvjptg"

func TestNostrSettingsSaveShowAndDisconnect(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")

	code, body := e.get(t, c, "/settings/nostr")
	if code != 200 || !strings.Contains(body, "Connect") {
		t.Fatalf("empty page (%d):\n%s", code, body)
	}

	resp := e.post(t, c, "/settings/nostr", url.Values{"csrf": {csrf}, "relays": {"wss://relay.example.com"}, "npubs": {testNpub}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save = %d", resp.StatusCode)
	}
	_, body = e.get(t, c, "/settings/nostr?notice=saved")
	if !strings.Contains(body, "wss://relay.example.com") || !strings.Contains(body, testNpub) || !strings.Contains(body, "Disconnect") {
		t.Fatalf("page after save:\n%s", body)
	}

	// Saving again updates the one connection.
	e.post(t, c, "/settings/nostr", url.Values{"csrf": {csrf}, "relays": {"wss://two.example.com"}, "npubs": {""}})
	var n int
	_ = e.st.DB().QueryRow(`SELECT COUNT(*) FROM connections WHERE kind = 'nostr'`).Scan(&n)
	if n != 1 {
		t.Fatalf("connections = %d", n)
	}

	resp = e.post(t, c, "/settings/nostr/delete", url2(csrf))
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	_ = e.st.DB().QueryRow(`SELECT COUNT(*) FROM connections WHERE kind = 'nostr'`).Scan(&n)
	if n != 0 {
		t.Fatal("connection not deleted")
	}
}

func TestNostrSettingsRejectBadInputAndKeepWhatWasTyped(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	resp := e.post(t, c, "/settings/nostr", url.Values{"csrf": {csrf}, "relays": {"https://relay.example.com"}, "npubs": {"npub1typed"}})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body := e.body(t, resp)
	if !strings.Contains(body, "npub1typed") || !strings.Contains(body, "not a ws:// or wss://") {
		t.Fatalf("error page:\n%s", body)
	}
	var n int
	_ = e.st.DB().QueryRow(`SELECT COUNT(*) FROM connections`).Scan(&n)
	if n != 0 {
		t.Fatal("a rejected save stored something")
	}
	// No CSRF token, no change.
	if r := e.post(t, c, "/settings/nostr", url.Values{"relays": {"wss://relay.example.com"}}); r.StatusCode != http.StatusForbidden {
		t.Fatalf("no csrf = %d", r.StatusCode)
	}
}

func TestNostrSettingsAreIsolatedPerUser(t *testing.T) {
	e := setup(t, nil, false)
	a, aCSRF := e.signedIn(t, "alice")
	b, _ := e.signedIn(t, "bob")
	e.post(t, a, "/settings/nostr", url.Values{"csrf": {aCSRF}, "relays": {"wss://alice-relay.example.com"}, "npubs": {testNpub}})
	_, body := e.get(t, b, "/settings/nostr")
	if strings.Contains(body, "alice-relay") || strings.Contains(body, testNpub) {
		t.Fatalf("bob sees alice's Nostr settings:\n%s", body)
	}
}
