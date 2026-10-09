// Package server wires routes and middleware.
package server

import (
	"fmt"
	"net/http"

	"github.com/Jolls/Siftstr/internal/ui"
)

// New returns the HTTP handler. Auth, sessions and CSRF arrive with
// internal/auth; until then /login only renders the form.
func New() (http.Handler, error) {
	r, err := ui.New()
	if err != nil {
		return nil, err
	}
	static, err := ui.Static()
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
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
