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
	Seeds          []SeedConnection // SIFTSTR_<SVC>_URL and _API_KEY; nil when none set
}

// SeedConnection is an upstream connection supplied by instance config. It is
// applied to one user's connections at startup (ARCHITECTURE.md section 13).
type SeedConnection struct {
	Kind   string // "miniflux", "karakeep" or "metube"
	URL    string
	APIKey string // empty when not set; never log it
}

// String hides the key so a %v of the config cannot print it.
func (s SeedConnection) String() string {
	return fmt.Sprintf("{%s %s key set: %t}", s.Kind, s.URL, s.APIKey != "")
}

// Load reads the config using getenv (os.Getenv in production). readFile is
// os.ReadFile in production; it reads *_FILE variables.
func Load(getenv func(string) string, readFile func(string) ([]byte, error)) (Config, error) {
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
	seeds, err := parseSeeds(getenv, readFile)
	if err != nil {
		return Config{}, err
	}
	c.Seeds = seeds
	return c, nil
}

// seedKinds are the connections instance config can seed, in the order they
// are applied. hasKey is false for kinds whose secret stays UI-only.
var seedKinds = []struct {
	kind, prefix string
	hasKey       bool
}{
	{"miniflux", "SIFTSTR_MINIFLUX", true},
	{"karakeep", "SIFTSTR_KARAKEEP", true},
	{"metube", "SIFTSTR_METUBE", false},
}

func parseSeeds(getenv func(string) string, readFile func(string) ([]byte, error)) ([]SeedConnection, error) {
	var out []SeedConnection
	for _, k := range seedKinds {
		raw := strings.TrimSpace(getenv(k.prefix + "_URL"))
		var key string
		if k.hasKey {
			var err error
			if key, err = readKey(getenv, readFile, k.prefix+"_API_KEY"); err != nil {
				return nil, err
			}
		}
		if raw == "" {
			if key != "" {
				return nil, fmt.Errorf("%s_API_KEY is set but %s_URL is not", k.prefix, k.prefix)
			}
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("%s_URL must be an http(s) URL without credentials, a query string or a fragment", k.prefix)
		}
		out = append(out, SeedConnection{Kind: k.kind, URL: raw, APIKey: key})
	}
	return out, nil
}

// readKey reads name or name_FILE. Errors name the variable, never the value.
func readKey(getenv func(string) string, readFile func(string) ([]byte, error), name string) (string, error) {
	val := strings.TrimSpace(getenv(name))
	path := strings.TrimSpace(getenv(name + "_FILE"))
	switch {
	case val != "" && path != "":
		return "", fmt.Errorf("set %s or %s_FILE, not both", name, name)
	case path != "":
		b, err := readFile(path)
		if err != nil {
			return "", fmt.Errorf("%s_FILE: %w", name, err)
		}
		v := strings.TrimSpace(string(b))
		if v == "" {
			return "", fmt.Errorf("%s_FILE: file is empty", name)
		}
		return v, nil
	}
	return val, nil
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
