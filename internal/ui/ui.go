// Package ui renders the server-side HTML pages.
package ui

import (
	"bytes"
	"cmp"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"

	"github.com/Jolls/Siftstr/internal/briefing"
	"github.com/Jolls/Siftstr/web"
)

// Page is the data every page template receives.
type Page struct {
	Title  string
	Active string // nav entry to mark current: today, backlog, read, briefings, settings
	User   *User  // nil when logged out
	CSRF   string
	Error  string
	// Problems is how many stopped upstream updates need attention; the base
	// template shows an alert for it.
	Problems int
	// Theme is the signed-in user's colour theme: "auto", "light" or "dark".
	// Empty (logged out) behaves as auto. base.html sets data-theme only for light or dark.
	Theme string
	// Accent is the signed-in user's accent color (triage.Accents). Empty (logged out) or "jade"
	// renders no data-accent, so Pico's jade applies.
	Accent string

	// Item list pages (/today, /backlog).
	Heading string
	Empty   string
	Items   []Item

	// Settings pages.
	Notice        string // one-line confirmation, e.g. "Saved."
	Sources       []SourceRow
	Ingest        IngestForm
	Timezone      string
	AccentOptions []AccentOption // /settings/appearance
	PhoneFlow     string         // "up" or "down": list direction on a phone (item lists and /settings/appearance)
	ZoneGroups    []ZoneGroup
	Keys          []KeyRow
	Sessions      []SessionRow // /settings/sessions
	IdleDays      int          // /settings/sessions: auth.SessionIdleTimeout in days
	LifetimeDays  int          // /settings/sessions: auth.SessionLifetime in days
	NewKey        string       // a just-created API key, shown once
	Prompts       []PromptField
	Destinations  []Destination
	NostrRelays   string // /settings/nostr: one relay per line
	NostrNpubs    string // one npub per line
	NostrSaved    bool   // a Nostr connection exists
	Keep          []KeepChoice
	Activity      []ActivityRow

	// The /briefings pages.
	BriefingDays []BriefingDay
	BriefingText string // the stored file shown on /briefings/{day}

	// The /read page.
	Day       string
	Highlight *briefing.Highlight // the day's Karakeep highlight, shown first under Today
	// HighlightColors are the colors the highlight's control offers on /today.
	HighlightColors []HighlightColor
	Today           []briefing.Entry
	Backlog         []briefing.Entry
}

// BriefingDay is one row of /briefings.
type BriefingDay struct {
	Day     string
	HasFile bool // the markdown file is on disk
}

// ActivityRow is one outbox entry on /settings/activity.
type ActivityRow struct {
	ID, Destination, Title, Status, When, Error string
	Attempts                                    int
	Stopped                                     bool
}

// KeepChoice is one row of the "where kept items go" form: a media type and
// a checkbox per destination that can take it.
type KeepChoice struct {
	Field   string // form field, also the setting
	Label   string
	Options []KeepOption
}

// KeepOption is one destination checkbox.
type KeepOption struct {
	Kind      string
	Label     string
	Checked   bool
	Connected bool
}

// IngestForm holds the user-level ingest settings shown above the source list.
type IngestForm struct {
	ExcerptLength int
	MaxAgeDays    int

	KarakeepSummaryDelayMinutes int
	KarakeepSummaryTimeoutHours int

	KarakeepHighlights   bool
	OldSiftingDaily      int
	OldSiftingBacklogCap int
}

// KeyRow is one API key on /settings/keys. The key itself is never part of
// it; only a freshly created key is shown, once, through Page.NewKey.
type KeyRow struct {
	ID       string
	Label    string
	Created  string // already in the user's timezone
	LastUsed string
	Revoked  string
}

// SessionRow is one active session on /settings/sessions. It never carries
// the session token or its hash.
type SessionRow struct {
	Created, LastSeen, Expires string // already in the user's timezone
	Current                    bool
}

// ZoneGroup is one region of the timezone dropdown.
type ZoneGroup struct {
	Region string
	Zones  []string
}

// PromptField is one editable prompt on /settings/prompts.
type PromptField struct {
	Name    string
	Label   string
	Help    string
	Value   string // the saved prompt, or empty when the default applies
	Default string
}

// SourceRow is one editable source on /settings/sources.
type SourceRow struct {
	ID             string
	Name           string
	Category       string
	Kind           string
	Granularity    string
	MaxDepth       string
	Carryover      string
	PromptOverride string
	Enabled        bool
	SummarySource  string // agent | karakeep
}

// Destination is one upstream connection form on /settings/destinations.
// The secret itself is never part of it, only whether one is saved.
type Destination struct {
	Kind         string
	Label        string
	Help         string
	SecretLabel  string
	Connected    bool
	BaseURL      string
	HasSecret    bool
	ID           string // the saved connection, when Connected
	Status       string // check state; "" = never checked
	StatusLabel  string
	StatusHelp   string
	StatusDetail string // short reason, never a secret
	StatusWhen   string // in the user's timezone
	StatusVia    string
	// PlainHTTPPublic: the saved base URL is http:// to a host that is not a private address.
	PlainHTTPPublic bool
}

// User is the part of the signed-in user the templates need.
type User struct {
	ID   string
	Name string
}

// AccentOption is one radio on /settings/appearance.
type AccentOption struct{ Value, Label string }

// Link is where the card's title goes: its Karakeep bookmark when it has one,
// else the original.
func (i Item) Link() string { return cmp.Or(i.KarakeepURL, i.URL) }

// HasOriginal reports whether the original gets its own link beside a title
// that goes to Karakeep.
func (i Item) HasOriginal() bool { return i.KarakeepURL != "" && i.URL != "" }

// HighlightColor is one button of the highlight's color control.
type HighlightColor struct{ Value, Label string }

// Item is one triage card.
type Item struct {
	ID          string
	Title       string
	URL         string
	KarakeepURL string
	Source      string
	MediaType   string
	Excerpt     string // shown until the card has a summary
	Summary     string
	State       string // item state, see ARCHITECTURE.md §5
	Promoted    bool   // has been promoted, so swipe left archives instead of deleting
	Badge       string // "deleted", "archived", "promoted", "kept", or empty
	CanPromote  bool
	Kind        string // "item" or "digest": the subject type actions use
	UndoID      string // the held action that can still be undone, if any
}

// Renderer holds the parsed page templates.
type Renderer struct {
	pages map[string]*template.Template
}

// pages maps a page name to the template file that defines its "content".
var pageFiles = map[string][]string{
	"login":                 {"login.html"},
	"items":                 {"items.html"},
	"read":                  {"read.html"},
	"briefings":             {"briefings.html"},
	"briefing":              {"briefing.html"},
	"settings_sources":      {"settings_nav.html", "settings_sources.html"},
	"settings_appearance":   {"settings_nav.html", "settings_appearance.html"},
	"settings_destinations": {"settings_nav.html", "settings_destinations.html"},
	"settings_prompts":      {"settings_nav.html", "settings_prompts.html"},
	"settings_keys":         {"settings_nav.html", "settings_keys.html"},
	"settings_sessions":     {"settings_nav.html", "settings_sessions.html"},
	"settings_activity":     {"settings_nav.html", "settings_activity.html"},
	"settings_nostr":        {"settings_nav.html", "settings_nostr.html"},
}

// New parses the embedded templates. Each page gets its own set so that
// "content" can be defined once per page alongside the shared base.
func New() (*Renderer, error) {
	r := &Renderer{pages: make(map[string]*template.Template, len(pageFiles))}
	for name, files := range pageFiles {
		paths := []string{"templates/base.html"}
		for _, f := range files {
			paths = append(paths, "templates/"+f)
		}
		t, err := template.ParseFS(web.FS, paths...)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		r.pages[name] = t
	}
	return r, nil
}

// Render writes the named page. It renders to a buffer first so a template
// error never produces a half-written response.
func (r *Renderer) Render(w http.ResponseWriter, status int, name string, p Page) {
	t, ok := r.pages[name]
	if !ok {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "base", p); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// Static serves the embedded static assets under /static/.
func Static() (http.Handler, error) {
	sub, err := fs.Sub(web.FS, "static")
	if err != nil {
		return nil, fmt.Errorf("static assets: %w", err)
	}
	return http.StripPrefix("/static/", http.FileServerFS(sub)), nil
}
