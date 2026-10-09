package sources

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jolls/Siftstr/internal/store"
)

func setup(t *testing.T) *Service {
	t.Helper()
	st, err := store.OpenFile(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, q := range []string{
		`INSERT INTO users (id, username, password_hash, created_at) VALUES ('u1','u1','x','n'), ('u2','u2','x','n')`,
		`INSERT INTO sources (id, user_id, kind, external_id, name, category) VALUES ('s1','u1','miniflux_feed','1','B','Z'), ('s2','u1','miniflux_feed','2','A','Z'), ('s3','u2','miniflux_feed','1','Other','')`,
	} {
		if _, err := st.DB().Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return New(st.DB())
}

func TestListIsScopedAndOrdered(t *testing.T) {
	s := setup(t)
	got, err := s.List(context.Background(), "u1")
	if err != nil || len(got) != 2 || got[0].ID != "s2" || got[1].ID != "s1" {
		t.Fatalf("list = %+v, %v", got, err)
	}
	if got[0].Granularity != "individual" || got[0].MaxDepth != "deep" || !got[0].Enabled {
		t.Fatalf("defaults wrong: %+v", got[0])
	}
}

func TestUpdate(t *testing.T) {
	ctx := context.Background()
	s := setup(t)
	in := Settings{Granularity: "digest", MaxDepth: "light", Carryover: "drop", PromptOverride: "  be brief ", Enabled: false}
	if err := s.Update(ctx, "u1", "s1", in); err != nil {
		t.Fatal(err)
	}
	list, _ := s.List(ctx, "u1")
	var x Source
	for _, c := range list {
		if c.ID == "s1" {
			x = c
		}
	}
	if x.Granularity != "digest" || x.MaxDepth != "light" || x.Carryover != "drop" || x.PromptOverride != "be brief" || x.Enabled {
		t.Fatalf("got %+v", x)
	}
	in.PromptOverride = ""
	in.Enabled = true
	if err := s.Update(ctx, "u1", "s1", in); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateOtherUsersSourceIsNotFound(t *testing.T) {
	s := setup(t)
	in := Settings{Granularity: "digest", MaxDepth: "light", Carryover: "drop", Enabled: true}
	if err := s.Update(context.Background(), "u1", "s3", in); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	list, _ := s.List(context.Background(), "u2")
	if list[0].Granularity != "individual" {
		t.Fatal("u2's source was modified")
	}
}

func TestUpdateValidation(t *testing.T) {
	s := setup(t)
	good := Settings{Granularity: "individual", MaxDepth: "deep", Carryover: "carry", Enabled: true}
	bad := []Settings{good, good, good, good}
	bad[0].Granularity = "x"
	bad[1].MaxDepth = ""
	bad[2].Carryover = "keep"
	bad[3].PromptOverride = strings.Repeat("a", MaxPromptLen+1)
	for i, b := range bad {
		if err := s.Update(context.Background(), "u1", "s1", b); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}
