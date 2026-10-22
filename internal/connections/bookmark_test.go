package connections

import (
	"context"
	"testing"
)

func TestBookmarkURL(t *testing.T) {
	if got := BookmarkURL("https://kk.example.com/", "a b"); got != "https://kk.example.com/dashboard/preview/a%20b" {
		t.Errorf("BookmarkURL = %q", got)
	}
	if got := BookmarkURL("", "x"); got != "" {
		t.Errorf("no base URL: %q", got)
	}
	if got := BookmarkURL("https://kk", ""); got != "" {
		t.Errorf("no bookmark ID: %q", got)
	}
}

func TestKarakeepBaseURLIsPerUser(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	for _, q := range []string{
		`INSERT INTO connections (id, user_id, kind, base_url) VALUES ('c1','u1','karakeep','https://kk1.example.com')`,
		`INSERT INTO connections (id, user_id, kind, base_url) VALUES ('c2','u2','miniflux','https://mf.example.com')`,
	} {
		if _, err := e.st.DB().Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for user, want := range map[string]string{"u1": "https://kk1.example.com", "u2": ""} {
		got, err := KarakeepBaseURL(ctx, e.st.DB(), user)
		if err != nil || got != want {
			t.Errorf("%s: base = %q, %v; want %q", user, got, err, want)
		}
	}
}
