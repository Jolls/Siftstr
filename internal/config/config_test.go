package config

import (
	"net/netip"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestDefaults(t *testing.T) {
	c, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.DataDir != "/data" || c.Timezone != "UTC" || c.SecureCookies() || c.TrustedProxies != nil {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestBaseURL(t *testing.T) {
	c, err := Load(env(map[string]string{"SIFTSTR_BASE_URL": "https://example.com/"}))
	if err != nil || c.BaseURL != "https://example.com" || !c.SecureCookies() {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := Load(env(map[string]string{"SIFTSTR_BASE_URL": "example.com"})); err == nil {
		t.Fatal("expected error for URL without scheme")
	}
}

func TestTrustedProxies(t *testing.T) {
	load := func(v string) ([]netip.Prefix, error) {
		c, err := Load(env(map[string]string{"SIFTSTR_TRUSTED_PROXIES": v}))
		return c.TrustedProxies, err
	}
	got, err := load("10.0.0.0/8, fd00::/8 ,192.168.1.10")
	want := []string{"10.0.0.0/8", "fd00::/8", "192.168.1.10/32"}
	if err != nil || len(got) != len(want) {
		t.Fatalf("%v %v", got, err)
	}
	for i, p := range got {
		if p.String() != want[i] {
			t.Errorf("entry %d = %v, want %v", i, p, want[i])
		}
	}
	if got, err := load("10.0.0.1/8"); err != nil || len(got) != 1 || got[0].String() != "10.0.0.0/8" {
		t.Errorf("host bits: %v %v", got, err)
	}
	if got, err := load("::ffff:172.18.0.0/112"); err != nil || len(got) != 1 || got[0].String() != "172.18.0.0/16" {
		t.Errorf("4-in-6 prefix: %v %v", got, err)
	}
	if got, err := load(" , "); err != nil || got != nil {
		t.Errorf("empty entries: %v %v", got, err)
	}
	for _, bad := range []string{"nonsense", "10.0.0.0/33"} {
		if _, err := load(bad); err == nil || !strings.Contains(err.Error(), "SIFTSTR_TRUSTED_PROXIES") {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
}

func TestSecureCookiesByScheme(t *testing.T) {
	for in, want := range map[string]bool{
		"":                             false,
		"http://192.168.1.20:8080":     false,
		"https://siftstr.example.com":  true,
		"https://siftstr.example.com/": true,
		"HTTPS://siftstr.example.com":  true,
	} {
		c, err := Load(env(map[string]string{"SIFTSTR_BASE_URL": in}))
		if err != nil || c.SecureCookies() != want {
			t.Errorf("%q: SecureCookies=%v err=%v, want %v", in, c.SecureCookies(), err, want)
		}
	}
}
