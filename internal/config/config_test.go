package config

import (
	"fmt"
	"io/fs"
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

var noFiles = func(string) ([]byte, error) { return nil, fs.ErrNotExist }

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestDefaults(t *testing.T) {
	c, err := Load(env(nil), noFiles)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.DataDir != "/data" || c.Timezone != "UTC" || c.SecureCookies() || c.TrustedProxies != nil {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestBaseURL(t *testing.T) {
	c, err := Load(env(map[string]string{"SIFTSTR_BASE_URL": "https://example.com/"}), noFiles)
	if err != nil || c.BaseURL != "https://example.com" || !c.SecureCookies() {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := Load(env(map[string]string{"SIFTSTR_BASE_URL": "example.com"}), noFiles); err == nil {
		t.Fatal("expected error for URL without scheme")
	}
}

func TestTrustedProxies(t *testing.T) {
	load := func(v string) ([]netip.Prefix, error) {
		c, err := Load(env(map[string]string{"SIFTSTR_TRUSTED_PROXIES": v}), noFiles)
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
		c, err := Load(env(map[string]string{"SIFTSTR_BASE_URL": in}), noFiles)
		if err != nil || c.SecureCookies() != want {
			t.Errorf("%q: SecureCookies=%v err=%v, want %v", in, c.SecureCookies(), err, want)
		}
	}
}

func TestUnrelatedEnvIsIgnored(t *testing.T) {
	a, err := Load(env(map[string]string{"SIFTSTR_FOO": "bar"}), noFiles)
	b, err2 := Load(env(nil), noFiles)
	if err != nil || err2 != nil || !reflect.DeepEqual(a, b) {
		t.Fatalf("%+v %v / %+v %v", a, err, b, err2)
	}
}

func TestNoSeedsByDefault(t *testing.T) {
	c, err := Load(env(nil), noFiles)
	if err != nil || c.Seeds != nil {
		t.Fatalf("%+v %v", c.Seeds, err)
	}
}

func TestSeedsFromEnv(t *testing.T) {
	c, err := Load(env(map[string]string{
		"SIFTSTR_MINIFLUX_URL": " http://miniflux:8080 ", "SIFTSTR_MINIFLUX_API_KEY": "mk",
		"SIFTSTR_KARAKEEP_URL": "http://karakeep:3000",
		"SIFTSTR_METUBE_URL":   "http://metube:8081",
	}), noFiles)
	want := []SeedConnection{
		{"miniflux", "http://miniflux:8080", "mk"},
		{"karakeep", "http://karakeep:3000", ""},
		{"metube", "http://metube:8081", ""},
	}
	if err != nil || !reflect.DeepEqual(c.Seeds, want) {
		t.Fatalf("%+v %v", c.Seeds, err)
	}
}

func TestSeedKeyFromFile(t *testing.T) {
	read := func(p string) ([]byte, error) {
		if p != "/run/secrets/kk" {
			return nil, fs.ErrNotExist
		}
		return []byte("kk-key\n"), nil
	}
	c, err := Load(env(map[string]string{"SIFTSTR_KARAKEEP_URL": "http://k:3000", "SIFTSTR_KARAKEEP_API_KEY_FILE": "/run/secrets/kk"}), read)
	if err != nil || len(c.Seeds) != 1 || c.Seeds[0].APIKey != "kk-key" {
		t.Fatalf("%+v %v", c.Seeds, err)
	}
}

func TestSeedKeyFileErrors(t *testing.T) {
	blank := func(string) ([]byte, error) { return []byte("  \n"), nil }
	secretRead := func(string) ([]byte, error) { return nil, fmt.Errorf("denied") }
	cases := []struct {
		name string
		env  map[string]string
		read func(string) ([]byte, error)
		want string
	}{
		{"both", map[string]string{"SIFTSTR_MINIFLUX_URL": "http://m", "SIFTSTR_MINIFLUX_API_KEY": "SECRETVAL", "SIFTSTR_MINIFLUX_API_KEY_FILE": "/f"}, noFiles, "SIFTSTR_MINIFLUX_API_KEY_FILE"},
		{"unreadable", map[string]string{"SIFTSTR_MINIFLUX_URL": "http://m", "SIFTSTR_MINIFLUX_API_KEY_FILE": "/f"}, secretRead, "SIFTSTR_MINIFLUX_API_KEY_FILE"},
		{"empty", map[string]string{"SIFTSTR_MINIFLUX_URL": "http://m", "SIFTSTR_MINIFLUX_API_KEY_FILE": "/f"}, blank, "empty"},
	}
	for _, tc := range cases {
		_, err := Load(env(tc.env), tc.read)
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "SECRETVAL") {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

func TestSeedKeyWithoutURL(t *testing.T) {
	_, err := Load(env(map[string]string{"SIFTSTR_KARAKEEP_API_KEY": "KEYVAL"}), noFiles)
	if err == nil || !strings.Contains(err.Error(), "SIFTSTR_KARAKEEP_URL") || strings.Contains(err.Error(), "KEYVAL") {
		t.Fatalf("%v", err)
	}
}

func TestSeedURLValidation(t *testing.T) {
	for _, raw := range []string{"ftp://x.example", "x.example", "https://u:p@x.example", "https://x.example/?t=1", "https://x.example/#f"} {
		_, err := Load(env(map[string]string{"SIFTSTR_MINIFLUX_URL": raw}), noFiles)
		if err == nil || !strings.Contains(err.Error(), "SIFTSTR_MINIFLUX_URL") || strings.Contains(err.Error(), raw) {
			t.Errorf("%q: %v", raw, err)
		}
	}
}

func TestConfigStringHidesSeedKey(t *testing.T) {
	c, err := Load(env(map[string]string{"SIFTSTR_MINIFLUX_URL": "http://m", "SIFTSTR_MINIFLUX_API_KEY": "KEYVAL"}), noFiles)
	if err != nil {
		t.Fatal(err)
	}
	if out := fmt.Sprintf("%+v %v", c, c.Seeds); strings.Contains(out, "KEYVAL") {
		t.Fatal("key printed")
	}
}
