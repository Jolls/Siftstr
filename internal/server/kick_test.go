package server

import (
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Jolls/Siftstr/internal/dest"
)

type kickCall struct {
	User string
	Jobs []string
}

// kickRec records what the server asked the scheduler to run.
type kickRec struct {
	mu    sync.Mutex
	calls []kickCall
}

func (k *kickRec) kick(userID string, jobs ...string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls = append(k.calls, kickCall{userID, append([]string(nil), jobs...)})
}

func (k *kickRec) take() []kickCall {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := k.calls
	k.calls = nil
	return out
}

func kickEnv(t *testing.T, fc *fakeCheck) (*env, *kickRec) {
	t.Helper()
	rec := &kickRec{}
	e := setupDeps(t, nil, false, func(d *Deps) {
		d.Kick = rec.kick
		if fc != nil {
			d.CheckConnection = fc.check
		}
	})
	return e, rec
}

func (e *env) addConn(t *testing.T, userID, kind string) {
	t.Helper()
	e.exec(t, `INSERT INTO connections (id, user_id, kind, base_url) VALUES (?, ?, ?, 'https://x.example.com')`, "con_"+kind+"_"+userID, userID, kind)
}

// want checks the recorded kicks and the notice on a redirect.
func wantKick(t *testing.T, label string, rec *kickRec, resp *http.Response, notice string, calls ...kickCall) {
	t.Helper()
	got := rec.take()
	if len(calls) == 0 && len(got) == 0 {
		got = nil
	}
	if !reflect.DeepEqual(got, calls) && !(len(got) == 0 && len(calls) == 0) {
		t.Errorf("%s: kicks = %+v, want %+v", label, got, calls)
	}
	if notice != "" {
		if loc := resp.Header.Get("Location"); !strings.HasSuffix(loc, "notice="+notice) {
			t.Errorf("%s: Location = %q, want notice=%s", label, loc, notice)
		}
	}
}

func ingestForm(csrf, maxAge string, highlights bool) url.Values {
	v := url.Values{"csrf": {csrf}, "max_age_days": {maxAge}, "excerpt_length": {"500"}, "highlights_form": {"1"}}
	if highlights {
		v.Set("karakeep_highlights", "1")
	}
	return v
}

// Characterization: without a Kick, every save keeps its plain notice.
func TestSavesWithoutKickKeepSavedNotice(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	uid := e.userID(t, "alice")
	e.exec(t, `INSERT INTO sources (id, user_id, kind, external_id, name, enabled) VALUES ('s_a', ?, 'miniflux_feed', '1', 'A', 0)`, uid)
	for label, resp := range map[string]*http.Response{
		"ingest": e.post(t, c, "/settings/ingest", ingestForm(csrf, "30", true)),
		"source": e.post(t, c, "/settings/sources/s_a", url.Values{"csrf": {csrf}, "granularity": {"individual"}, "max_depth": {"light"}, "carryover": {"carry"}, "enabled": {"1"}}),
		"dest":   e.post(t, c, "/settings/destinations/miniflux", url.Values{"csrf": {csrf}, "base_url": {"https://mf.example.com"}, "secret": {"tok"}}),
		"nostr":  e.post(t, c, "/settings/nostr", url.Values{"csrf": {csrf}, "relays": {"wss://relay.example.com"}, "npubs": {testNpub}}),
	} {
		if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || !strings.HasSuffix(loc, "notice=saved") {
			t.Errorf("%s: %d -> %q", label, resp.StatusCode, loc)
		}
	}
}

func TestActivityReleaseNowRedirects(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	resp := e.post(t, c, "/settings/activity/release", url.Values{"csrf": {csrf}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/settings/activity?notice=released" {
		t.Fatalf("release = %d -> %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp := e.post(t, c, "/settings/activity/release", url.Values{}); resp.StatusCode == http.StatusSeeOther {
		t.Error("released without csrf")
	}
}

func TestHighlightsOnKicksHighlights(t *testing.T) {
	e, rec := kickEnv(t, nil)
	c, csrf := e.signedIn(t, "alice")
	alice := e.userID(t, "alice")
	e.addConn(t, alice, "karakeep")
	wantKick(t, "off->on", rec, e.post(t, c, "/settings/ingest", ingestForm(csrf, "14", true)), "fetching", kickCall{alice, []string{"highlights"}})
	wantKick(t, "on->on", rec, e.post(t, c, "/settings/ingest", ingestForm(csrf, "14", true)), "saved")
	wantKick(t, "on->off", rec, e.post(t, c, "/settings/ingest", ingestForm(csrf, "14", false)), "saved")

	bc, bcsrf := e.signedIn(t, "bob")
	wantKick(t, "no connection", rec, e.post(t, bc, "/settings/ingest", ingestForm(bcsrf, "14", true)), "saved")
	if resp := e.post(t, c, "/settings/ingest", ingestForm(csrf, "x", true)); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bad form = %d", resp.StatusCode)
	}
	wantKick(t, "rejected", rec, nil, "")
}

func TestMaxAgeRaiseKicksIngestJobs(t *testing.T) {
	e, rec := kickEnv(t, nil)
	c, csrf := e.signedIn(t, "alice")
	alice := e.userID(t, "alice")
	e.addConn(t, alice, "miniflux")
	e.addConn(t, alice, "nostr")
	two := []string{"ingest-miniflux", "ingest-nostr"}
	wantKick(t, "14->30", rec, e.post(t, c, "/settings/ingest", ingestForm(csrf, "30", false)), "fetching", kickCall{alice, two})
	wantKick(t, "30->14", rec, e.post(t, c, "/settings/ingest", ingestForm(csrf, "14", false)), "saved")
	wantKick(t, "14->0", rec, e.post(t, c, "/settings/ingest", ingestForm(csrf, "0", false)), "fetching", kickCall{alice, two})
	wantKick(t, "0->30", rec, e.post(t, c, "/settings/ingest", ingestForm(csrf, "30", false)), "saved")
	wantKick(t, "unchanged", rec, e.post(t, c, "/settings/ingest", ingestForm(csrf, "30", false)), "saved")
	e.addConn(t, alice, "karakeep")
	wantKick(t, "30->40 with karakeep", rec, e.post(t, c, "/settings/ingest", ingestForm(csrf, "40", false)), "fetching", kickCall{alice, []string{"ingest-miniflux", "ingest-nostr", "karakeep"}})
}

func TestSourceEnableKicksItsJob(t *testing.T) {
	e, rec := kickEnv(t, nil)
	c, csrf := e.signedIn(t, "alice")
	alice := e.userID(t, "alice")
	for _, k := range []string{"miniflux", "nostr", "karakeep"} {
		e.addConn(t, alice, k)
	}
	for id, kind := range map[string]string{"s_m": "miniflux_feed", "s_n": "nostr", "s_k": "karakeep"} {
		e.exec(t, `INSERT INTO sources (id, user_id, kind, external_id, name, enabled) VALUES (?, ?, ?, ?, ?, 0)`, id, alice, kind, id, id)
	}
	form := func(enabled bool) url.Values {
		v := url.Values{"csrf": {csrf}, "granularity": {"individual"}, "max_depth": {"light"}, "carryover": {"carry"}}
		if enabled {
			v.Set("enabled", "1")
		}
		return v
	}
	for id, job := range map[string]string{"s_m": "ingest-miniflux", "s_n": "ingest-nostr", "s_k": "karakeep"} {
		wantKick(t, id+" enable", rec, e.post(t, c, "/settings/sources/"+id, form(true)), "fetching", kickCall{alice, []string{job}})
		wantKick(t, id+" save again", rec, e.post(t, c, "/settings/sources/"+id, form(true)), "saved")
		wantKick(t, id+" disable", rec, e.post(t, c, "/settings/sources/"+id, form(false)), "saved")
	}
	bad := form(true)
	bad.Set("granularity", "weekly")
	if resp := e.post(t, c, "/settings/sources/s_m", bad); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bad form = %d", resp.StatusCode)
	}
	wantKick(t, "rejected", rec, nil, "")

	bc, bcsrf := e.signedIn(t, "bob")
	bob := e.userID(t, "bob")
	bv := form(true)
	bv.Set("csrf", bcsrf)
	if resp := e.post(t, bc, "/settings/sources/s_m", bv); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("other user's source = %d", resp.StatusCode)
	}
	wantKick(t, "other user", rec, nil, "")
	e.exec(t, `INSERT INTO sources (id, user_id, kind, external_id, name, enabled) VALUES ('s_b', ?, 'miniflux_feed', 'b', 'b', 0)`, bob)
	wantKick(t, "no connection", rec, e.post(t, bc, "/settings/sources/s_b", bv), "saved")
}

func TestConnectionSaveKicksWhenCheckPasses(t *testing.T) {
	fc := &fakeCheck{}
	e, rec := kickEnv(t, fc)
	c, csrf := e.signedIn(t, "alice")
	alice := e.userID(t, "alice")
	save := func(kind, base, secret string) *http.Response { return saveConn(t, e, c, csrf, kind, base, secret) }
	mf := kickCall{alice, []string{"ingest-miniflux"}}
	wantKick(t, "new", rec, save("miniflux", "https://mf.example.com", "tok"), "fetching", mf)
	wantKick(t, "same", rec, save("miniflux", "https://mf.example.com", ""), "saved")
	wantKick(t, "new url", rec, save("miniflux", "https://mf2.example.com", ""), "fetching", mf)
	wantKick(t, "new secret", rec, save("miniflux", "https://mf2.example.com", "tok2"), "fetching", mf)

	fc.fn = func(string, string, string) error { return fmt.Errorf("GET x: %w", &dest.StatusError{Code: 401}) }
	wantKick(t, "failed check", rec, save("miniflux", "https://mf3.example.com", ""), "saved")
	fc.fn = nil
	wantKick(t, "previous status not ok", rec, save("miniflux", "https://mf3.example.com", ""), "fetching", mf)

	wantKick(t, "karakeep", rec, save("karakeep", "https://kk.example.com", "key"), "fetching", kickCall{alice, []string{"karakeep", "highlights"}})
	wantKick(t, "metube", rec, save("metube", "https://mt.example.com", ""), "saved")
	if resp := save("miniflux", "ftp://bad", ""); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bad url = %d", resp.StatusCode)
	}
	wantKick(t, "rejected", rec, nil, "")
}

func TestConnectionSaveWithoutCheckerDoesNotKick(t *testing.T) {
	e, rec := kickEnv(t, nil)
	c, csrf := e.signedIn(t, "alice")
	wantKick(t, "no checker", rec, saveConn(t, e, c, csrf, "miniflux", "https://mf.example.com", "tok"), "saved")
}

func TestNostrSaveKicksOnChange(t *testing.T) {
	e, rec := kickEnv(t, nil)
	c, csrf := e.signedIn(t, "alice")
	alice := e.userID(t, "alice")
	save := func(relays, npubs string) *http.Response {
		return e.post(t, c, "/settings/nostr", url.Values{"csrf": {csrf}, "relays": {relays}, "npubs": {npubs}})
	}
	ni := kickCall{alice, []string{"ingest-nostr"}}
	wantKick(t, "new", rec, save("wss://relay.example.com", testNpub), "fetching", ni)
	wantKick(t, "same", rec, save("wss://relay.example.com", testNpub), "saved")
	wantKick(t, "relays only", rec, save("wss://relay.example.com", ""), "saved")
	wantKick(t, "back", rec, save("wss://relay.example.com", testNpub), "fetching", ni)
	if resp := save("http://bad", testNpub); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bad relay = %d", resp.StatusCode)
	}
	wantKick(t, "rejected", rec, nil, "")
}

func TestActivityKicksOutbox(t *testing.T) {
	e, rec := kickEnv(t, nil)
	c, csrf := e.signedIn(t, "alice")
	alice := e.userID(t, "alice")
	e.stopped(t, alice, "obx_1")
	ob := kickCall{alice, []string{"outbox"}}
	wantKick(t, "retry", rec, e.post(t, c, "/settings/activity/obx_1/retry", url.Values{"csrf": {csrf}}), "retried", ob)
	wantKick(t, "release", rec, e.post(t, c, "/settings/activity/release", url.Values{"csrf": {csrf}}), "released", ob)
	bc, bcsrf := e.signedIn(t, "bob")
	if resp := e.post(t, bc, "/settings/activity/obx_1/retry", url.Values{"csrf": {bcsrf}}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("other user's retry = %d", resp.StatusCode)
	}
	wantKick(t, "other user", rec, nil, "")
}

func TestKickIsPerUser(t *testing.T) {
	e, rec := kickEnv(t, nil)
	_, _ = e.signedIn(t, "alice")
	bc, bcsrf := e.signedIn(t, "bob")
	alice, bob := e.userID(t, "alice"), e.userID(t, "bob")
	e.addConn(t, alice, "karakeep")
	e.addConn(t, bob, "karakeep")
	wantKick(t, "bob", rec, e.post(t, bc, "/settings/ingest", ingestForm(bcsrf, "14", true)), "fetching", kickCall{bob, []string{"highlights"}})
}

func TestFetchingNoticeText(t *testing.T) {
	e := setup(t, nil, false)
	c, _ := e.signedIn(t, "alice")
	_, body := e.get(t, c, "/settings/sources?notice=fetching")
	if !strings.Contains(body, "Saved. Fetching in the background.") {
		t.Fatal("notice text missing")
	}
}
