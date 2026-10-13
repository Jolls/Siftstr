package server

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/Jolls/Siftstr/internal/triage"
	"github.com/Jolls/Siftstr/web"
)

// signedIn creates a user, logs in and returns the client and its CSRF token.
func (e *env) signedIn(t *testing.T, name string) (*http.Client, string) {
	t.Helper()
	if _, err := e.auth.CreateUser(context.Background(), name, "correct horse battery", "user"); err != nil {
		t.Fatal(err)
	}
	c := e.client()
	if resp := e.login(t, c, name, "correct horse battery"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d", resp.StatusCode)
	}
	resp, err := c.Get(e.srv.URL + "/settings/destinations")
	if err != nil {
		t.Fatal(err)
	}
	m := csrfRe.FindStringSubmatch(e.body(t, resp))
	if m == nil {
		t.Fatal("no csrf token on settings page")
	}
	return c, m[1]
}

func (e *env) post(t *testing.T, c *http.Client, path string, v url.Values) *http.Response {
	t.Helper()
	resp, err := c.PostForm(e.srv.URL+path, v)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (e *env) get(t *testing.T, c *http.Client, path string) (int, string) {
	t.Helper()
	resp, err := c.Get(e.srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, e.body(t, resp)
}

func TestSettingsRequireLoginAndCSRF(t *testing.T) {
	e := setup(t, nil, false)
	c := e.client()
	for _, p := range []string{"/settings", "/settings/sources", "/settings/destinations"} {
		resp, _ := c.Get(e.srv.URL + p)
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
			t.Errorf("%s: %d -> %q", p, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	c, _ = e.signedIn(t, "alice")
	resp := e.post(t, c, "/settings/destinations/miniflux", url.Values{"base_url": {"https://mf.example.com"}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("post without csrf = %d", resp.StatusCode)
	}
}

func TestDestinationLifecycleNeverEchoesSecret(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	form := func(base, secret string) url.Values {
		return url.Values{"csrf": {csrf}, "base_url": {base}, "secret": {secret}}
	}

	resp := e.post(t, c, "/settings/destinations/miniflux", form("https://mf.example.com", "tok-SECRET"))
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create = %d", resp.StatusCode)
	}
	code, body := e.get(t, c, "/settings/destinations?notice=saved")
	if code != 200 || strings.Contains(body, "tok-SECRET") || !strings.Contains(body, "https://mf.example.com") ||
		!strings.Contains(body, "Saved. Enter a new one") || !strings.Contains(body, "Saved.") {
		t.Fatalf("page after create (%d) wrong:\n%s", code, body)
	}

	// A blank secret keeps the saved one.
	e.post(t, c, "/settings/destinations/miniflux", form("https://mf2.example.com", ""))
	var base, has = "", false
	if err := e.st.DB().QueryRow(`SELECT base_url, secret_enc IS NOT NULL FROM connections`).Scan(&base, &has); err != nil {
		t.Fatal(err)
	}
	if base != "https://mf2.example.com" || !has {
		t.Fatalf("after blank-secret save: %q %v", base, has)
	}

	// An explicit clear removes it.
	v := form("https://mf2.example.com", "")
	v.Set("clear_secret", "1")
	e.post(t, c, "/settings/destinations/miniflux", v)
	_ = e.st.DB().QueryRow(`SELECT secret_enc IS NOT NULL FROM connections`).Scan(&has)
	if has {
		t.Fatal("secret not cleared")
	}

	resp = e.post(t, c, "/settings/destinations/miniflux/delete", url2(csrf))
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	var n int
	_ = e.st.DB().QueryRow(`SELECT COUNT(*) FROM connections`).Scan(&n)
	if n != 0 {
		t.Fatal("connection not deleted")
	}
}

func url2(csrf string) url.Values { return url.Values{"csrf": {csrf}} }

func TestDestinationValidationAndUnknownKind(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	resp := e.post(t, c, "/settings/destinations/miniflux", url.Values{"csrf": {csrf}, "base_url": {"ftp://x.example"}})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bad url = %d", resp.StatusCode)
	}
	resp = e.post(t, c, "/settings/destinations/bogus", url.Values{"csrf": {csrf}, "base_url": {"https://x.example"}})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown kind = %d", resp.StatusCode)
	}
}

func TestSourcesSaveAndIsolation(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	var aliceID, bobID string
	_ = e.st.DB().QueryRow(`SELECT id FROM users WHERE username = 'alice'`).Scan(&aliceID)
	if _, err := e.auth.CreateUser(context.Background(), "bob", "correct horse battery", "user"); err != nil {
		t.Fatal(err)
	}
	_ = e.st.DB().QueryRow(`SELECT id FROM users WHERE username = 'bob'`).Scan(&bobID)
	for _, q := range []struct{ id, user, name string }{{"s_a", aliceID, "Alice Feed"}, {"s_b", bobID, "Bob Feed"}} {
		if _, err := e.st.DB().Exec(`INSERT INTO sources (id, user_id, kind, external_id, name) VALUES (?, ?, 'miniflux_feed', '1', ?)`, q.id, q.user, q.name); err != nil {
			t.Fatal(err)
		}
	}

	code, body := e.get(t, c, "/settings/sources")
	if code != 200 || !strings.Contains(body, "Alice Feed") || strings.Contains(body, "Bob Feed") {
		t.Fatalf("list (%d) wrong", code)
	}

	save := url.Values{"csrf": {csrf}, "granularity": {"digest"}, "max_depth": {"light"}, "carryover": {"drop"}, "prompt_override": {"short"}}
	if resp := e.post(t, c, "/settings/sources/s_a", save); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save = %d", resp.StatusCode)
	}
	var gran string
	var enabled bool
	_ = e.st.DB().QueryRow(`SELECT granularity, enabled FROM sources WHERE id = 's_a'`).Scan(&gran, &enabled)
	if gran != "digest" || enabled { // checkbox omitted = disabled
		t.Fatalf("saved %q enabled=%v", gran, enabled)
	}

	if resp := e.post(t, c, "/settings/sources/s_b", save); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("other user's source = %d, want 404", resp.StatusCode)
	}
	_ = e.st.DB().QueryRow(`SELECT granularity FROM sources WHERE id = 's_b'`).Scan(&gran)
	if gran != "individual" {
		t.Fatal("bob's source was modified")
	}

	save.Set("granularity", "weekly")
	if resp := e.post(t, c, "/settings/sources/s_a", save); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid = %d", resp.StatusCode)
	}
}

func TestIngestSettings(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	_, body := e.get(t, c, "/settings/sources")
	if !strings.Contains(body, `name="max_age_days" min="0" max="3650" value="14"`) {
		t.Fatal("default max age not shown")
	}
	v := url.Values{"csrf": {csrf}, "max_age_days": {"30"}, "excerpt_length": {"300"}}
	if resp := e.post(t, c, "/settings/ingest", v); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save = %d", resp.StatusCode)
	}
	_, body = e.get(t, c, "/settings/sources")
	if !strings.Contains(body, `value="30"`) || !strings.Contains(body, `value="300"`) {
		t.Fatal("saved values not shown")
	}
	for _, bad := range []url.Values{
		{"csrf": {csrf}, "max_age_days": {"-1"}, "excerpt_length": {"300"}},
		{"csrf": {csrf}, "max_age_days": {"x"}, "excerpt_length": {"300"}},
		{"csrf": {csrf}, "max_age_days": {"5"}, "excerpt_length": {"10"}},
	} {
		if resp := e.post(t, c, "/settings/ingest", bad); resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("%v = %d", bad, resp.StatusCode)
		}
	}
}

func TestKeepDestinationsSaveAndIsolation(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	c2, _ := e.signedIn(t, "bob")
	_, body := e.get(t, c, "/settings/destinations")
	// Defaults: video goes to every capable destination, podcasts to Karakeep.
	if strings.Count(body, " checked>") != 3 {
		t.Fatalf("defaults: want 3 checked boxes\n%s", body)
	}
	v := url.Values{"csrf": {csrf}, "video": {"metube"}}
	if resp := e.post(t, c, "/settings/destinations/keep", v); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save = %d", resp.StatusCode)
	}
	_, body = e.get(t, c, "/settings/destinations")
	if strings.Count(body, " checked>") != 1 || !strings.Contains(body, `name="video" value="metube" checked>`) {
		t.Fatalf("saved choice not shown\n%s", body)
	}
	_, body = e.get(t, c2, "/settings/destinations")
	if strings.Count(body, " checked>") != 3 {
		t.Fatal("alice's choice leaked to bob")
	}
	// Without CSRF the save is refused.
	if resp := e.post(t, c, "/settings/destinations/keep", url.Values{"video": {"karakeep"}}); resp.StatusCode == http.StatusSeeOther {
		t.Fatal("saved without csrf")
	}
}

func TestSummarySourceAndKarakeepSettingsSave(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	uid := e.userID(t, "alice")
	e.exec(t, `INSERT INTO sources (id, user_id, kind, external_id, name) VALUES ('s_a', ?, 'miniflux_feed', '1', 'Alice Feed')`, uid)
	e.exec(t, `INSERT INTO sources (id, user_id, kind, external_id, name) VALUES ('s_k', ?, 'karakeep', 'karakeep', 'Karakeep')`, uid)

	save := url.Values{"csrf": {csrf}, "granularity": {"individual"}, "max_depth": {"deep"}, "carryover": {"carry"}, "enabled": {"1"}, "summary_source": {"karakeep"}}
	if resp := e.post(t, c, "/settings/sources/s_a", save); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save = %d", resp.StatusCode)
	}
	var ss string
	_ = e.st.DB().QueryRow(`SELECT summary_source FROM sources WHERE id = 's_a'`).Scan(&ss)
	if ss != "karakeep" {
		t.Fatalf("summary_source = %q", ss)
	}
	save.Set("granularity", "digest")
	if resp := e.post(t, c, "/settings/sources/s_a", save); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("a digest on Karakeep summaries = %d, want 422", resp.StatusCode)
	}
	_, body := e.get(t, c, "/settings/sources")
	if !strings.Contains(body, `name="summary_source"`) || !strings.Contains(body, `value="karakeep" selected`) {
		t.Fatal("the summary source choice is not on the page")
	}
	if strings.Count(body, `<option value="digest"`) != 1 {
		t.Fatal("the Karakeep source must not offer a digest")
	}

	// The two Karakeep summary settings: optional in the form, validated when given.
	v := url.Values{"csrf": {csrf}, "max_age_days": {"30"}, "excerpt_length": {"300"}, "karakeep_summary_delay_minutes": {"5"}, "karakeep_summary_timeout_hours": {"8"}}
	if resp := e.post(t, c, "/settings/ingest", v); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save = %d", resp.StatusCode)
	}
	_, body = e.get(t, c, "/settings/sources")
	if !strings.Contains(body, `name="karakeep_summary_delay_minutes" min="0" max="1440" value="5"`) || !strings.Contains(body, `value="8"`) {
		t.Fatal("saved Karakeep summary settings not shown")
	}
	v = url.Values{"csrf": {csrf}, "max_age_days": {"31"}, "excerpt_length": {"300"}}
	if resp := e.post(t, c, "/settings/ingest", v); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save without the optional fields = %d", resp.StatusCode)
	}
	_, body = e.get(t, c, "/settings/sources")
	if !strings.Contains(body, `max="1440" value="5"`) {
		t.Fatal("leaving the optional fields out must keep what was saved")
	}
	v.Set("karakeep_summary_timeout_hours", "0")
	if resp := e.post(t, c, "/settings/ingest", v); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bad timeout = %d", resp.StatusCode)
	}
}

func TestPhoneFlowSetting(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	c2, _ := e.signedIn(t, "bob")
	// The list only renders when it has a card; an earlier-day light card is in the backlog.
	for _, n := range []string{"alice", "bob"} {
		e.seedLight(t, n, "itm_"+n)
		e.exec(t, `UPDATE items SET batch_date = '2000-01-01', light_summary = 's' WHERE id = ?`, "itm_"+n)
	}
	_, body := e.get(t, c, "/backlog")
	if !strings.Contains(body, `data-flow="up"`) {
		t.Fatal("default is not bottom-up")
	}
	_, body = e.get(t, c, "/settings/appearance")
	if !strings.Contains(body, `value="up" checked`) {
		t.Fatal("settings page does not show bottom-up as chosen")
	}
	if resp := e.post(t, c, "/settings/display", url.Values{"csrf": {csrf}, "phone_flow": {"down"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save = %d", resp.StatusCode)
	}
	if _, body = e.get(t, c, "/backlog"); !strings.Contains(body, `data-flow="down"`) {
		t.Fatal("saved direction not used on the list")
	}
	if resp := e.post(t, c, "/settings/display", url.Values{"csrf": {csrf}, "phone_flow": {"sideways"}}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bad value = %d", resp.StatusCode)
	}
	// Another user keeps the default.
	if _, body = e.get(t, c2, "/backlog"); !strings.Contains(body, `data-flow="up"`) {
		t.Fatal("alice's setting leaked to bob")
	}
}

var themedPages = []string{"/today", "/backlog", "/read", "/settings/sources", "/settings/prompts",
	"/settings/keys", "/settings/activity", "/settings/nostr", "/settings/destinations", "/settings/appearance"}

func TestPagesFollowOSWithNoThemeSaved(t *testing.T) {
	e := setup(t, nil, false)
	c, _ := e.signedIn(t, "alice")
	for _, p := range themedPages {
		if _, body := e.get(t, c, p); !strings.Contains(body, `content="light dark"`) || strings.Contains(body, "data-theme") {
			t.Errorf("%s does not follow the OS", p)
		}
	}
	if _, body := e.get(t, e.client(), "/login"); !strings.Contains(body, `content="light dark"`) || strings.Contains(body, "data-theme") {
		t.Error("login does not follow the OS")
	}
}

func TestThemeSetting(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	c2, _ := e.signedIn(t, "bob")
	if _, body := e.get(t, c, "/settings/appearance"); !strings.Contains(body, `name="theme" value="auto" checked`) {
		t.Fatal("settings page does not show auto as chosen")
	}
	save := func(v url.Values) *http.Response { return e.post(t, c, "/settings/theme", v) }
	if resp := save(url.Values{"theme": {"dark"}}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no csrf = %d", resp.StatusCode)
	}
	resp := save(url.Values{"csrf": {csrf}, "theme": {"dark"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/settings/appearance?notice=saved" {
		t.Fatalf("save = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	for _, p := range themedPages {
		if _, body := e.get(t, c, p); !strings.Contains(body, `<html lang="en" data-theme="dark">`) || !strings.Contains(body, `content="dark"`) {
			t.Errorf("%s is not dark", p)
		}
	}
	if _, body := e.get(t, c, "/settings/appearance"); !strings.Contains(body, `name="theme" value="dark" checked`) {
		t.Error("settings page does not show dark as chosen")
	}
	// Another user keeps the default, and so does the login page.
	for _, p := range []string{"/today", "/settings/sources"} {
		if _, body := e.get(t, c2, p); strings.Contains(body, "data-theme") {
			t.Errorf("alice's theme leaked to bob on %s", p)
		}
	}
	if _, body := e.get(t, e.client(), "/login"); strings.Contains(body, "data-theme") {
		t.Error("login should follow the OS only")
	}
	// Bad values are rejected, and the 422 page still wears the saved theme.
	for _, v := range []url.Values{{"csrf": {csrf}, "theme": {"sepia"}}, {"csrf": {csrf}}} {
		resp := save(v)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("bad value %v = %d", v, resp.StatusCode)
		}
		if body := e.body(t, resp); !strings.Contains(body, "theme must be auto, light or dark.") || !strings.Contains(body, `data-theme="dark"`) {
			t.Error("422 page lacks the message or lost the theme")
		}
	}
	// It follows the user to a new session.
	c3 := e.client()
	if resp := e.login(t, c3, "alice", "correct horse battery"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login = %d", resp.StatusCode)
	}
	if _, body := e.get(t, c3, "/today"); !strings.Contains(body, `data-theme="dark"`) {
		t.Error("theme did not persist to a new session")
	}
	if resp := save(url.Values{"csrf": {csrf}, "theme": {"light"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save light = %d", resp.StatusCode)
	}
	if _, body := e.get(t, c, "/today"); !strings.Contains(body, `<html lang="en" data-theme="light">`) {
		t.Error("light not applied")
	}
	save(url.Values{"csrf": {csrf}, "theme": {"auto"}})
	if _, body := e.get(t, c, "/today"); strings.Contains(body, "data-theme") || !strings.Contains(body, `content="light dark"`) {
		t.Error("auto did not clear the theme")
	}
}

// Characterization (#36): moving Theme and list direction must leave the other Sources forms in place.
func TestSourcesPageKeepsOtherForms(t *testing.T) {
	e := setup(t, nil, false)
	c, _ := e.signedIn(t, "alice")
	_, body := e.get(t, c, "/settings/sources")
	for _, want := range []string{`action="/settings/timezone"`, `action="/settings/ingest"`} {
		if !strings.Contains(body, want) {
			t.Errorf("Sources lost %s", want)
		}
	}
}

func TestAppearancePage(t *testing.T) {
	e := setup(t, nil, false)
	if resp, err := e.client().Get(e.srv.URL + "/settings/appearance"); err != nil || resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("signed out = %v %v", resp, err)
	}
	c, _ := e.signedIn(t, "alice")
	code, body := e.get(t, c, "/settings/appearance")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	for _, want := range []string{
		`<h1>Appearance</h1>`, `action="/settings/theme"`, `action="/settings/accent"`, `action="/settings/display"`,
		`name="theme" value="auto" checked`, `name="accent" value="jade" checked`, `value="up" checked`,
		`href="/settings/appearance" aria-current="page"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if n := strings.Count(body, `name="accent"`); n != 16 {
		t.Errorf("%d accent radios, want 16", n)
	}
	if strings.Contains(body, `value="slate"`) {
		t.Error("neutrals are not offered")
	}
}

func TestThemeAndFlowMovedToAppearance(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	_, body := e.get(t, c, "/settings/sources")
	if strings.Contains(body, `name="theme"`) || strings.Contains(body, `name="phone_flow"`) {
		t.Error("Sources still has the theme or list direction form")
	}
	for path, v := range map[string]url.Values{
		"/settings/theme":   {"csrf": {csrf}, "theme": {"dark"}},
		"/settings/display": {"csrf": {csrf}, "phone_flow": {"down"}},
	} {
		resp := e.post(t, c, path, v)
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/settings/appearance?notice=saved" {
			t.Errorf("%s = %d %s", path, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	for path, v := range map[string]url.Values{
		"/settings/theme":   {"csrf": {csrf}, "theme": {"sepia"}},
		"/settings/display": {"csrf": {csrf}, "phone_flow": {"sideways"}},
	} {
		resp := e.post(t, c, path, v)
		if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(e.body(t, resp), `<h1>Appearance</h1>`) {
			t.Errorf("bad %s is not the Appearance page with 422", path)
		}
	}
}

func TestAccentSetting(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	if resp := e.post(t, c, "/settings/accent", url.Values{"accent": {"red"}}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no csrf = %d", resp.StatusCode)
	}
	resp := e.post(t, c, "/settings/accent", url.Values{"csrf": {csrf}, "accent": {"red"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/settings/appearance?notice=saved" {
		t.Fatalf("save = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	for _, p := range themedPages {
		if _, body := e.get(t, c, p); !strings.Contains(body, `<html lang="en" data-accent="red">`) {
			t.Errorf("%s has no red accent", p)
		}
	}
	if _, body := e.get(t, c, "/settings/appearance"); !strings.Contains(body, `name="accent" value="red" checked`) {
		t.Error("Appearance does not show red as chosen")
	}
	if resp := e.post(t, c, "/settings/theme", url.Values{"csrf": {csrf}, "theme": {"dark"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("theme = %d", resp.StatusCode)
	}
	if _, body := e.get(t, c, "/today"); !strings.Contains(body, `<html lang="en" data-theme="dark" data-accent="red">`) {
		t.Error("theme and accent should both render")
	}
	c2 := e.client()
	if resp := e.login(t, c2, "alice", "correct horse battery"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login = %d", resp.StatusCode)
	}
	if _, body := e.get(t, c2, "/today"); !strings.Contains(body, `data-accent="red"`) {
		t.Error("accent did not persist to a new session")
	}
	if resp := e.post(t, c, "/settings/accent", url.Values{"csrf": {csrf}, "accent": {"jade"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("jade = %d", resp.StatusCode)
	}
	if _, body := e.get(t, c, "/today"); strings.Contains(body, "data-accent") {
		t.Error("jade renders no data-accent")
	}
}

func TestAccentIsolation(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	c2, _ := e.signedIn(t, "bob")
	if resp := e.post(t, c, "/settings/accent", url.Values{"csrf": {csrf}, "accent": {"red"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save = %d", resp.StatusCode)
	}
	for _, p := range []string{"/today", "/settings/appearance"} {
		if _, body := e.get(t, c2, p); strings.Contains(body, `<html lang="en" data-accent`) {
			t.Errorf("alice's accent leaked to bob on %s", p)
		}
	}
	if _, body := e.get(t, c2, "/settings/appearance"); !strings.Contains(body, `name="accent" value="jade" checked`) {
		t.Error("bob's Appearance should show jade chosen")
	}
	if _, body := e.get(t, e.client(), "/login"); strings.Contains(body, "data-accent") {
		t.Error("login should always be jade")
	}
	var n int
	if err := e.st.DB().QueryRow(`SELECT COUNT(*) FROM user_settings s JOIN users u ON u.id = s.user_id WHERE u.username = 'bob' AND s.key = 'accent'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("bob has %d accent rows (%v)", n, err)
	}
}

func TestAccentRejectsUnknown(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	if resp := e.post(t, c, "/settings/accent", url.Values{"csrf": {csrf}, "accent": {"red"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save = %d", resp.StatusCode)
	}
	for _, v := range []url.Values{
		{"csrf": {csrf}, "accent": {"neon"}}, {"csrf": {csrf}, "accent": {""}}, {"csrf": {csrf}, "accent": {"Red"}},
		{"csrf": {csrf}, "accent": {"slate"}}, {"csrf": {csrf}, "accent": {`red"x`}}, {"csrf": {csrf}},
	} {
		resp := e.post(t, c, "/settings/accent", v)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("bad value %v = %d", v, resp.StatusCode)
		}
		body := e.body(t, resp)
		for _, want := range []string{"accent is not one of the offered colors.", `<h1>Appearance</h1>`, `data-accent="red"`} {
			if !strings.Contains(body, want) {
				t.Errorf("%v: 422 page lacks %q", v, want)
			}
		}
	}
	if _, body := e.get(t, c, "/settings/appearance"); !strings.Contains(body, `name="accent" value="red" checked`) {
		t.Error("a rejected value changed the saved accent")
	}
}

func TestAccentListMatchesCSS(t *testing.T) {
	b, err := web.FS.ReadFile("static/accents.css")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, m := range regexp.MustCompile(`\[data-accent="([a-z]+)"\]\s*\{`).FindAllStringSubmatch(string(b), -1) {
		got[m[1]] = true
	}
	if len(triage.Accents) != 16 || len(got) != len(triage.Accents) {
		t.Fatalf("%d accents in Go, %d in CSS", len(triage.Accents), len(got))
	}
	for _, a := range triage.Accents {
		if !got[a] {
			t.Errorf("accents.css has no block for %s", a)
		}
	}
}

func TestPlainHTTPToPublicHost(t *testing.T) {
	for u, want := range map[string]bool{
		"http://mf.example.com": true, "http://203.0.113.5:8080": true, "http://[2001:db8::1]/": true,
		"https://mf.example.com": false, "http://localhost": false, "http://foo.localhost": false,
		"http://127.0.0.1:8080": false, "http://10.0.0.2": false, "http://172.16.5.4": false,
		"http://192.168.1.20:8080": false, "http://169.254.1.1": false, "http://100.64.0.1": false,
		"http://[fd00::1]": false, "http://[::1]": false, "http://[fe80::1]": false,
		"http://miniflux:8080": false, "http://nas.local": false, "http://nas.lan": false,
		"http://nas.home.arpa": false, "http://svc.internal": false, "not a url": false,
	} {
		if got := plainHTTPToPublicHost(u); got != want {
			t.Errorf("%q = %v, want %v", u, got, want)
		}
	}
}

func TestDestinationNoPlainHTTPHintForPrivateOrHTTPS(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	for _, base := range []string{"https://mf.example.com", "http://192.168.1.20:8080", "http://miniflux:8080", "http://localhost:8080", "http://100.101.102.103"} {
		resp := e.post(t, c, "/settings/destinations/miniflux", url.Values{"csrf": {csrf}, "base_url": {base}, "secret": {"tok"}})
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("%s: save = %d", base, resp.StatusCode)
		}
		if _, body := e.get(t, c, "/settings/destinations"); strings.Contains(body, "plain-http-hint") {
			t.Errorf("%s: hint shown", base)
		}
	}
}

func TestDestinationPlainHTTPPublicHostShowsHint(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	resp := e.post(t, c, "/settings/destinations/miniflux", url.Values{"csrf": {csrf}, "base_url": {"http://mf.example.com"}, "secret": {"tok-SECRET"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save = %d", resp.StatusCode)
	}
	_, body := e.get(t, c, "/settings/destinations")
	for _, want := range []string{"plain-http-hint", `aria-describedby="plain-http-miniflux"`, "API token"} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	if strings.Contains(body, "tok-SECRET") {
		t.Error("secret echoed")
	}
}
