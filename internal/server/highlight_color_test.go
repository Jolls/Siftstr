package server

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pickEnv has alice signed in with a Karakeep connection and a yellow pick (h1)
// for today. It returns the day /read calls today.
func pickEnv(t *testing.T) (e *env, rec *kickRec, c *http.Client, csrf, day string) {
	t.Helper()
	e, rec = kickEnv(t, nil)
	c, csrf = e.signedIn(t, "alice")
	_, page := e.get(t, c, "/read")
	m := readDayRe.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no day on /read:\n%s", page)
	}
	day = m[1]
	aid := e.userID(t, "alice")
	e.addConn(t, aid, "karakeep")
	e.pick(t, aid, day, "h1", "Marked <b>text</b>", "yellow")
	return
}

func (e *env) pick(t *testing.T, uid, day, id, text, color string) {
	t.Helper()
	e.exec(t, `INSERT INTO highlight_picks (user_id, day, highlight_id, text, note, color, bookmark_title, bookmark_url, byline, created_at)
		VALUES (?, ?, ?, ?, 'Because it matters', ?, 'The Page', 'https://example.com/p', 'example.com', 'c')`, uid, day, id, text, color)
}

func (e *env) pickColor(t *testing.T, uid, id string) (color string) {
	t.Helper()
	if err := e.st.DB().QueryRow(`SELECT color FROM highlight_picks WHERE user_id = ? AND highlight_id = ?`, uid, id).Scan(&color); err != nil {
		t.Fatal(err)
	}
	return
}

func (e *env) outboxRows(t *testing.T, uid string) (n int) {
	t.Helper()
	if err := e.st.DB().QueryRow(`SELECT COUNT(*) FROM outbox WHERE user_id = ?`, uid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return
}

func colorForm(csrf, id, color string) url.Values {
	return url.Values{"csrf": {csrf}, "highlight_id": {id}, "color": {color}}
}

// aside returns the Today page's highlight block.
func aside(t *testing.T, page string) string {
	t.Helper()
	i := strings.Index(page, "<aside")
	if i < 0 {
		t.Fatalf("no highlight on the page:\n%s", page)
	}
	return page[i : i+strings.Index(page[i:], "</aside>")]
}

// --- characterization: what the highlight looks like before the change ---

func TestCharTodayHighlightAsideIsNotAnItem(t *testing.T) {
	e, _, c, _, _ := pickEnv(t)
	_, page := e.get(t, c, "/today")
	a := aside(t, page)
	for _, want := range []string{"Marked &lt;b&gt;text&lt;/b&gt;", "Note: Because it matters"} {
		if !strings.Contains(a, want) {
			t.Errorf("the highlight lacks %q:\n%s", want, a)
		}
	}
	if strings.Contains(a, "data-item-id") || strings.Contains(a, "data-action") {
		t.Errorf("the highlight must not be an item:\n%s", a)
	}
	if i := strings.Index(page, `<section class="items"`); i >= 0 && i < strings.Index(page, "<aside") {
		t.Error("the highlight must come before the cards")
	}
	bc, _ := e.signedIn(t, "bob")
	if _, bobPage := e.get(t, bc, "/today"); strings.Contains(bobPage, "Marked") {
		t.Error("another user's highlight on Today")
	}
}

func TestCharUnknownAndEmptyColorRenderWithoutError(t *testing.T) {
	e, _, c, _, _ := pickEnv(t)
	aid := e.userID(t, "alice")
	for _, color := range []string{"", "purple"} {
		e.exec(t, `UPDATE highlight_picks SET color = ? WHERE user_id = ?`, color, aid)
		for _, path := range []string{"/today", "/read"} {
			if code, page := e.get(t, c, path); code != 200 || !strings.Contains(page, "Marked") {
				t.Errorf("color %q on %s: %d", color, path, code)
			}
		}
	}
}

// --- the color change ---

func TestHighlightColorNeedsSessionAndCSRF(t *testing.T) {
	e, rec, c, csrf, _ := pickEnv(t)
	aid := e.userID(t, "alice")
	anon := e.client()
	if resp := e.post(t, anon, "/highlight/color", colorForm("x", "h1", "red")); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Errorf("anonymous POST = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp := e.post(t, c, "/highlight/color", url.Values{"highlight_id": {"h1"}, "color": {"red"}}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("no CSRF token = %d, want 403", resp.StatusCode)
	}
	big := colorForm(csrf, "h1", "red")
	big.Set("pad", strings.Repeat("x", 2<<20))
	if resp := e.post(t, c, "/highlight/color", big); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body = %d, want 413", resp.StatusCode)
	}
	if e.pickColor(t, aid, "h1") != "yellow" || e.outboxRows(t, aid) != 0 || len(rec.take()) != 0 {
		t.Error("a refused request changed something")
	}
}

func TestHighlightColorChangesTodaysPickAndSendsIt(t *testing.T) {
	e, rec, c, csrf, _ := pickEnv(t)
	aid := e.userID(t, "alice")
	resp := e.post(t, c, "/highlight/color", colorForm(csrf, "h1", "red"))
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/today" {
		t.Fatalf("POST = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if e.pickColor(t, aid, "h1") != "red" || e.outboxRows(t, aid) != 1 {
		t.Errorf("color = %q, outbox rows = %d", e.pickColor(t, aid, "h1"), e.outboxRows(t, aid))
	}
	wantKick(t, "color change", rec, resp, "", kickCall{User: aid, Jobs: []string{"outbox"}})
	_, page := e.get(t, c, "/today")
	a := aside(t, page)
	if !strings.Contains(a, `data-color="red"`) || !strings.Contains(a, `<span class="highlight-color-name">Red</span>`) {
		t.Errorf("Today does not show the new color:\n%s", a)
	}
}

func TestHighlightColorRewritesTodaysBriefingFile(t *testing.T) {
	e, _, c, csrf, day := pickEnv(t)
	e.seedBriefing(t, "alice", day, "old file\n")
	if resp := e.post(t, c, "/highlight/color", colorForm(csrf, "h1", "red")); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST = %d", resp.StatusCode)
	}
	got, err := os.ReadFile(filepath.Join(e.briefDir, "alice", day+".md"))
	if err != nil || !strings.Contains(string(got), "Color: Red") {
		t.Errorf("file = %v\n%s", err, got)
	}
}

func TestHighlightColorDoesNotCreateABriefingFile(t *testing.T) {
	e, _, c, csrf, day := pickEnv(t)
	if resp := e.post(t, c, "/highlight/color", colorForm(csrf, "h1", "red")); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST = %d", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(e.briefDir, "alice", day+".md")); err == nil {
		t.Error("changing a color created a briefing file")
	}
}

func TestHighlightColorRejectsUnknownColorAndStalePick(t *testing.T) {
	e, rec, c, csrf, _ := pickEnv(t)
	aid := e.userID(t, "alice")
	if resp := e.post(t, c, "/highlight/color", colorForm(csrf, "h1", "purple")); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("unknown color = %d, want 422", resp.StatusCode)
	}
	if resp := e.post(t, c, "/highlight/color", colorForm(csrf, "other", "red")); resp.StatusCode != http.StatusNotFound {
		t.Errorf("stale pick = %d, want 404", resp.StatusCode)
	}
	if e.pickColor(t, aid, "h1") != "yellow" || e.outboxRows(t, aid) != 0 || len(rec.take()) != 0 {
		t.Error("a rejected change did something")
	}
}

func TestHighlightColorTwoUsersCannotChangeEachOthersPick(t *testing.T) {
	e, rec, ac, acsrf, day := pickEnv(t)
	aid := e.userID(t, "alice")
	bc, bcsrf := e.signedIn(t, "bob")
	bid := e.userID(t, "bob")
	e.addConn(t, bid, "karakeep")
	e.pick(t, bid, day, "h2", "bob text", "blue")

	if resp := e.post(t, ac, "/highlight/color", colorForm(acsrf, "h2", "red")); resp.StatusCode != http.StatusNotFound {
		t.Errorf("alice reaching bob's pick = %d, want 404", resp.StatusCode)
	}
	if resp := e.post(t, bc, "/highlight/color", colorForm(bcsrf, "nope", "red")); resp.StatusCode != http.StatusNotFound {
		t.Errorf("bob with an unknown pick = %d, want 404", resp.StatusCode)
	}
	if e.pickColor(t, bid, "h2") != "blue" || e.outboxRows(t, bid) != 0 || len(rec.take()) != 0 {
		t.Error("alice changed bob's pick")
	}

	// The same highlight ID belongs to each user separately: bob's pick gets alice's ID.
	e.exec(t, `UPDATE highlight_picks SET highlight_id = 'h1' WHERE user_id = ?`, bid)
	if resp := e.post(t, bc, "/highlight/color", colorForm(bcsrf, "h1", "red")); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("bob's own change = %d", resp.StatusCode)
	}
	if e.pickColor(t, aid, "h1") != "yellow" || e.outboxRows(t, aid) != 0 {
		t.Error("bob changed alice's pick")
	}
	var conn string
	_ = e.st.DB().QueryRow(`SELECT connection_id FROM outbox WHERE user_id = ?`, bid).Scan(&conn)
	if conn != "con_karakeep_"+bid {
		t.Errorf("bob's entry uses connection %q, want his own", conn)
	}
}

// --- what the pages show ---

func TestTodayShowsTheColorBorderLabelAndControl(t *testing.T) {
	e, _, c, csrf, _ := pickEnv(t)
	e.exec(t, `UPDATE highlight_picks SET color = 'green' WHERE user_id = ?`, e.userID(t, "alice"))
	_, page := e.get(t, c, "/today")
	a := aside(t, page)
	for _, want := range []string{
		`data-color="green"`, `<span class="highlight-color-name">Green</span>`,
		`action="/highlight/color"`, `name="csrf" value="` + csrf + `"`, `name="highlight_id" value="h1"`,
	} {
		if !strings.Contains(a, want) {
			t.Errorf("the highlight lacks %q:\n%s", want, a)
		}
	}
	last := -1
	for _, o := range []string{`value="yellow"`, `value="red"`, `value="green"`, `value="blue"`} {
		i := strings.Index(a, o)
		if i <= last {
			t.Errorf("buttons out of order or missing at %s:\n%s", o, a)
		}
		last = i
	}
	if strings.Count(a, `aria-pressed="true"`) != 1 || !strings.Contains(a, `value="green" class="outline secondary" aria-pressed="true"`) {
		t.Errorf("only Green should be pressed:\n%s", a)
	}
	if strings.Contains(a, "data-item-id") || strings.Contains(a, "data-action") {
		t.Errorf("the highlight must stay out of the item controls:\n%s", a)
	}
	if i := strings.Index(page, `<section class="items"`); i >= 0 && strings.Index(page, "</aside>") > i {
		t.Error("the highlight must sit outside the item list")
	}
}

func TestReadShowsOnlyTheColorBorder(t *testing.T) {
	e, _, c, _, _ := pickEnv(t)
	e.exec(t, `UPDATE highlight_picks SET color = 'green' WHERE user_id = ?`, e.userID(t, "alice"))
	_, page := e.get(t, c, "/read")
	i := strings.Index(page, `class="read-entry read-highlight"`)
	if i < 0 {
		t.Fatalf("no highlight on /read:\n%s", page)
	}
	art := page[i : i+strings.Index(page[i:], "</article>")]
	if !strings.Contains(page[i:i+80], `data-color="green"`) {
		t.Errorf("the highlight lacks its color:\n%s", art)
	}
	for _, banned := range []string{"highlight-color-name", "Color:", "Green"} {
		if strings.Contains(art, banned) {
			t.Errorf("/read names the color (%q) in the reading flow:\n%s", banned, art)
		}
	}
	main := page[strings.Index(page, "<main"):strings.Index(page, "</main>")]
	for _, banned := range []string{"<button", "<form", "data-action"} {
		if strings.Contains(main, banned) {
			t.Errorf("/read contains %q in the reading flow", banned)
		}
	}
}

func TestUnknownOrEmptyColorShowsNoMarkButKeepsTheControl(t *testing.T) {
	e, _, c, _, _ := pickEnv(t)
	aid := e.userID(t, "alice")
	for _, color := range []string{"", "purple"} {
		e.exec(t, `UPDATE highlight_picks SET color = ? WHERE user_id = ?`, color, aid)
		for _, path := range []string{"/today", "/read"} {
			code, page := e.get(t, c, path)
			if code != 200 || strings.Contains(page, "data-color") || strings.Contains(page, "highlight-color-name") || strings.Contains(page, `aria-pressed="true"`) {
				t.Errorf("color %q on %s: %d, and it shows a mark", color, path, code)
			}
		}
		_, page := e.get(t, c, "/today")
		if strings.Count(aside(t, page), `name="color"`) != 4 {
			t.Errorf("color %q: Today should still offer the four buttons", color)
		}
	}
}
