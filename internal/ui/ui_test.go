package ui

import (
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/Jolls/Siftstr/internal/briefing"
)

func render(t *testing.T, name string, p Page) string {
	t.Helper()
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	r.Render(rec, http.StatusOK, name, p)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	return rec.Body.String()
}

func TestLoginPage(t *testing.T) {
	body := render(t, "login", Page{Title: "Sign in", CSRF: "tok123", Error: "bad login"})
	for _, want := range []string{`name="csrf" value="tok123"`, `autocomplete="current-password"`, "bad login"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, `href="/today"`) {
		t.Error("logged-out page must not show the nav")
	}
}

func TestItemEscaping(t *testing.T) {
	body := render(t, "items", Page{
		User: &User{Name: "a"}, Active: "today", Heading: "Today",
		Items: []Item{{ID: "itm_1", Title: `<script>alert(1)</script>`, URL: "https://example.com/x", State: "light"}},
	})
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("title not escaped")
	}
}

// Actioned items stay in the DOM with a badge; the badge slot always exists.
func TestItemBadgeAndControls(t *testing.T) {
	body := render(t, "items", Page{
		User: &User{Name: "a"}, Active: "today", Heading: "Today",
		Items: []Item{
			{ID: "itm_1", Title: "A", URL: "https://example.com/a", State: "archived", Badge: "archived", CanPromote: true},
			{ID: "itm_2", Title: "B", URL: "https://example.com/b", State: "light"},
		},
	})
	for _, want := range []string{
		`id="itm_1"`, `data-state="archived"`, `>archived</span>`,
		`id="itm_2"`, `data-badge="" hidden`, `data-action="archive"`, `data-action="keep"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Count(body, `data-action="promote"`) != 1 {
		t.Error("promote button should appear only where CanPromote is set")
	}
	if i := strings.Index(body, `<section class="items"`); i < 0 || strings.Contains(body[i:], "<form") {
		t.Error("triage controls must not be forms or direct requests")
	}
}

// The Pico build is named in exactly one place: the Pico stylesheet link. Accents come from accents.css.
func TestBaseLinksStylesheets(t *testing.T) {
	body := render(t, "items", Page{User: &User{}})
	if !strings.Contains(body, `href="/static/app.css"`) {
		t.Error("app.css link missing")
	}
	if n := strings.Count(body, `href="/static/pico.`); n != 1 {
		t.Errorf("want exactly one Pico stylesheet link, got %d", n)
	}
}

func TestEmptyList(t *testing.T) {
	body := render(t, "items", Page{User: &User{}, Heading: "Backlog", Empty: "Nothing carried over."})
	if !strings.Contains(body, "Nothing carried over.") {
		t.Error("empty message missing")
	}
}

func TestStatic(t *testing.T) {
	h, err := Static()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/static/pico.jade.min.css", "/static/accents.css", "/static/app.css"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s = %d", p, rec.Code)
		}
	}
}

// A card shows its excerpt until it has a summary, and the swipe-left button
// reads Delete, or Archive on a promoted card (the page picks by data-promoted).
func TestItemExcerptAndDeleteLabel(t *testing.T) {
	body := render(t, "items", Page{
		User: &User{Name: "a"}, Active: "today", Heading: "Today",
		Items: []Item{
			{ID: "itm_1", Title: "A", State: "light", Excerpt: "only the excerpt"},
			{ID: "itm_2", Title: "B", State: "deep", Excerpt: "hidden excerpt", Summary: "the summary", Promoted: true},
		},
	})
	for _, want := range []string{
		`<p class="excerpt">only the excerpt</p>`, `<p class="summary">the summary</p>`,
		`data-promoted="false"`, `data-promoted="true"`,
		`<span class="label-delete">Delete</span><span class="label-archive">Archive</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, "hidden excerpt") {
		t.Error("the excerpt should give way to the summary")
	}
}

// A triaged card is collapsed by CSS, so everything it hides must still be in
// the markup: the summary or excerpt, the badge and the buttons. Undo shows
// whenever there is something to undo, and Promote hides itself outside "light".
func TestActionedCardStaysInDOM(t *testing.T) {
	body := render(t, "items", Page{
		User: &User{Name: "a"}, Active: "today", Heading: "Today",
		Items: []Item{
			{ID: "itm_1", Title: "A", State: "archived", Badge: "archived", UndoID: "act_1", Excerpt: "excerpt one"},
			{ID: "itm_2", Title: "B", State: "kept", Badge: "kept", UndoID: "act_2", Summary: "summary two"},
			{ID: "itm_3", Title: "C", State: "pending_deep", Badge: "promoted", UndoID: "act_3", Excerpt: "excerpt three", CanPromote: true},
		},
	})
	for _, want := range []string{
		`<p class="excerpt">excerpt one</p>`, `<p class="summary">summary two</p>`, `<p class="excerpt">excerpt three</p>`,
		`>archived</span>`, `>kept</span>`, `>promoted</span>`,
		`data-undo-action="act_1"`, `data-undo-action="act_2"`, `data-undo-action="act_3"`,
		`data-action="archive"`, `data-action="keep"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Count(body, `data-action="undo" hidden`) != 0 {
		t.Error("Undo must not be hidden while there is an action to undo")
	}
	if !strings.Contains(body, `data-action="promote" hidden`) {
		t.Error("Promote must be hidden outside the light state")
	}
}

// /read is a linear text page: none of the card markup or its state belongs there.
func TestReadPageHasNoCardMarkup(t *testing.T) {
	body := render(t, "read", Page{
		User: &User{Name: "a"}, Active: "read",
		Today:   []briefing.Entry{{Title: "T", URL: "https://example.com/t", Source: "S", Excerpt: "x", Summary: "y"}},
		Backlog: []briefing.Entry{{Title: "B", Source: "S", Excerpt: "z"}},
	})
	for _, bad := range []string{`data-state`, `data-action`, `data-badge`, `class="item"`} {
		if strings.Contains(body, bad) {
			t.Errorf("/read must not contain %q", bad)
		}
	}
	if !strings.Contains(body, `class="read-entry"`) {
		t.Error("/read entries missing")
	}
}

func appCSS(t *testing.T) string {
	t.Helper()
	h, err := Static()
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/app.css", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("app.css = %d", rec.Code)
	}
	return rec.Body.String()
}

// cssBlock returns the declarations of the first rule whose selector list
// contains sel, or "" when there is none.
func cssBlock(css, sel string) string {
	i := strings.Index(css, sel)
	if i < 0 {
		return ""
	}
	open := strings.Index(css[i:], "{")
	if open < 0 {
		return ""
	}
	rest := css[i+open+1:]
	if end := strings.Index(rest, "}"); end >= 0 {
		return rest[:end]
	}
	return ""
}

var triagedStates = []string{"archived", "kept", "pending_deep"}

func TestCollapseHidesSummaryAndExcerpt(t *testing.T) {
	css := appCSS(t)
	for _, s := range triagedStates {
		for _, part := range []string{".summary", ".excerpt"} {
			sel := `.item[data-state="` + s + `"] ` + part
			if !strings.Contains(cssBlock(css, sel), "display: none") {
				t.Errorf("%s should be display: none", sel)
			}
		}
		if !strings.Contains(cssBlock(css, `.item[data-state="`+s+`"]`), "opacity: 0.6") && !strings.Contains(css, "opacity: 0.6") {
			t.Errorf("triaged cards should stay dimmed (%s)", s)
		}
	}
}

func TestActionedHidesAllButUndo(t *testing.T) {
	css := appCSS(t)
	for _, s := range triagedStates {
		sel := `.item[data-state="` + s + `"] [data-action]:not([data-action="undo"])`
		if !strings.Contains(cssBlock(css, sel), "display: none") {
			t.Errorf("%s should be display: none", sel)
		}
		// Undo's visibility is the hidden attribute's job; a display rule would override it.
		if strings.Contains(cssBlock(css, `.item[data-state="`+s+`"] [data-action="undo"]`), "display") {
			t.Errorf("Undo must not get a display rule in %s", s)
		}
	}
}

func TestPerStateChosenButtonRulesRemoved(t *testing.T) {
	css := appCSS(t)
	for _, gone := range []string{
		"only the chosen button stays",
		"justify-content: flex-start",
		"justify-content: flex-end",
		`[data-state="pending_deep"] [data-action="promote"] { display: inline-block`,
		`[data-state="pending_deep"] [data-action="undo"] { order: -1`,
	} {
		if strings.Contains(css, gone) {
			t.Errorf("app.css still has %q", gone)
		}
	}
}

func TestControlsBadgeLeftUndoRight(t *testing.T) {
	css := appCSS(t)
	for _, s := range triagedStates {
		if !strings.Contains(cssBlock(css, `.item[data-state="`+s+`"] .controls`), "justify-content: space-between") {
			t.Errorf("%s controls should be space-between", s)
		}
		if !strings.Contains(cssBlock(css, `.item[data-state="`+s+`"] [data-action="undo"]`), "margin-inline-start: auto") {
			t.Errorf("%s Undo should sit at the end of the row", s)
		}
	}
}

// Collapsing is instant: no motion for the viewer to opt out of.
func TestNoCollapseAnimation(t *testing.T) {
	css := appCSS(t)
	for _, motion := range []string{"transition", "animation"} {
		if strings.Contains(css, motion) {
			t.Errorf("app.css should not use %s", motion)
		}
	}
}

func TestBaseFollowsOSByDefault(t *testing.T) {
	for name, p := range map[string]Page{
		"login":            {Title: "Sign in"},
		"items":            {User: &User{}, Heading: "Today"},
		"read":             {User: &User{}},
		"settings_sources": {User: &User{}, Title: "Sources"},
	} {
		body := render(t, name, p)
		if !strings.Contains(body, `<meta name="color-scheme" content="light dark">`) || !strings.Contains(body, `<html lang="en">`) {
			t.Errorf("%s: not following the OS", name)
		}
		if strings.Contains(body, "data-theme") {
			t.Errorf("%s: has data-theme with no theme chosen", name)
		}
	}
}

// The status badge colors (#31) are fixed hex values chosen for AA contrast; they live only in --badge-* variables.
func TestAppCSSUsesOnlyPicoColors(t *testing.T) {
	css := regexp.MustCompile(`--badge-[a-z]+-(bg|fg):\s*#[0-9a-fA-F]{6};`).ReplaceAllString(appCSS(t), "")
	if m := regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|rgb\(|hsl\(`).FindString(css); m != "" {
		t.Errorf("app.css has a literal color %q", m)
	}
}

func TestBaseRendersChosenTheme(t *testing.T) {
	for theme, want := range map[string][2]string{
		"dark":  {`<html lang="en" data-theme="dark">`, `<meta name="color-scheme" content="dark">`},
		"light": {`<html lang="en" data-theme="light">`, `<meta name="color-scheme" content="light">`},
		"auto":  {`<html lang="en">`, `<meta name="color-scheme" content="light dark">`},
		"bogus": {`<html lang="en">`, `<meta name="color-scheme" content="light dark">`},
	} {
		body := render(t, "items", Page{User: &User{}, Heading: "Today", Theme: theme})
		for _, w := range want {
			if !strings.Contains(body, w) {
				t.Errorf("theme %q: missing %q", theme, w)
			}
		}
		if (theme == "auto" || theme == "bogus") && strings.Contains(body, "data-theme") {
			t.Errorf("theme %q: should render no data-theme", theme)
		}
	}
}

func TestSessionsPageTemplate(t *testing.T) {
	out := render(t, "settings_sessions", Page{Title: "Sessions", User: &User{ID: "u", Name: "a"}, CSRF: "tok", IdleDays: 7, LifetimeDays: 30})
	for _, want := range []string{"7 days", "30 days", `name="csrf" value="tok"`, `action="/settings/sessions/end-all"`, "data-logout"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
}

// Characterization (#31): the delete word renders, and the badge rules never use the accent.
func TestBadgeWordRenderedForDeleted(t *testing.T) {
	body := render(t, "items", Page{
		User: &User{Name: "a"}, Active: "today", Heading: "Today",
		Items: []Item{{ID: "itm_1", Title: "A", URL: "https://example.com/a", State: "archived", Badge: "deleted"}},
	})
	if !strings.Contains(body, `>deleted</span>`) {
		t.Error("deleted badge word missing")
	}
}

func TestBadgeColorsIgnoreAccent(t *testing.T) {
	css := appCSS(t)
	for _, sel := range []string{".badge {", ".badge["} {
		if strings.Contains(cssBlock(css, sel), "--pico-primary") {
			t.Errorf("%s must not use the accent", sel)
		}
	}
}

// Red (#31): each status carries its word as the color hook.
func TestBadgeDataAttrPerStatus(t *testing.T) {
	body := render(t, "items", Page{
		User: &User{Name: "a"}, Active: "today", Heading: "Today",
		Items: []Item{
			{ID: "itm_1", Title: "A", URL: "https://example.com/a", State: "archived", Badge: "deleted"},
			{ID: "itm_2", Title: "B", URL: "https://example.com/b", State: "archived", Badge: "archived"},
			{ID: "itm_3", Title: "C", URL: "https://example.com/c", State: "pending_deep", Badge: "promoted"},
			{ID: "itm_4", Title: "D", URL: "https://example.com/d", State: "kept", Badge: "kept"},
			{ID: "itm_5", Title: "E", URL: "https://example.com/e", State: "light"},
		},
	})
	for _, w := range []string{"deleted", "archived", "promoted", "kept"} {
		if !strings.Contains(body, `data-badge="`+w+`">`+w+`</span>`) {
			t.Errorf("missing colored badge for %q", w)
		}
	}
	if !strings.Contains(body, `data-badge="" hidden></span>`) {
		t.Error("an untriaged card should render an empty hidden badge")
	}
}

var badgeWords = []string{"deleted", "archived", "promoted", "kept"}

func TestBadgeColorsDefinedForEachStatus(t *testing.T) {
	css := appCSS(t)
	for _, w := range badgeWords {
		b := cssBlock(css, `.badge[data-badge="`+w+`"]`)
		if !strings.Contains(b, "var(--badge-"+w+"-bg)") || !strings.Contains(b, "var(--badge-"+w+"-fg)") {
			t.Errorf("badge %q has no color rule", w)
		}
	}
}

func luminance(hex string) float64 {
	var c [3]float64
	for i := range c {
		v, _ := strconv.ParseUint(hex[1+2*i:3+2*i], 16, 8)
		f := float64(v) / 255
		if f <= 0.03928 {
			c[i] = f / 12.92
		} else {
			c[i] = math.Pow((f+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*c[0] + 0.7152*c[1] + 0.0722*c[2]
}

func contrast(a, b string) float64 {
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// Light values, the OS-dark values and the forced-dark values all exist and meet WCAG AA.
func TestBadgeContrastAA(t *testing.T) {
	css := appCSS(t)
	re := regexp.MustCompile(`--badge-([a-z]+)-(bg|fg):\s*(#[0-9a-fA-F]{6})`)
	blocks := map[string]string{
		"light":       cssBlock(css, ":root {"),
		"os dark":     cssBlock(css, ":root:not([data-theme])"),
		"forced dark": cssBlock(css, `:root[data-theme="dark"]`),
	}
	parsed := map[string]map[string]string{}
	for name, b := range blocks {
		vals := map[string]string{}
		for _, m := range re.FindAllStringSubmatch(b, -1) {
			vals[m[1]+"-"+m[2]] = m[3]
		}
		parsed[name] = vals
		for _, w := range badgeWords {
			bg, fg := vals[w+"-bg"], vals[w+"-fg"]
			if bg == "" || fg == "" {
				t.Errorf("%s: %s colors missing", name, w)
				continue
			}
			if r := contrast(bg, fg); r < 4.5 {
				t.Errorf("%s: %s contrast %.2f < 4.5", name, w, r)
			}
		}
	}
	for k, v := range parsed["os dark"] {
		if parsed["forced dark"][k] != v {
			t.Errorf("OS dark and forced dark differ at %s", k)
		}
	}
}

// The card dims, the badge does not: dimmed text would fail AA.
func TestBadgeNotDimmed(t *testing.T) {
	css := appCSS(t)
	for _, rule := range strings.Split(css, "}") {
		sel, body, ok := strings.Cut(rule, "{")
		if !ok || !strings.Contains(body, "opacity") {
			continue
		}
		for _, one := range strings.Split(sel, ",") {
			one = strings.TrimSpace(one)
			for _, s := range triagedStates {
				if one == `.item[data-state="`+s+`"]` {
					t.Errorf("%s card must not dim as a whole", s)
				}
			}
		}
	}
	if !strings.Contains(css, ".controls > :not(.badge)") || !strings.Contains(css, "opacity: 0.6") {
		t.Error("the card's other parts should still dim, except the badge")
	}
}

func TestBaseRendersAccent(t *testing.T) {
	cases := []struct {
		p    Page
		want string
	}{
		{Page{User: &User{}, Heading: "Today", Accent: "red"}, `<html lang="en" data-accent="red">`},
		{Page{User: &User{}, Heading: "Today", Theme: "dark", Accent: "red"}, `<html lang="en" data-theme="dark" data-accent="red">`},
		{Page{User: &User{}, Heading: "Today", Accent: "jade"}, `<html lang="en">`},
		{Page{User: &User{}, Heading: "Today"}, `<html lang="en">`},
		{Page{Title: "Sign in"}, `<html lang="en">`},
	}
	for _, c := range cases {
		name := "items"
		if c.p.User == nil {
			name = "login"
		}
		if body := render(t, name, c.p); !strings.Contains(body, c.want) {
			t.Errorf("%+v: missing %s", c.p.Accent, c.want)
		}
	}
	if body := render(t, "items", Page{User: &User{}, Heading: "Today", Accent: "jade"}); strings.Contains(body, "data-accent") {
		t.Error("jade renders no data-accent")
	}
}

func TestBaseLinksAccentsAfterPico(t *testing.T) {
	body := render(t, "login", Page{Title: "Sign in"})
	pico, acc, app := strings.Index(body, "pico.jade.min.css"), strings.Index(body, `href="/static/accents.css"`), strings.Index(body, "/static/app.css")
	if strings.Count(body, "/static/accents.css") != 1 || !(pico < acc && acc < app) {
		t.Errorf("accents.css must be linked once, after pico and before app.css (%d %d %d)", pico, acc, app)
	}
}

func TestAppearancePageTemplate(t *testing.T) {
	body := render(t, "settings_appearance", Page{
		User: &User{Name: "a"}, Title: "Appearance", CSRF: "tok", Theme: "dark", Accent: "blue", PhoneFlow: "down",
		AccentOptions: []AccentOption{{"jade", "Jade"}, {"blue", "Blue"}},
	})
	for _, want := range []string{
		`name="theme" value="dark" checked`, `name="phone_flow" value="down" checked`, `name="accent" value="blue" checked`,
		`class="accent-swatch" data-accent="blue" aria-hidden="true"`, `name="csrf" value="tok"`,
		`action="/settings/accent"`, `action="/settings/theme"`, `action="/settings/display"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
}
