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
}

// User is the part of the signed-in user the templates need.
type User struct {
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
}

// Renderer holds the parsed page templates.
type Renderer struct {
	pages map[string]*template.Template
}

// pages maps a page name to the template file that defines its "content".
var pageFiles = map[string]string{
	"login": "login.html",
	"items": "items.html",
}

// New parses the embedded templates. Each page gets its own set so that
// "content" can be defined once per page alongside the shared base.
func New() (*Renderer, error) {
	r := &Renderer{pages: make(map[string]*template.Template, len(pageFiles))}
	for name, file := range pageFiles {
		t, err := template.ParseFS(web.FS, "templates/base.html", "templates/"+file)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", file, err)
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
