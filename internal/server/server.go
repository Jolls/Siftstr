// Package server wires routes and middleware.
package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/Jolls/Siftstr/internal/auth"
	"github.com/Jolls/Siftstr/internal/briefing"
	"github.com/Jolls/Siftstr/internal/connections"
	"github.com/Jolls/Siftstr/internal/outbox"
	"github.com/Jolls/Siftstr/internal/runs"
	"github.com/Jolls/Siftstr/internal/sources"
	"github.com/Jolls/Siftstr/internal/triage"
	"github.com/Jolls/Siftstr/internal/ui"
	"github.com/Jolls/Siftstr/internal/writeback"
)

const (
	maxLoginBody = 16 << 10 // POST /login
	maxFormBody  = 1 << 20  // POST /settings/* and POST /logout
)

const (
	sessionCookieBase = "siftstr_session"
	loginCookieBase   = "siftstr_login"
)

// cookieName adds the __Host- prefix when cookies are Secure, so a sibling
// subdomain cannot set or overwrite them. Browsers accept a __Host- cookie
// only with Secure, Path=/ and no Domain, which is how every cookie here is set.
func cookieName(base string, secure bool) string {
	if secure {
		return "__Host-" + base
	}
	return base
}

func (s *server) sessionCookie() string { return cookieName(sessionCookieBase, s.SecureCookies) }
func (s *server) loginCookie() string   { return cookieName(loginCookieBase, s.SecureCookies) }

// Deps are the server's collaborators.
type Deps struct {
	// Ready reports whether the database is open and migrated; /healthz
	// returns 200 only while it succeeds.
	Ready func(context.Context) error
	Auth  *auth.Service
	// Conns and Sources back the settings pages.
	Conns   *connections.Service
	Sources *sources.Service
	Runs    *runs.Service
	Triage  *triage.Service
	Dest    *writeback.Settings // where kept videos and podcasts go
	// Briefing builds /read and writes the daily markdown files.
	Briefing *briefing.Service
	// Outbox backs the Activity page and the alert for stopped entries.
	Outbox *outbox.Log
	// SecureCookies sets the Secure flag and turns on the HSTS header. It comes
	// from SIFTSTR_BASE_URL, not from r.TLS, because TLS ends at the reverse proxy.
	SecureCookies bool
	// TrustedProxies comes from SIFTSTR_TRUSTED_PROXIES. Only a request whose
	// peer address is inside one of them has its client IP read from
	// X-Forwarded-For.
	TrustedProxies []netip.Prefix
	// CheckConnection tests a saved connection with one cheap upstream call.
	// Nil turns the test off (tests that do not set it make no upstream call).
	CheckConnection func(ctx context.Context, kind, baseURL, secret string) error
	// Kick starts one background run of each named scheduler job for userID
	// (scheduler.Trigger). Nil turns triggers off (tests that do not set it
	// start nothing). Handlers call it through s.kick, after a successful write.
	Kick func(userID string, jobs ...string)
}

type server struct {
	Deps
	ui      *ui.Renderer
	newKeys *oneTimeSecrets
}

// session is the authenticated context handed to protected handlers.
type session struct {
	User  auth.User
	Token string
	CSRF  string
	// Problems is how many stopped outbox entries need attention.
	Problems int
	Theme    string // "auto", "light" or "dark"; empty means auto
	Accent   string // one of triage.Accents; empty means jade
}

// New returns the HTTP handler.
func New(d Deps) (http.Handler, error) {
	r, err := ui.New()
	if err != nil {
		return nil, err
	}
	static, err := ui.Static()
	if err != nil {
		return nil, err
	}
	s := &server{Deps: d, ui: r, newKeys: newOneTimeSecrets(2 * time.Minute)}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.Handle("GET /static/", cacheStatic(static))
	mux.HandleFunc("GET /login", s.loginForm)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.protected(s.logout))
	mux.HandleFunc("GET /api/v1/ping", s.apiAuth(s.ping))
	mux.HandleFunc("POST /api/v1/runs", s.apiAuth(s.createRun))
	mux.HandleFunc("POST /api/v1/runs/{run_id}/summaries", s.apiAuth(s.submitSummaries))
	mux.HandleFunc("GET /{$}", s.protected(func(w http.ResponseWriter, req *http.Request, _ *session) {
		http.Redirect(w, req, "/today", http.StatusSeeOther)
	}))
	mux.HandleFunc("GET /today", s.protected(s.itemsPage("today", "Today", "Nothing to sift yet.")))
	mux.HandleFunc("GET /backlog", s.protected(s.itemsPage("backlog", "Backlog", "Nothing carried over.")))
	mux.HandleFunc("GET /read", s.protected(s.readPage))
	mux.HandleFunc("POST /sync/actions", s.protected(s.syncActions))
	mux.HandleFunc("GET /settings", s.protected(s.settingsHome))
	mux.HandleFunc("GET /settings/sources", s.protected(s.sourcesForm))
	mux.HandleFunc("POST /settings/ingest", s.protected(s.ingestSave))
	mux.HandleFunc("POST /settings/sources/{id}", s.protected(s.sourcesSave))
	mux.HandleFunc("GET /settings/prompts", s.protected(s.promptsForm))
	mux.HandleFunc("POST /settings/prompts", s.protected(s.promptsSave))
	mux.HandleFunc("POST /settings/timezone", s.protected(s.timezoneSave))
	mux.HandleFunc("POST /settings/display", s.protected(s.displaySave))
	mux.HandleFunc("POST /settings/theme", s.protected(s.themeSave))
	mux.HandleFunc("GET /settings/appearance", s.protected(s.appearanceForm))
	mux.HandleFunc("POST /settings/accent", s.protected(s.accentSave))
	mux.HandleFunc("GET /settings/keys", s.protected(s.keysForm))
	mux.HandleFunc("POST /settings/keys", s.protected(s.keyCreate))
	mux.HandleFunc("POST /settings/keys/{id}/revoke", s.protected(s.keyRevoke))
	mux.HandleFunc("GET /settings/sessions", s.protected(s.sessionsForm))
	mux.HandleFunc("POST /settings/sessions/end-all", s.protected(s.sessionsEndAll))
	mux.HandleFunc("GET /settings/activity", s.protected(s.activityForm))
	mux.HandleFunc("POST /settings/activity/release", s.protected(s.activityReleaseNow))
	mux.HandleFunc("POST /settings/activity/{id}/retry", s.protected(s.activityRetry))
	mux.HandleFunc("POST /settings/activity/{id}/dismiss", s.protected(s.activityDismiss))
	mux.HandleFunc("GET /settings/nostr", s.protected(s.nostrForm))
	mux.HandleFunc("POST /settings/nostr", s.protected(s.nostrSave))
	mux.HandleFunc("POST /settings/nostr/delete", s.protected(s.nostrDelete))
	mux.HandleFunc("GET /settings/destinations", s.protected(s.destinationsForm))
	mux.HandleFunc("POST /settings/destinations/keep", s.protected(s.keepSave))
	mux.HandleFunc("POST /settings/destinations/{kind}", s.protected(s.destinationSave))
	mux.HandleFunc("POST /settings/destinations/{kind}/test", s.protected(s.destinationTest))
	mux.HandleFunc("POST /settings/destinations/{kind}/delete", s.protected(s.destinationDelete))
	return secureHeaders(s.SecureCookies, limitForms(mux)), nil
}

func (s *server) healthz(w http.ResponseWriter, req *http.Request) {
	ctx, cancel := context.WithTimeout(req.Context(), 3*time.Second)
	defer cancel()
	if err := s.Ready(ctx); err != nil {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintln(w, "ok")
}

// protected requires a valid session. State-changing methods must also carry
// the session's CSRF token in the "csrf" form field or X-CSRF-Token header.
// Browsers without a session are sent to /login.
func (s *server) protected(h func(http.ResponseWriter, *http.Request, *session)) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		c, err := req.Cookie(s.sessionCookie())
		if err != nil {
			http.Redirect(w, req, "/login", http.StatusSeeOther)
			return
		}
		u, err := s.Auth.UserForSession(req.Context(), c.Value)
		if err != nil {
			if errors.Is(err, auth.ErrNoSession) {
				s.clearCookie(w, s.sessionCookie())
				http.Redirect(w, req, "/login", http.StatusSeeOther)
				return
			}
			serverError(w)
			return
		}
		sess := &session{User: u, Token: c.Value, CSRF: s.Auth.CSRFToken(c.Value)}
		// Only pages render a theme and accent: GETs, and settings POSTs that re-render on error.
		// A failed lookup must never block a page; it just falls back to the OS theme.
		if req.Method == http.MethodGet || req.Method == http.MethodHead || strings.HasPrefix(req.URL.Path, "/settings/") {
			sess.Theme, _ = s.Triage.Theme(req.Context(), u.ID)
			sess.Accent, _ = s.Triage.Accent(req.Context(), u.ID)
		}
		// A failed count must never block a page, so an error just hides the alert.
		if req.Method == http.MethodGet {
			sess.Problems, _ = s.Outbox.Problems(req.Context(), u.ID)
		}
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			got := req.Header.Get("X-CSRF-Token")
			if got == "" {
				got = req.PostFormValue("csrf")
			}
			if !s.Auth.CheckCSRF(c.Value, got) {
				http.Error(w, "invalid CSRF token", http.StatusForbidden)
				return
			}
		}
		h(w, req, sess)
	}
}

// apiAuth requires "Authorization: Bearer <api key>". It is the only auth
// that /api/* accepts: session cookies are ignored here, and API keys are
// ignored everywhere else, so no CSRF check applies to these routes.
func (s *server) apiAuth(h func(http.ResponseWriter, *http.Request, auth.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		scheme, key, ok := strings.Cut(req.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") {
			apiUnauthorized(w)
			return
		}
		u, err := s.Auth.UserForAPIKey(req.Context(), strings.TrimSpace(key))
		if err != nil {
			if errors.Is(err, auth.ErrNoSession) {
				apiUnauthorized(w)
				return
			}
			apiError(w, http.StatusInternalServerError, "internal error")
			return
		}
		h(w, req, u)
	}
}

func apiUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="siftstr"`)
	apiError(w, http.StatusUnauthorized, "unauthorized")
}

// serverError answers 500 without exposing the cause.
func serverError(w http.ResponseWriter) {
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// apiError writes a JSON error body for /api routes.
func apiError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ping lets a scheduled task confirm its key works and see whose it is.
func (s *server) ping(w http.ResponseWriter, _ *http.Request, u auth.User) {
	writeJSON(w, http.StatusOK, map[string]string{"user": u.Username})
}

func (s *server) page(sess *session, active, title string) ui.Page {
	return ui.Page{Title: title, Active: active, User: &ui.User{ID: sess.User.ID, Name: sess.User.Username}, CSRF: sess.CSRF, Problems: sess.Problems, Theme: sess.Theme, Accent: sess.Accent}
}

// badge is the word an actioned card shows. gestures.js has the same rule.
// The stored state is "archived" for both swipe-left outcomes (Q28), so a card
// that was promoted first reads "archived" and any other reads "deleted".
func badge(state string, promoted bool) string {
	switch state {
	case "archived":
		if promoted {
			return "archived"
		}
		return "deleted"
	case "kept":
		return "kept"
	case "pending_deep":
		return "promoted"
	}
	return ""
}

// itemsPage renders one list of cards: today's, or the carried-over backlog.
func (s *server) itemsPage(list, heading, empty string) func(http.ResponseWriter, *http.Request, *session) {
	return func(w http.ResponseWriter, req *http.Request, sess *session) {
		ctx := req.Context()
		today, err := s.Runs.Today(ctx, sess.User.ID)
		var cards []triage.Card
		if err == nil {
			cards, err = s.Triage.Cards(ctx, sess.User.ID, list, today)
		}
		if err != nil {
			serverError(w)
			return
		}
		p := s.page(sess, list, heading)
		p.Heading, p.Empty = heading, empty
		if p.PhoneFlow, err = s.Triage.PhoneFlow(ctx, sess.User.ID); err != nil {
			serverError(w)
			return
		}
		for _, c := range cards {
			p.Items = append(p.Items, ui.Item{
				ID: c.ID, Title: c.Title, URL: c.URL, Source: c.Source, MediaType: c.MediaType,
				Excerpt: c.Excerpt, Summary: c.Summary, State: c.State, Promoted: c.Promoted,
				Badge: badge(c.State, c.Promoted), CanPromote: c.CanPromote,
				Kind: c.Kind, UndoID: c.UndoID,
			})
		}
		w.Header().Set("Cache-Control", "no-store")
		s.ui.Render(w, http.StatusOK, "items", p)
	}
}

func (s *server) loginForm(w http.ResponseWriter, req *http.Request) {
	if c, err := req.Cookie(s.sessionCookie()); err == nil {
		if _, err := s.Auth.UserForSession(req.Context(), c.Value); err == nil {
			http.Redirect(w, req, "/today", http.StatusSeeOther)
			return
		}
	}
	s.renderLogin(w, http.StatusOK, "")
}

// renderLogin issues a fresh double-submit CSRF token for the login form,
// since there is no session to bind one to yet.
func (s *server) renderLogin(w http.ResponseWriter, status int, msg string) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		serverError(w)
		return
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{
		Name: s.loginCookie(), Value: token, Path: "/",
		HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteStrictMode,
	})
	if !s.SecureCookies {
		// Before the cookie moved to Path=/, it lived at /login. An old one
		// would be sent first and fail the check, so expire it.
		http.SetCookie(w, &http.Cookie{
			Name: s.loginCookie(), Value: "", Path: "/login", MaxAge: -1,
			HttpOnly: true, SameSite: http.SameSiteStrictMode,
		})
	}
	w.Header().Set("Cache-Control", "no-store")
	s.ui.Render(w, status, "login", ui.Page{Title: "Sign in", CSRF: token, Error: msg})
}

func (s *server) login(w http.ResponseWriter, req *http.Request) {
	c, err := req.Cookie(s.loginCookie())
	form := req.PostFormValue("csrf")
	if err != nil || form == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(form)) != 1 {
		s.renderLogin(w, http.StatusForbidden, "Your login form expired. Please try again.")
		return
	}
	u, err := s.Auth.Authenticate(req.Context(), req.PostFormValue("username"), req.PostFormValue("password"), clientIP(req, s.TrustedProxies))
	switch {
	case errors.Is(err, auth.ErrRateLimited):
		s.renderLogin(w, http.StatusTooManyRequests, err.Error())
		return
	case errors.Is(err, auth.ErrInvalidCredentials):
		s.renderLogin(w, http.StatusUnauthorized, err.Error())
		return
	case errors.Is(err, context.Canceled):
		return // the client went away while its login was delayed
	case err != nil:
		serverError(w)
		return
	}
	token, expires, err := s.Auth.NewSession(req.Context(), u.ID)
	if err != nil {
		serverError(w)
		return
	}
	s.clearCookie(w, s.loginCookie())
	http.SetCookie(w, &http.Cookie{
		Name: s.sessionCookie(), Value: token, Path: "/", Expires: expires,
		HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, req, "/today", http.StatusSeeOther)
}

func (s *server) logout(w http.ResponseWriter, req *http.Request, sess *session) {
	_ = s.Auth.EndSession(req.Context(), sess.Token)
	s.clearCookie(w, s.sessionCookie())
	http.Redirect(w, req, "/login", http.StatusSeeOther)
}

func (s *server) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteLaxMode,
	})
}

// clientIP is the address a login attempt counts against. It is the peer
// address unless the peer is a trusted proxy. Then X-Forwarded-For is read
// from the right, skipping trusted hops, and the first untrusted address is
// the client. An entry that does not parse stops the walk at the nearest
// trusted hop; if every hop is trusted the leftmost one is used. Entries left
// of the client are never read, so a spoofed prefix costs nothing.
func clientIP(req *http.Request, trusted []netip.Prefix) string {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		host = req.RemoteAddr
	}
	if len(trusted) == 0 {
		return host
	}
	peer, err := netip.ParseAddr(host)
	if err != nil || !isTrusted(peer, trusted) {
		return host
	}
	client := peer.WithZone("").Unmap()
	hops := strings.Split(strings.Join(req.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop := strings.TrimSpace(hops[i])
		if hop == "" {
			continue
		}
		a, err := netip.ParseAddr(hop)
		if err != nil {
			// Some proxies write the client's port too.
			ap, perr := netip.ParseAddrPort(hop)
			if perr != nil {
				return client.String()
			}
			a = ap.Addr()
		}
		client = a.WithZone("").Unmap()
		if !isTrusted(client, trusted) {
			break
		}
	}
	return client.String()
}

func isTrusted(a netip.Addr, trusted []netip.Prefix) bool {
	a = a.WithZone("").Unmap()
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// limitForms caps the body of HTML form POSTs and parses it, so an oversized
// body is refused with 413 before any handler or the CSRF check sees it.
// /sync/actions and /api/ have their own limits and are passed through.
func limitForms(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var limit int64
		switch {
		case req.Method != http.MethodPost:
		case req.URL.Path == "/login":
			limit = maxLoginBody
		case req.URL.Path == "/logout" || strings.HasPrefix(req.URL.Path, "/settings/"):
			limit = maxFormBody
		}
		if limit > 0 {
			req.Body = http.MaxBytesReader(w, req.Body, limit)
			// Parsing caches the form on req, so later PostFormValue calls do
			// not read the body again. The body is capped at limit, so no
			// multipart part can spill to a temporary file.
			// ParseMultipartForm drops ParseForm's error for a body that is not
			// multipart, so check the URL-encoded parse on its own first.
			err := req.ParseForm()
			if err == nil {
				err = req.ParseMultipartForm(limit)
			}
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				w.Header().Set("Connection", "close")
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
		}
		next.ServeHTTP(w, req)
	})
}

// secureHeaders sets headers that suit a plain-HTTP app behind a TLS proxy.
// Inline scripts and styles are blocked; all JS and CSS are embedded files.
// With hsts set (BASE_URL is https) it also sends Strict-Transport-Security;
// browsers ignore that over plain HTTP.
func secureHeaders(hsts bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		if hsts {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, req)
	})
}

func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, req)
	})
}
