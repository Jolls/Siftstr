package connections

import (
	"context"
	"errors"
	"testing"
)

func seedCount(t *testing.T, e env, user, kind string) int {
	t.Helper()
	var n int
	if err := e.st.DB().QueryRow(`SELECT COUNT(*) FROM connections WHERE user_id = ? AND kind = ?`, user, kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestApplySeedCreates(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	res, err := e.svc.ApplySeed(ctx, "u1", Seed{Kind: "miniflux", BaseURL: "http://mf:8080", Secret: "env-key"})
	if err != nil || !res.Created || !res.SecretSet {
		t.Fatalf("%+v %v", res, err)
	}
	list, _ := e.svc.List(ctx, "u1")
	if len(list) != 1 || list[0].BaseURL != "http://mf:8080" {
		t.Fatalf("%+v", list)
	}
	if got, _ := e.svc.Secret(ctx, "u1", list[0].ID); got != "env-key" {
		t.Fatalf("secret = %q", got)
	}
}

func TestApplySeedURLOnly(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	res, err := e.svc.ApplySeed(ctx, "u1", Seed{Kind: "karakeep", BaseURL: "http://kk:3000"})
	if err != nil || !res.Created || res.SecretSet {
		t.Fatalf("%+v %v", res, err)
	}
	list, _ := e.svc.List(ctx, "u1")
	if len(list) != 1 || list[0].HasSecret {
		t.Fatalf("%+v", list)
	}
}

func TestApplySeedIsIdempotent(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	in := Seed{Kind: "miniflux", BaseURL: "http://mf:8080", Secret: "k"}
	if _, err := e.svc.ApplySeed(ctx, "u1", in); err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.ApplySeed(ctx, "u1", in)
	if err != nil || res != (SeedResult{}) || seedCount(t, e, "u1", "miniflux") != 1 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestApplySeedUpdatesURLKeepsKey(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	c, _ := e.svc.Create(ctx, "u1", Input{Kind: "miniflux", BaseURL: "http://old:8080", Secret: str("ui-key")})
	res, err := e.svc.ApplySeed(ctx, "u1", Seed{Kind: "miniflux", BaseURL: "http://new:9090", Secret: "env-key"})
	if err != nil || !res.URLUpdated || res.SecretSet || res.Created {
		t.Fatalf("%+v %v", res, err)
	}
	got, _ := e.svc.Get(ctx, "u1", c.ID)
	sec, _ := e.svc.Secret(ctx, "u1", c.ID)
	if got.BaseURL != "http://new:9090" || sec != "ui-key" || seedCount(t, e, "u1", "miniflux") != 1 {
		t.Fatalf("%+v %q", got, sec)
	}
}

func TestApplySeedFillsMissingKey(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	c, _ := e.svc.Create(ctx, "u1", Input{Kind: "miniflux", BaseURL: "http://mf:8080"})
	res, err := e.svc.ApplySeed(ctx, "u1", Seed{Kind: "miniflux", BaseURL: "http://mf:8080", Secret: "env-key"})
	if err != nil || res.URLUpdated || !res.SecretSet {
		t.Fatalf("%+v %v", res, err)
	}
	if sec, _ := e.svc.Secret(ctx, "u1", c.ID); sec != "env-key" {
		t.Fatalf("secret = %q", sec)
	}
}

func TestApplySeedIsUserScoped(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	c2, _ := e.svc.Create(ctx, "u2", Input{Kind: "miniflux", BaseURL: "http://x:1", Secret: str("u2-key")})
	res, err := e.svc.ApplySeed(ctx, "u1", Seed{Kind: "miniflux", BaseURL: "http://mf:8080", Secret: "k"})
	if err != nil || !res.Created {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := e.svc.ApplySeed(ctx, "u1", Seed{Kind: "miniflux", BaseURL: "http://other:8080"}); err != nil {
		t.Fatal(err)
	}
	got, _ := e.svc.Get(ctx, "u2", c2.ID)
	sec, _ := e.svc.Secret(ctx, "u2", c2.ID)
	if got.BaseURL != "http://x:1" || sec != "u2-key" {
		t.Fatalf("u2 changed: %+v %q", got, sec)
	}
}

func TestApplySeedRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	if _, err := e.svc.ApplySeed(ctx, "u1", Seed{Kind: "nope", BaseURL: "http://x"}); err == nil {
		t.Error("unknown kind accepted")
	}
	for _, u := range []string{"ftp://x.example", ""} {
		if _, err := e.svc.ApplySeed(ctx, "u1", Seed{Kind: "miniflux", BaseURL: u}); !errors.Is(err, ErrBaseURL) {
			t.Errorf("%q: %v", u, err)
		}
	}
	if n := seedCount(t, e, "u1", "miniflux"); n != 0 {
		t.Errorf("%d rows written", n)
	}
}
