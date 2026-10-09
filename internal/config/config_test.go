package config

import "testing"

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestDefaults(t *testing.T) {
	c, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.DataDir != "/data" || c.Timezone != "UTC" || c.SecureCookies() {
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
