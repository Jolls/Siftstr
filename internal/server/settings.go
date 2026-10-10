package server

import (
	"errors"
	"github.com/Jolls/Siftstr/internal/outbox"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Jolls/Siftstr/internal/auth"
	"github.com/Jolls/Siftstr/internal/connections"
	"github.com/Jolls/Siftstr/internal/ingest/nostr"
	"github.com/Jolls/Siftstr/internal/runs"
	"github.com/Jolls/Siftstr/internal/sources"
	"github.com/Jolls/Siftstr/internal/ui"
	"github.com/Jolls/Siftstr/internal/writeback"
)

// destinationKinds are the connections with a settings form, in display
// order. The rest (YouTube, Nextcloud, Nostr) arrive with their phases.
var destinationKinds = []ui.Destination{
	{Kind: "miniflux", Label: "Miniflux", Help: "Where feeds and unread entries come from, and where read state is written back.", SecretLabel: "API token"},
	{Kind: "karakeep", Label: "Karakeep", Help: "Where kept articles are saved as bookmarks.", SecretLabel: "API key"},
	{Kind: "metube", Label: "MeTube", Help: "Where kept videos are sent to download.", SecretLabel: "Secret (optional)"},
}

var notices = map[string]string{"saved": "Saved.", "disconnected": "Disconnected.", "revoked": "Key revoked.", "retried": "Queued to send again.", "dismissed": "Dismissed.", "released": "Held actions released. They will be sent shortly."}

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
		serverError(w)
		return
	}
	ing, err := s.Sources.Ingest(req.Context(), sess.User.ID)
	if err != nil {
		serverError(w)
		return
	}
	p := s.settingsPage(sess, req, "Sources")
	p.Error = msg
	if p.Timezone, err = s.Runs.Timezone(req.Context(), sess.User.ID); err != nil {
		serverError(w)
		return
	}
	for _, g := range runs.ZoneGroups(p.Timezone) {
		p.ZoneGroups = append(p.ZoneGroups, ui.ZoneGroup{Region: g.Region, Zones: g.Zones})
	}
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
		serverError(w)
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
		serverError(w)
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
		serverError(w)
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
	choice, err := s.Dest.Get(req.Context(), sess.User.ID)
	if err != nil {
		serverError(w)
		return
	}
	for _, row := range []struct {
		field, label string
		picked       []string
	}{{"video", "Kept videos", choice.Video}, {"podcast", "Kept podcast episodes", choice.Podcast}} {
		kc := ui.KeepChoice{Field: row.field, Label: row.label}
		for _, d := range destinationKinds {
			if !slices.Contains(writeback.VideoCapable, d.Kind) {
				continue
			}
			kc.Options = append(kc.Options, ui.KeepOption{
				Kind: d.Kind, Label: d.Label, Checked: slices.Contains(row.picked, d.Kind),
				Connected: slices.ContainsFunc(saved, func(c connections.Connection) bool { return c.Kind == d.Kind }),
			})
		}
		p.Keep = append(p.Keep, kc)
	}
	s.ui.Render(w, status, "settings_destinations", p)
}

func knownDestination(kind string) bool {
	return slices.ContainsFunc(destinationKinds, func(d ui.Destination) bool { return d.Kind == kind })
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

// keepSave stores where kept videos and podcasts go. Articles and posts always
// go to Karakeep, so there is nothing to choose for them.
func (s *server) keepSave(w http.ResponseWriter, req *http.Request, sess *session) {
	if err := req.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	err := s.Dest.Set(req.Context(), sess.User.ID, writeback.Choice{Video: req.PostForm["video"], Podcast: req.PostForm["podcast"]})
	if err != nil {
		serverError(w)
		return
	}
	http.Redirect(w, req, "/settings/destinations?notice=saved", http.StatusSeeOther)
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
		serverError(w)
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
	case errors.Is(err, connections.ErrBaseURL):
		s.renderDestinations(w, req, sess, http.StatusUnprocessableEntity, err.Error()+".")
	default:
		serverError(w)
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
		serverError(w)
		return
	}
	http.Redirect(w, req, "/settings/destinations?notice=disconnected", http.StatusSeeOther)
}

var promptFields = []ui.PromptField{
	{Name: runs.PromptGlobal, Label: "Global instructions", Help: "Tone and rules that apply to every summary.", Default: runs.Defaults.Global},
	{Name: runs.PromptLight, Label: "Light summary", Help: "Used for each new item, from its title and excerpt.", Default: runs.Defaults.Light},
	{Name: runs.PromptDeep, Label: "Deep summary", Help: "Used the morning after you promote an item.", Default: runs.Defaults.Deep},
	{Name: runs.PromptDigest, Label: "Digest", Help: "Used for sources set to one digest a day.", Default: runs.Defaults.Digest},
}

func (s *server) promptsForm(w http.ResponseWriter, req *http.Request, sess *session) {
	s.renderPrompts(w, req, sess, http.StatusOK, "")
}

func (s *server) renderPrompts(w http.ResponseWriter, req *http.Request, sess *session, status int, msg string) {
	cur, err := s.Runs.Prompts(req.Context(), sess.User.ID)
	if err != nil {
		serverError(w)
		return
	}
	p := s.settingsPage(sess, req, "Prompts")
	p.Error = msg
	for _, f := range promptFields {
		if v := cur.Get(f.Name); v != f.Default {
			f.Value = v
		}
		p.Prompts = append(p.Prompts, f)
	}
	s.ui.Render(w, status, "settings_prompts", p)
}

func (s *server) promptsSave(w http.ResponseWriter, req *http.Request, sess *session) {
	err := s.Runs.SetPrompt(req.Context(), sess.User.ID, req.PostFormValue("name"), req.PostFormValue("value"))
	switch {
	case errors.Is(err, runs.ErrUnknownPrompt):
		http.NotFound(w, req)
	case errors.Is(err, runs.ErrPromptTooLong):
		s.renderPrompts(w, req, sess, http.StatusUnprocessableEntity, err.Error()+".")
	case err != nil:
		serverError(w)
	default:
		http.Redirect(w, req, "/settings/prompts?notice=saved", http.StatusSeeOther)
	}
}

func (s *server) timezoneSave(w http.ResponseWriter, req *http.Request, sess *session) {
	if err := s.Runs.SetTimezone(req.Context(), sess.User.ID, req.PostFormValue("timezone")); err != nil {
		if errors.Is(err, runs.ErrBadTimezone) {
			s.renderSources(w, req, sess, http.StatusUnprocessableEntity, err.Error()+".")
			return
		}
		serverError(w)
		return
	}
	http.Redirect(w, req, "/settings/sources?notice=saved", http.StatusSeeOther)
}

func (s *server) activityForm(w http.ResponseWriter, req *http.Request, sess *session) {
	rows, err := s.Outbox.Recent(req.Context(), sess.User.ID, 100)
	if err != nil {
		serverError(w)
		return
	}
	loc, err := time.LoadLocation(sess.User.Timezone)
	if err != nil {
		loc = time.UTC
	}
	p := s.settingsPage(sess, req, "Activity")
	for _, r := range rows {
		p.Activity = append(p.Activity, ui.ActivityRow{
			ID: r.ID, Destination: r.Op, Title: r.Title, Status: r.Status, Attempts: r.Attempts,
			When: r.CreatedAt.In(loc).Format("2006-01-02 15:04"), Error: r.LastError, Stopped: r.Stopped,
		})
	}
	w.Header().Set("Cache-Control", "no-store")
	s.ui.Render(w, http.StatusOK, "settings_activity", p)
}

func (s *server) activityRetry(w http.ResponseWriter, req *http.Request, sess *session) {
	s.activityChange(w, req, s.Outbox.Retry(req.Context(), sess.User.ID, req.PathValue("id")), "retried")
}

func (s *server) activityDismiss(w http.ResponseWriter, req *http.Request, sess *session) {
	s.activityChange(w, req, s.Outbox.Dismiss(req.Context(), sess.User.ID, req.PathValue("id"), time.Now()), "dismissed")
}

func (s *server) activityReleaseNow(w http.ResponseWriter, req *http.Request, sess *session) {
	if _, err := s.Triage.ReleaseNow(req.Context(), sess.User.ID); err != nil {
		serverError(w)
		return
	}
	http.Redirect(w, req, "/settings/activity?notice=released", http.StatusSeeOther)
}

func (s *server) activityChange(w http.ResponseWriter, req *http.Request, err error, notice string) {
	switch {
	case errors.Is(err, outbox.ErrNotFound):
		http.NotFound(w, req) // another user's entry looks the same as a missing one
	case err != nil:
		serverError(w)
	default:
		http.Redirect(w, req, "/settings/activity?notice="+notice, http.StatusSeeOther)
	}
}

func (s *server) keysForm(w http.ResponseWriter, req *http.Request, sess *session) {
	newKey, _ := s.newKeys.take(sess.Token) // set by keyCreate, shown once
	s.renderKeys(w, req, sess, http.StatusOK, "", newKey)
}

// renderKeys lists the user's keys. newKey is set only on the first page load
// after creating one, and that response is never cached.
func (s *server) renderKeys(w http.ResponseWriter, req *http.Request, sess *session, status int, msg, newKey string) {
	keys, err := s.Auth.ListAPIKeys(req.Context(), sess.User.ID)
	if err != nil {
		serverError(w)
		return
	}
	loc, err := time.LoadLocation(sess.User.Timezone)
	if err != nil {
		loc = time.UTC
	}
	show := func(t time.Time) string { return t.In(loc).Format("2006-01-02 15:04") }
	p := s.settingsPage(sess, req, "API keys")
	p.Error, p.NewKey = msg, newKey
	for _, k := range keys {
		row := ui.KeyRow{ID: k.ID, Label: k.Label, Created: show(k.CreatedAt)}
		if k.LastUsedAt != nil {
			row.LastUsed = show(*k.LastUsedAt)
		}
		if k.RevokedAt != nil {
			row.Revoked = show(*k.RevokedAt)
		}
		p.Keys = append(p.Keys, row)
	}
	w.Header().Set("Cache-Control", "no-store")
	s.ui.Render(w, status, "settings_keys", p)
}

func (s *server) keyCreate(w http.ResponseWriter, req *http.Request, sess *session) {
	label := strings.TrimSpace(req.PostFormValue("label"))
	if utf8.RuneCountInString(label) > 80 {
		s.renderKeys(w, req, sess, http.StatusUnprocessableEntity, "The label is longer than 80 characters.", "")
		return
	}
	_, secret, err := s.Auth.CreateAPIKey(req.Context(), sess.User.ID, label)
	if err != nil {
		serverError(w)
		return
	}
	// Redirect so a refresh reloads the list instead of re-submitting the form.
	s.newKeys.put(sess.Token, secret)
	http.Redirect(w, req, "/settings/keys", http.StatusSeeOther)
}

func (s *server) keyRevoke(w http.ResponseWriter, req *http.Request, sess *session) {
	err := s.Auth.RevokeAPIKey(req.Context(), sess.User.ID, req.PathValue("id"))
	switch {
	case errors.Is(err, auth.ErrNotFound):
		http.NotFound(w, req) // another user's key looks the same as a missing one
	case err != nil:
		serverError(w)
	default:
		http.Redirect(w, req, "/settings/keys?notice=revoked", http.StatusSeeOther)
	}
}

func (s *server) nostrForm(w http.ResponseWriter, req *http.Request, sess *session) {
	s.renderNostr(w, req, sess, http.StatusOK, "", nil)
}

// renderNostr shows the saved relays and npubs, or, after a rejected save,
// what the user typed so nothing is lost.
func (s *server) renderNostr(w http.ResponseWriter, req *http.Request, sess *session, status int, msg string, typed *[2]string) {
	cur, found, err := s.existing(req, sess.User.ID, "nostr")
	if err != nil {
		serverError(w)
		return
	}
	p := s.settingsPage(sess, req, "Nostr")
	p.Error, p.NostrSaved = msg, found
	if typed != nil {
		p.NostrRelays, p.NostrNpubs = typed[0], typed[1]
	} else if found {
		cfg := nostr.ParseConfig(cur.Config)
		p.NostrRelays = strings.Join(cfg.Relays, "\n")
		npubs := make([]string, len(cfg.Npubs))
		for i, k := range cfg.Npubs {
			npubs[i] = nostr.Npub(k)
		}
		p.NostrNpubs = strings.Join(npubs, "\n")
	}
	s.ui.Render(w, status, "settings_nostr", p)
}

func (s *server) nostrSave(w http.ResponseWriter, req *http.Request, sess *session) {
	relays, npubs := req.PostFormValue("relays"), req.PostFormValue("npubs")
	cfg, err := nostr.NewConfig(relays, npubs)
	if err != nil {
		s.renderNostr(w, req, sess, http.StatusUnprocessableEntity, err.Error()+".", &[2]string{relays, npubs})
		return
	}
	cur, found, err := s.existing(req, sess.User.ID, "nostr")
	if err == nil && found {
		_, err = s.Conns.Update(req.Context(), sess.User.ID, cur.ID, connections.UpdateInput{Config: cfg.JSON()})
	} else if err == nil {
		_, err = s.Conns.Create(req.Context(), sess.User.ID, connections.Input{Kind: "nostr", Config: cfg.JSON()})
	}
	if err != nil {
		serverError(w)
		return
	}
	http.Redirect(w, req, "/settings/nostr?notice=saved", http.StatusSeeOther)
}

func (s *server) nostrDelete(w http.ResponseWriter, req *http.Request, sess *session) {
	cur, found, err := s.existing(req, sess.User.ID, "nostr")
	if err == nil && found {
		err = s.Conns.Delete(req.Context(), sess.User.ID, cur.ID)
	}
	if err != nil {
		serverError(w)
		return
	}
	http.Redirect(w, req, "/settings/nostr?notice=disconnected", http.StatusSeeOther)
}
