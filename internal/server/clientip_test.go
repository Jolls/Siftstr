package server

import (
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestClientIP(t *testing.T) {
	t10 := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	fd := []netip.Prefix{netip.MustParsePrefix("fd00::/8")}
	cases := []struct {
		name    string
		trusted []netip.Prefix
		peer    string
		xff     []string
		want    string
	}{
		{"no trusted proxies", nil, "203.0.113.7:5555", []string{"1.2.3.4"}, "203.0.113.7"},
		{"untrusted peer", t10, "203.0.113.7:5555", []string{"1.2.3.4"}, "203.0.113.7"},
		{"trusted peer", t10, "10.0.0.2:1", []string{"198.51.100.9"}, "198.51.100.9"},
		{"spoofed prefix and trusted hop", t10, "10.0.0.2:1", []string{"6.6.6.6, 198.51.100.9, 10.0.0.3"}, "198.51.100.9"},
		{"two headers", t10, "10.0.0.2:1", []string{"6.6.6.6", "198.51.100.9"}, "198.51.100.9"},
		{"no header", t10, "10.0.0.2:1", nil, "10.0.0.2"},
		{"all hops trusted", t10, "10.0.0.2:1", []string{"10.0.0.5, 10.0.0.4"}, "10.0.0.5"},
		{"unparseable hop", t10, "10.0.0.2:1", []string{"198.51.100.9, junk"}, "10.0.0.2"},
		{"empty entries", t10, "10.0.0.2:1", []string{"198.51.100.9,, "}, "198.51.100.9"},
		{"hop with port", t10, "10.0.0.2:1", []string{"198.51.100.9:4711"}, "198.51.100.9"},
		{"ipv6 hop with port", t10, "10.0.0.2:1", []string{"[2001:db8::7]:4711"}, "2001:db8::7"},
		{"ipv6", fd, "[fd00::1]:443", []string{"2001:db8::5"}, "2001:db8::5"},
		{"mapped peer", t10, "[::ffff:10.0.0.2]:1", []string{"198.51.100.9"}, "198.51.100.9"},
		{"peer without port", t10, "10.0.0.2", []string{"198.51.100.9"}, "198.51.100.9"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/login", nil)
			req.RemoteAddr = c.peer
			for _, v := range c.xff {
				req.Header.Add("X-Forwarded-For", v)
			}
			if got := clientIP(req, c.trusted); got != c.want {
				t.Errorf("clientIP = %q, want %q", got, c.want)
			}
		})
	}
}
