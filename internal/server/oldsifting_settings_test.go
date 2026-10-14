package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestOldSiftingSettingsSaveAndKeepWhenOmitted(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	_, body := e.get(t, c, "/settings/sources")
	for _, want := range []string{`name="old_sifting_daily" min="0" max="50" value="5"`, `name="old_sifting_backlog_cap" min="0" max="500" value="20"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q", want)
		}
	}
	form := func(extra url.Values) url.Values {
		v := url.Values{"csrf": {csrf}, "max_age_days": {"14"}, "excerpt_length": {"500"}}
		for k, vs := range extra {
			v[k] = vs
		}
		return v
	}
	if resp := e.post(t, c, "/settings/ingest", form(url.Values{"old_sifting_daily": {"0"}, "old_sifting_backlog_cap": {"12"}})); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save = %d", resp.StatusCode)
	}
	_, body = e.get(t, c, "/settings/sources")
	if !strings.Contains(body, `name="old_sifting_daily" min="0" max="50" value="0"`) || !strings.Contains(body, `name="old_sifting_backlog_cap" min="0" max="500" value="12"`) {
		t.Fatal("saved values not shown")
	}
	if resp := e.post(t, c, "/settings/ingest", form(nil)); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save without the fields = %d", resp.StatusCode)
	}
	_, body = e.get(t, c, "/settings/sources")
	if !strings.Contains(body, `name="old_sifting_daily" min="0" max="50" value="0"`) || !strings.Contains(body, `name="old_sifting_backlog_cap" min="0" max="500" value="12"`) {
		t.Fatal("omitted fields did not keep the saved values")
	}
	for label, extra := range map[string]url.Values{"text": {"old_sifting_daily": {"x"}}, "cap too big": {"old_sifting_backlog_cap": {"501"}}} {
		if resp := e.post(t, c, "/settings/ingest", form(extra)); resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d, want 422", label, resp.StatusCode)
		}
	}
	bc, _ := e.signedIn(t, "bob")
	_, body = e.get(t, bc, "/settings/sources")
	if !strings.Contains(body, `name="old_sifting_daily" min="0" max="50" value="5"`) {
		t.Fatal("alice's setting leaked to bob")
	}
}
