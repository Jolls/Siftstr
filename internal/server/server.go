// Package server wires routes and middleware.
package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/Jolls/Siftstr/internal/ui"
)

// New returns the HTTP handler. ready reports whether the database is open
// and migrated; /healthz returns 200 only while it succeeds. Auth, sessions
// and CSRF arrive with internal/auth; until then /login only renders the form.
func New(ready func(context.Context) error) (http.Handler, error) {
	r, err := ui.New()
	if err != nil {
		return nil, err
	}
	static, err := ui.Static()
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), 3*time.Second)
		defer cancel()
		if err := ready(ctx); err != nil {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	})
	mux.Handle("GET /static/", cacheStatic(static))
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, _ *http.Request) {
		r.Render(w, http.StatusOK, "login", ui.Page{Title: "Sign in"})
	})
	return secureHeaders(mux), nil
}

// secureHeaders sets headers that suit a plain-HTTP app behind a TLS proxy.
// Inline scripts and styles are blocked; all JS and CSS are embedded files.
func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
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
