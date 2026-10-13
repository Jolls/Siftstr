// Package config reads instance configuration from environment variables.
// Everything else is stored in the database (ARCHITECTURE.md section 13).
package config

import (
	"fmt"
	"net/netip"
	"net/url"
	"strings"
)

// Config is the instance-level configuration.
type Config struct {
	Listen         string
	DataDir        string
	BaseURL        string         // public URL; empty if unset
	TrustedProxies []netip.Prefix // SIFTSTR_TRUSTED_PROXIES; nil means X-Forwarded-For is never read
	SecretKey      string         // empty means use /data/secret.key
	AdminUser      string
	AdminPassword  string
	Timezone       string
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
		// url.Parse accepts HTTPS://; keep the stored scheme lowercase so the
		// https:// prefix check in SecureCookies sees it.
		u.Scheme = strings.ToLower(u.Scheme)
		c.BaseURL = u.String()
	}
	proxies, err := parseTrustedProxies(getenv("SIFTSTR_TRUSTED_PROXIES"))
	if err != nil {
		return Config{}, err
	}
	c.TrustedProxies = proxies
	return c, nil
}

// parseTrustedProxies reads a comma-separated list of CIDRs. A bare IP
// counts as a single-address prefix. Empty entries are skipped.
func parseTrustedProxies(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if p, err := netip.ParsePrefix(f); err == nil {
			// Client addresses are unmapped before they are compared, so an
			// IPv4-mapped prefix must be written as the IPv4 one it means.
			if a := p.Addr(); a.Is4In6() && p.Bits() >= 96 {
				p = netip.PrefixFrom(a.Unmap(), p.Bits()-96)
			}
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(f)
		if err != nil {
			return nil, fmt.Errorf("SIFTSTR_TRUSTED_PROXIES: %q is not a CIDR or IP address", f)
		}
		a = a.WithZone("").Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// SecureCookies reports whether cookies get the Secure flag. It follows
// SIFTSTR_BASE_URL, never r.TLS, because TLS ends at the reverse proxy.
func (c Config) SecureCookies() bool {
	return strings.HasPrefix(c.BaseURL, "https://")
}
