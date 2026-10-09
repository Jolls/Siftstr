// Package server wires routes and middleware.
package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/Jolls/Siftstr/internal/auth"
	"github.com/Jolls/Siftstr/internal/ui"
)

const (
	sessionCookie = "siftstr_session"
	loginCookie   = "siftstr_login"
)

// Deps are the server's collaborators.
type Deps struct {
	// Ready reports whether the database is open and migrated; /healthz
	// returns 200 only while it succeeds.
	Ready func(context.Context) error
	Auth  *auth.Service
	// SecureCookies sets the Secure flag. It comes from SIFTSTR_BASE_URL, not
	// from r.TLS, because TLS ends at the reverse proxy.
	SecureCookies bool
}

type server struct {
	Deps
	ui *ui.Renderer
}

// session is the authenticated context handed to protected handlers.
type session struct {
	User  auth.User
	Token string
	CSRF  string
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
	s := &server{Deps: d, ui: r}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.Handle("GET /static/", cacheStatic(static))
	mux.HandleFunc("GET /login", s.loginForm)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.protected(s.logout))
	mux.HandleFunc("GET /{$}", s.protected(func(w http.ResponseWriter, req *http.Request, _ *session) {
		http.Redirect(w, req, "/today", http.StatusSeeOther)
	}))
	mux.HandleFunc("GET /today", s.protected(s.itemsPage("today", "Today", "Nothing to sift yet.")))
	mux.HandleFunc("GET /backlog", s.protected(s.itemsPage("backlog", "Backlog", "Nothing carried over.")))
	return secureHeaders(mux), nil
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
		c, err := req.Cookie(sessionCookie)
		if err != nil {
			http.Redirect(w, req, "/login", http.StatusSeeOther)
			return
		}
		u, err := s.Auth.UserForSession(req.Context(), c.Value)
		if err != nil {
			if errors.Is(err, auth.ErrNoSession) {
				s.clearCookie(w, sessionCookie)
				http.Redirect(w, req, "/login", http.StatusSeeOther)
				return
			}
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		sess := &session{User: u, Token: c.Value, CSRF: s.Auth.CSRFToken(c.Value)}
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

func (s *server) page(sess *session, active, title string) ui.Page {
	return ui.Page{Title: title, Active: active, User: &ui.User{Name: sess.User.Username}, CSRF: sess.CSRF}
}

// itemsPage renders an item list. Items arrive with ingest and the triage
// service; until then every list is empty.
func (s *server) itemsPage(active, heading, empty string) func(http.ResponseWriter, *http.Request, *session) {
	return func(w http.ResponseWriter, _ *http.Request, sess *session) {
		p := s.page(sess, active, heading)
		p.Heading, p.Empty = heading, empty
		s.ui.Render(w, http.StatusOK, "items", p)
	}
}

func (s *server) loginForm(w http.ResponseWriter, req *http.Request) {
	if c, err := req.Cookie(sessionCookie); err == nil {
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
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{
		Name: loginCookie, Value: token, Path: "/login",
		HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteStrictMode,
	})
	w.Header().Set("Cache-Control", "no-store")
	s.ui.Render(w, status, "login", ui.Page{Title: "Sign in", CSRF: token, Error: msg})
}

func (s *server) login(w http.ResponseWriter, req *http.Request) {
	c, err := req.Cookie(loginCookie)
	form := req.PostFormValue("csrf")
	if err != nil || form == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(form)) != 1 {
		s.renderLogin(w, http.StatusForbidden, "Your login form expired. Please try again.")
		return
	}
	u, err := s.Auth.Authenticate(req.Context(), req.PostFormValue("username"), req.PostFormValue("password"), clientIP(req))
	switch {
	case errors.Is(err, auth.ErrRateLimited):
		s.renderLogin(w, http.StatusTooManyRequests, err.Error())
		return
	case errors.Is(err, auth.ErrInvalidCredentials):
		s.renderLogin(w, http.StatusUnauthorized, err.Error())
		return
	case err != nil:
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	token, expires, err := s.Auth.NewSession(req.Context(), u.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.clearCookie(w, loginCookie)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", Expires: expires,
		HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, req, "/today", http.StatusSeeOther)
}

func (s *server) logout(w http.ResponseWriter, req *http.Request, sess *session) {
	_ = s.Auth.EndSession(req.Context(), sess.Token)
	s.clearCookie(w, sessionCookie)
	http.Redirect(w, req, "/login", http.StatusSeeOther)
}

func (s *server) clearCookie(w http.ResponseWriter, name string) {
	path := "/"
	if name == loginCookie {
		path = "/login"
	}
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: path, MaxAge: -1,
		HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteLaxMode,
	})
}

// clientIP is the peer address. Behind a reverse proxy this is the proxy's
// address, so the per-IP login limit then applies to all clients together.
func clientIP(req *http.Request) string {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return req.RemoteAddr
	}
	return host
}

// secureHeaders sets headers that suit a plain-HTTP app behind a TLS proxy.
// Inline scripts and styles are blocked; all JS and CSS are embedded files.
func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, req)
	})
}

func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, req)
	})
}
