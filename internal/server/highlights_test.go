package server

import (
	"net/url"
	"regexp"
	"strings"
	"testing"
)

var readDayRe = regexp.MustCompile(`Read, (\d{4}-\d{2}-\d{2})`)

func TestHighlightSettingOptInAndKeepWhenOmitted(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	_, body := e.get(t, c, "/settings/sources")
	if !strings.Contains(body, `name="karakeep_highlights"`) || strings.Contains(body, `name="karakeep_highlights" value="1" checked`) {
		t.Fatal("the highlights box must be on the page and off by default")
	}
	base := url.Values{"csrf": {csrf}, "max_age_days": {"30"}, "excerpt_length": {"300"}}

	on := url.Values{"highlights_form": {"1"}, "karakeep_highlights": {"1"}}
	for k, v := range base {
		on[k] = v
	}
	if resp := e.post(t, c, "/settings/ingest", on); resp.StatusCode != 303 {
		t.Fatalf("save on = %d", resp.StatusCode)
	}
	_, body = e.get(t, c, "/settings/sources")
	if !strings.Contains(body, `name="karakeep_highlights" value="1" checked`) {
		t.Fatal("the saved opt-in is not shown")
	}

	// A post that does not carry the form marker leaves the choice alone.
	if resp := e.post(t, c, "/settings/ingest", base); resp.StatusCode != 303 {
		t.Fatalf("save without the marker = %d", resp.StatusCode)
	}
	_, body = e.get(t, c, "/settings/sources")
	if !strings.Contains(body, `name="karakeep_highlights" value="1" checked`) {
		t.Fatal("omitting the field must keep the saved choice")
	}

	// The marker with the box unchecked turns it off.
	off := url.Values{"highlights_form": {"1"}}
	for k, v := range base {
		off[k] = v
	}
	if resp := e.post(t, c, "/settings/ingest", off); resp.StatusCode != 303 {
		t.Fatalf("save off = %d", resp.StatusCode)
	}
	_, body = e.get(t, c, "/settings/sources")
	if strings.Contains(body, `name="karakeep_highlights" value="1" checked`) {
		t.Fatal("unchecking the box did not turn highlights off")
	}

	// Another user's choice is theirs.
	bc, _ := e.signedIn(t, "bob")
	_, body = e.get(t, bc, "/settings/sources")
	if strings.Contains(body, `name="karakeep_highlights" value="1" checked`) {
		t.Error("a user inherited another user's opt-in")
	}
}

func TestReadShowsTheHighlightFirstAndOnlyToItsOwner(t *testing.T) {
	e := setup(t, nil, false)
	c, _ := e.signedIn(t, "alice")
	_, page := e.get(t, c, "/read")
	m := readDayRe.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no day on /read:\n%s", page)
	}
	day := m[1]
	if strings.Contains(page, "read-highlight") {
		t.Fatal("no highlight is stored yet, but /read shows one")
	}

	uid := e.userID(t, "alice")
	e.exec(t, `INSERT INTO highlight_picks (user_id, day, highlight_id, text, note, color, bookmark_title, bookmark_url, byline, created_at)
		VALUES (?, ?, 'h1', 'Marked <b>text</b>', 'Because it matters', 'yellow', 'The Page', 'https://example.com/p', 'example.com', 'c')`, uid, day)
	_, page = e.get(t, c, "/read")
	today := page[strings.Index(page, `id="read-today"`):strings.Index(page, `id="read-backlog"`)]
	first := strings.Index(today, "read-highlight")
	if first < 0 {
		t.Fatalf("/read lacks the highlight:\n%s", today)
	}
	for _, want := range []string{"Marked &lt;b&gt;text&lt;/b&gt;", "Note: Because it matters", `href="https://example.com/p"`, "The Page"} {
		if !strings.Contains(today, want) {
			t.Errorf("/read lacks %q", want)
		}
	}
	if strings.Contains(today, "<b>text</b>") {
		t.Error("highlight text is not escaped")
	}
	main := page[strings.Index(page, "<main"):strings.Index(page, "</main>")]
	for _, banned := range []string{"<button", "<form", "data-action"} {
		if strings.Contains(main, banned) {
			t.Errorf("/read contains %q in the reading flow", banned)
		}
	}

	bc, _ := e.signedIn(t, "bob")
	_, bobPage := e.get(t, bc, "/read")
	if strings.Contains(bobPage, "Marked") {
		t.Error("another user's highlight on /read")
	}
}
