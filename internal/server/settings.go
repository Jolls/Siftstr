package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Jolls/Siftstr/internal/connections"
	"github.com/Jolls/Siftstr/internal/sources"
	"github.com/Jolls/Siftstr/internal/ui"
)

// destinationKinds are the connections with a settings form, in display
// order. The rest (YouTube, Nextcloud, Nostr) arrive with their phases.
var destinationKinds = []ui.Destination{
	{Kind: "miniflux", Label: "Miniflux", Help: "Where feeds and unread entries come from, and where read state is written back.", SecretLabel: "API token"},
	{Kind: "karakeep", Label: "Karakeep", Help: "Where kept articles are saved as bookmarks.", SecretLabel: "API key"},
	{Kind: "metube", Label: "MeTube", Help: "Where kept videos are sent to download.", SecretLabel: "Secret (optional)"},
}

var notices = map[string]string{"saved": "Saved.", "disconnected": "Disconnected."}

func (s *server) settingsPage(sess *session, req *http.Request, title string) ui.Page {
	p := s.page(sess, "settings", title)
	p.Notice = notices[req.URL.Query().Get("notice")]
	return p
}

func (s *server) settingsHome(w http.ResponseWriter, req *http.Request, _ *session) {
	http.Redirect(w, req, "/settings/sources", http.StatusSeeOther)
}

func (s *server) sourcesForm(w http.ResponseWriter, req *http.Request, sess *session) {
	s.renderSources(w, req, sess, http.StatusOK, "")
}

func (s *server) renderSources(w http.ResponseWriter, req *http.Request, sess *session, status int, msg string) {
	list, err := s.Sources.List(req.Context(), sess.User.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	ing, err := s.Sources.Ingest(req.Context(), sess.User.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	p := s.settingsPage(sess, req, "Sources")
	p.Error = msg
	p.Ingest = ui.IngestForm{ExcerptLength: ing.ExcerptLength, MaxAgeDays: ing.MaxAgeDays}
	for _, x := range list {
		p.Sources = append(p.Sources, ui.SourceRow{
			ID: x.ID, Name: x.Name, Category: x.Category, Kind: x.Kind,
			Granularity: x.Granularity, MaxDepth: x.MaxDepth, Carryover: x.Carryover,
			PromptOverride: x.PromptOverride, Enabled: x.Enabled,
		})
	}
	s.ui.Render(w, status, "settings_sources", p)
}

func (s *server) sourcesSave(w http.ResponseWriter, req *http.Request, sess *session) {
	in := sources.Settings{
		Granularity:    req.PostFormValue("granularity"),
		MaxDepth:       req.PostFormValue("max_depth"),
		Carryover:      req.PostFormValue("carryover"),
		PromptOverride: req.PostFormValue("prompt_override"),
		Enabled:        req.PostFormValue("enabled") != "",
	}
	if err := in.Validate(); err != nil {
		s.renderSources(w, req, sess, http.StatusUnprocessableEntity, err.Error()+".")
		return
	}
	err := s.Sources.Update(req.Context(), sess.User.ID, req.PathValue("id"), in)
	switch {
	case errors.Is(err, sources.ErrNotFound):
		http.NotFound(w, req) // another user's source looks the same as a missing one
	case err != nil:
		http.Error(w, "internal error", http.StatusInternalServerError)
	default:
		http.Redirect(w, req, "/settings/sources?notice=saved", http.StatusSeeOther)
	}
}

func (s *server) ingestSave(w http.ResponseWriter, req *http.Request, sess *session) {
	excerpt, err1 := strconv.Atoi(strings.TrimSpace(req.PostFormValue("excerpt_length")))
	maxAge, err2 := strconv.Atoi(strings.TrimSpace(req.PostFormValue("max_age_days")))
	if err1 != nil || err2 != nil {
		s.renderSources(w, req, sess, http.StatusUnprocessableEntity, "Both values must be whole numbers.")
		return
	}
	in := sources.IngestSettings{ExcerptLength: excerpt, MaxAgeDays: maxAge}
	if err := in.Validate(); err != nil {
		s.renderSources(w, req, sess, http.StatusUnprocessableEntity, err.Error()+".")
		return
	}
	if err := s.Sources.SetIngest(req.Context(), sess.User.ID, in); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, req, "/settings/sources?notice=saved", http.StatusSeeOther)
}

func (s *server) destinationsForm(w http.ResponseWriter, req *http.Request, sess *session) {
	s.renderDestinations(w, req, sess, http.StatusOK, "")
}

func (s *server) renderDestinations(w http.ResponseWriter, req *http.Request, sess *session, status int, msg string) {
	saved, err := s.Conns.List(req.Context(), sess.User.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	p := s.settingsPage(sess, req, "Destinations")
	p.Error = msg
	for _, d := range destinationKinds {
		for _, c := range saved {
			if c.Kind == d.Kind {
				d.Connected, d.BaseURL, d.HasSecret = true, c.BaseURL, c.HasSecret
				break
			}
		}
		p.Destinations = append(p.Destinations, d)
	}
	s.ui.Render(w, status, "settings_destinations", p)
}

func knownDestination(kind string) bool {
	for _, d := range destinationKinds {
		if d.Kind == kind {
			return true
		}
	}
	return false
}

// existing returns the user's connection of this kind, if any.
func (s *server) existing(req *http.Request, userID, kind string) (connections.Connection, bool, error) {
	list, err := s.Conns.List(req.Context(), userID)
	if err != nil {
		return connections.Connection{}, false, err
	}
	for _, c := range list {
		if c.Kind == kind {
			return c, true, nil
		}
	}
	return connections.Connection{}, false, nil
}

func (s *server) destinationSave(w http.ResponseWriter, req *http.Request, sess *session) {
	kind := req.PathValue("kind")
	if !knownDestination(kind) {
		http.NotFound(w, req)
		return
	}
	baseURL := strings.TrimSpace(req.PostFormValue("base_url"))
	secret := req.PostFormValue("secret")
	cur, found, err := s.existing(req, sess.User.ID, kind)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if found {
		_, err = s.Conns.Update(req.Context(), sess.User.ID, cur.ID, connections.UpdateInput{
			BaseURL: &baseURL, Secret: &secret, ClearSecret: req.PostFormValue("clear_secret") != "",
		})
	} else {
		_, err = s.Conns.Create(req.Context(), sess.User.ID, connections.Input{Kind: kind, BaseURL: baseURL, Secret: &secret})
	}
	switch {
	case err == nil:
		http.Redirect(w, req, "/settings/destinations?notice=saved", http.StatusSeeOther)
	case strings.HasPrefix(err.Error(), "base URL"):
		s.renderDestinations(w, req, sess, http.StatusUnprocessableEntity, err.Error()+".")
	default:
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func (s *server) destinationDelete(w http.ResponseWriter, req *http.Request, sess *session) {
	kind := req.PathValue("kind")
	if !knownDestination(kind) {
		http.NotFound(w, req)
		return
	}
	cur, found, err := s.existing(req, sess.User.ID, kind)
	if err == nil && found {
		err = s.Conns.Delete(req.Context(), sess.User.ID, cur.ID)
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, req, "/settings/destinations?notice=disconnected", http.StatusSeeOther)
}
