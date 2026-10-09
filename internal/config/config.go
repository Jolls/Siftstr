// Package config reads instance configuration from environment variables.
// Everything else is stored in the database (ARCHITECTURE.md section 13).
package config

import (
	"fmt"
	"net/url"
	"strings"
)

// Config is the instance-level configuration.
type Config struct {
	Listen        string
	DataDir       string
	BaseURL       string // public URL; empty if unset
	SecretKey     string // empty means use /data/secret.key
	AdminUser     string
	AdminPassword string
	Timezone      string
}

// Load reads the config using getenv (os.Getenv in production).
func Load(getenv func(string) string) (Config, error) {
	def := func(key, fallback string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return fallback
	}
	c := Config{
		Listen:        def("SIFTSTR_LISTEN", ":8080"),
		DataDir:       def("SIFTSTR_DATA_DIR", "/data"),
		BaseURL:       strings.TrimRight(def("SIFTSTR_BASE_URL", ""), "/"),
		SecretKey:     getenv("SIFTSTR_SECRET_KEY"),
		AdminUser:     def("SIFTSTR_ADMIN_USER", ""),
		AdminPassword: getenv("SIFTSTR_ADMIN_PASSWORD"),
		Timezone:      def("TZ", "UTC"),
	}
	if c.BaseURL != "" {
		u, err := url.Parse(c.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return Config{}, fmt.Errorf("SIFTSTR_BASE_URL must be an http(s) URL")
		}
	}
	return c, nil
}

// SecureCookies reports whether cookies get the Secure flag. It follows
// SIFTSTR_BASE_URL, never r.TLS, because TLS ends at the reverse proxy.
func (c Config) SecureCookies() bool {
	return strings.HasPrefix(c.BaseURL, "https://")
}
