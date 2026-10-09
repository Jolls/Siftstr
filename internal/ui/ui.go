// Package ui renders the server-side HTML pages.
package ui

import (
	"bytes"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"

	"github.com/Jolls/Siftstr/web"
)

// Page is the data every page template receives.
type Page struct {
	Title  string
	Active string // nav entry to mark current: today, backlog, read, settings
	User   *User  // nil when logged out
	CSRF   string
	Error  string

	// Item list pages (/today, /backlog).
	Heading string
	Empty   string
	Items   []Item

	// Settings pages.
	Notice       string // one-line confirmation, e.g. "Saved."
	Sources      []SourceRow
	Ingest       IngestForm
	Timezone     string
	ZoneGroups   []ZoneGroup
	Keys         []KeyRow
	NewKey       string // a just-created API key, shown once
	Prompts      []PromptField
	Destinations []Destination
	Keep         []KeepChoice
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
}

// Destination is one upstream connection form on /settings/destinations.
// The secret itself is never part of it, only whether one is saved.
type Destination struct {
	Kind        string
	Label       string
	Help        string
	SecretLabel string
	Connected   bool
	BaseURL     string
	HasSecret   bool
}

// User is the part of the signed-in user the templates need.
type User struct {
	ID   string
	Name string
}

// Item is one triage card.
type Item struct {
	ID         string
	Title      string
	URL        string
	Source     string
	MediaType  string
	Summary    string
	State      string // item state, see ARCHITECTURE.md §5
	Badge      string // "archived", "promoted", "kept", or empty
	CanPromote bool
	Kind       string // "item" or "digest": the subject type actions use
	UndoID     string // the held action that can still be undone, if any
}

// Renderer holds the parsed page templates.
type Renderer struct {
	pages map[string]*template.Template
}

// pages maps a page name to the template file that defines its "content".
var pageFiles = map[string][]string{
	"login":                 {"login.html"},
	"items":                 {"items.html"},
	"settings_sources":      {"settings_nav.html", "settings_sources.html"},
	"settings_destinations": {"settings_nav.html", "settings_destinations.html"},
	"settings_prompts":      {"settings_nav.html", "settings_prompts.html"},
	"settings_keys":         {"settings_nav.html", "settings_keys.html"},
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
