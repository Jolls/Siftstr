package server

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const kkBase = "https://kk.example.com"

// linkEnv has alice and bob, each with a Karakeep connection and a light item
// in today's list: itm_a/bm_a has a bookmark, itm_b has none, itm_bob/bm_bob is
// bob's. itm_old is an earlier one of alice's (the backlog). It returns the
// day /read calls today.
func linkEnv(t *testing.T) (e *env, day string) {
	t.Helper()
	e = setup(t, nil, false)
	c, _ := e.signedIn(t, "alice")
	e.signedIn(t, "bob")
	_, page := e.get(t, c, "/read")
	m := readDayRe.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no day on /read:\n%s", page)
	}
	day = m[1]
	aid, bid := e.userID(t, "alice"), e.userID(t, "bob")
	e.exec(t, `INSERT INTO connections (id, user_id, kind, base_url) VALUES ('ck1', ?, 'karakeep', ?), ('ck2', ?, 'karakeep', 'https://kk2.example.com')`, aid, kkBase+"/", bid)
	e.seedLight(t, "alice", "itm_a")
	e.seedLight(t, "alice", "itm_b")
	e.seedLight(t, "alice", "itm_old")
	e.seedLight(t, "bob", "itm_bob")
	e.exec(t, `UPDATE items SET batch_date = ?, url = 'https://example.com/' || id WHERE id IN ('itm_a', 'itm_b', 'itm_bob')`, day)
	e.exec(t, `UPDATE items SET batch_date = '2000-01-01', url = 'https://example.com/itm_old' WHERE id = 'itm_old'`)
	e.exec(t, `UPDATE items SET karakeep_id = 'bm_a' WHERE id = 'itm_a'`)
	e.exec(t, `UPDATE items SET karakeep_id = 'bm_old' WHERE id = 'itm_old'`)
	e.exec(t, `UPDATE items SET karakeep_id = 'bm_bob' WHERE id = 'itm_bob'`)
	return e, day
}

// card returns the markup of the <article> with the given id.
func card(t *testing.T, page, id string) string {
	t.Helper()
	i := strings.Index(page, `<article class="item" id="`+id+`"`)
	if i < 0 {
		t.Fatalf("no card %s:\n%s", id, page)
	}
	return page[i : i+strings.Index(page[i:], "</article>")]
}

func TestTodayAndBacklogCardsLinkToKarakeepWithOriginal(t *testing.T) {
	e, _ := linkEnv(t)
	ac := e.client()
	if resp := e.login(t, ac, "alice", "correct horse battery"); resp.StatusCode != 303 {
		t.Fatal(resp.StatusCode)
	}
	for _, tc := range []struct{ path, id, kk, orig string }{
		{"/today", "itm_a", kkBase + "/dashboard/preview/bm_a", "https://example.com/itm_a"},
		{"/backlog", "itm_old", kkBase + "/dashboard/preview/bm_old", "https://example.com/itm_old"},
	} {
		_, page := e.get(t, ac, tc.path)
		cd := card(t, page, tc.id)
		if !strings.Contains(cd, `<h2><a href="`+tc.kk+`"`) {
			t.Errorf("%s: the title does not link to Karakeep:\n%s", tc.path, cd)
		}
		if !strings.Contains(cd, `class="original" href="`+tc.orig+`"`) || !strings.Contains(cd, ">Original</a>") {
			t.Errorf("%s: no Original link:\n%s", tc.path, cd)
		}
		if !strings.Contains(cd, `data-action="archive"`) {
			t.Errorf("%s: the card lost its controls", tc.path)
		}
	}

	_, page := e.get(t, ac, "/today")
	cd := card(t, page, "itm_b")
	if !strings.Contains(cd, `<h2><a href="https://example.com/itm_b"`) || strings.Contains(cd, "Original") || strings.Contains(cd, "dashboard/preview") {
		t.Errorf("an item without a bookmark must link to its original only:\n%s", cd)
	}
	if strings.Contains(page, "bm_bob") || strings.Contains(page, "kk2.example.com") {
		t.Error("another user's bookmark on alice's Today")
	}
}

func TestActionedCardKeepsKarakeepLinkInDOM(t *testing.T) {
	e, _ := linkEnv(t)
	ac := e.client()
	if resp := e.login(t, ac, "alice", "correct horse battery"); resp.StatusCode != 303 {
		t.Fatal(resp.StatusCode)
	}
	_, page := e.get(t, ac, "/today")
	m := csrfRe.FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no csrf token on /today")
	}
	body := fmt.Sprintf(`{"actions":[{"action_id":"a1","subject_type":"item","subject_id":"itm_a","kind":"archive","at":%q}]}`, time.Now().UTC().Format(time.RFC3339))
	if code, out := e.sync(t, ac, m[1], body); code != 200 {
		t.Fatalf("sync = %d %s", code, out)
	}
	_, page = e.get(t, ac, "/today")
	cd := card(t, page, "itm_a")
	if !strings.Contains(cd, kkBase+"/dashboard/preview/bm_a") || !strings.Contains(cd, ">Original</a>") {
		t.Errorf("the actioned card lost its links:\n%s", cd)
	}
}

func TestReadLinksToKarakeepWithOriginalAndKeepsControlsOut(t *testing.T) {
	e, _ := linkEnv(t)
	ac := e.client()
	if resp := e.login(t, ac, "alice", "correct horse battery"); resp.StatusCode != 303 {
		t.Fatal(resp.StatusCode)
	}
	_, page := e.get(t, ac, "/read")
	main := page[strings.Index(page, "<main"):strings.Index(page, "</main>")]
	if !strings.Contains(main, `<h3><a href="`+kkBase+`/dashboard/preview/bm_a"`) {
		t.Errorf("/read: the title does not link to Karakeep:\n%s", main)
	}
	if !strings.Contains(main, `<a href="https://example.com/itm_a" rel="noopener noreferrer">Original</a>`) {
		t.Errorf("/read: no Original link:\n%s", main)
	}
	if !strings.Contains(main, `<h3><a href="https://example.com/itm_b"`) {
		t.Errorf("/read: an item without a bookmark must link to its original:\n%s", main)
	}
	for _, banned := range []string{"<button", "<form", "data-action"} {
		if strings.Contains(main, banned) {
			t.Errorf("/read contains %q in the reading flow", banned)
		}
	}
	if strings.Contains(main, "bm_bob") {
		t.Error("another user's bookmark on /read")
	}
}

func TestHighlightKarakeepPrimaryOnReadAndToday(t *testing.T) {
	e, day := linkEnv(t)
	aid := e.userID(t, "alice")
	e.exec(t, `INSERT INTO highlight_picks (user_id, day, highlight_id, text, note, color, bookmark_title, bookmark_url, karakeep_url, byline, created_at)
		VALUES (?, ?, 'h1', 'Marked', '', 'yellow', 'The Page', 'https://example.com/p', ?, 'example.com', 'c')`, aid, day, kkBase+"/dashboard/preview/b1")
	ac := e.client()
	if resp := e.login(t, ac, "alice", "correct horse battery"); resp.StatusCode != 303 {
		t.Fatal(resp.StatusCode)
	}
	for _, path := range []string{"/today", "/read"} {
		_, page := e.get(t, ac, path)
		from := page[strings.Index(page, "From "):]
		from = from[:strings.Index(from, "</p>")]
		if !strings.Contains(from, `<a href="`+kkBase+`/dashboard/preview/b1" rel="noopener noreferrer">The Page</a>`) {
			t.Errorf("%s: From does not link to Karakeep:\n%s", path, from)
		}
		if strings.Count(from, "https://example.com/p") != 1 || !strings.Contains(from, ">Original</a>") || strings.Contains(from, ">Karakeep</a>") {
			t.Errorf("%s: want exactly one Original link and no Karakeep link:\n%s", path, from)
		}
	}
}
