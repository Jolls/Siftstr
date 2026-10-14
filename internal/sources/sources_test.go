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

func TestSummarySource(t *testing.T) {
	ctx := context.Background()
	s := setup(t)
	in := Settings{Granularity: "individual", MaxDepth: "deep", Carryover: "carry", Enabled: true}
	list, _ := s.List(ctx, "u1")
	if list[0].SummarySource != "agent" {
		t.Fatalf("default = %q, want agent", list[0].SummarySource)
	}
	in.SummarySource = "karakeep"
	if err := s.Update(ctx, "u1", "s1", in); err != nil {
		t.Fatal(err)
	}
	if list, _ = s.List(ctx, "u1"); list[1].SummarySource != "karakeep" {
		t.Fatalf("saved = %+v", list[1])
	}
	// A digest has no page for Karakeep to summarize.
	in.Granularity = "digest"
	if err := s.Update(ctx, "u1", "s1", in); err == nil {
		t.Fatal("a digest source was allowed to use Karakeep summaries")
	}
	in.SummarySource = "bogus"
	in.Granularity = "individual"
	if err := s.Update(ctx, "u1", "s1", in); err == nil {
		t.Fatal("unknown summary source accepted")
	}
	// Another user cannot change it.
	in.SummarySource = "karakeep"
	if err := s.Update(ctx, "u2", "s1", in); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

// Bookmarks Karakeep collected are one at a time and always summarized by Karakeep.
func TestKarakeepSourceIsAlwaysIndividualAndKarakeepSummarized(t *testing.T) {
	ctx := context.Background()
	s := setup(t)
	if _, err := s.db.Exec(`INSERT INTO sources (id, user_id, kind, external_id, name) VALUES ('sk','u1','karakeep','karakeep','Karakeep')`); err != nil {
		t.Fatal(err)
	}
	in := Settings{Granularity: "individual", MaxDepth: "light", Carryover: "drop", Enabled: true, SummarySource: "agent"}
	if err := s.Update(ctx, "u1", "sk", in); err != nil {
		t.Fatal(err)
	}
	list, _ := s.List(ctx, "u1")
	for _, x := range list {
		if x.ID == "sk" && (x.SummarySource != "karakeep" || x.MaxDepth != "light" || x.Carryover != "drop") {
			t.Fatalf("%+v: the other settings apply, the summary source does not change", x)
		}
	}
	in.Granularity = "digest"
	if err := s.Update(ctx, "u1", "sk", in); err == nil {
		t.Fatal("Karakeep bookmarks became a digest")
	}
}

func TestKarakeepSummaryIngestSettings(t *testing.T) {
	ctx := context.Background()
	s := setup(t)
	got, err := s.Ingest(ctx, "u1")
	if err != nil || got.KarakeepSummaryDelayMinutes != 30 || got.KarakeepSummaryTimeoutHours != 2 {
		t.Fatalf("defaults = %+v, %v", got, err)
	}
	in := got
	in.KarakeepSummaryDelayMinutes, in.KarakeepSummaryTimeoutHours = 10, 6
	if err := s.SetIngest(ctx, "u1", in); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.Ingest(ctx, "u1"); got.KarakeepSummaryDelayMinutes != 10 || got.KarakeepSummaryTimeoutHours != 6 {
		t.Fatalf("saved = %+v", got)
	}
	if other, _ := s.Ingest(ctx, "u2"); other.KarakeepSummaryTimeoutHours != 2 {
		t.Fatal("u1's setting leaked to u2")
	}
	for _, bad := range []func(*IngestSettings){
		func(i *IngestSettings) { i.KarakeepSummaryDelayMinutes = -1 },
		func(i *IngestSettings) { i.KarakeepSummaryTimeoutHours = 0 },
		func(i *IngestSettings) { i.KarakeepSummaryTimeoutHours = 1000 },
	} {
		b := in
		bad(&b)
		if b.Validate() == nil {
			t.Errorf("%+v accepted", b)
		}
	}
}

// Entries that were waiting for a digest must not be stranded when the source
// is switched to individual items: nothing would ever offer or show them.
func TestSwitchingFromDigestToIndividualReleasesWaitingEntries(t *testing.T) {
	ctx := context.Background()
	s := setup(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE sources SET granularity = 'digest' WHERE id = 's1'`)
	exec(`INSERT INTO digests (id, user_id, source_id, batch_date, state) VALUES ('dig1', 'u1', 's1', '2026-10-09', 'pending_light')`)
	for _, row := range [][]string{{"free", ""}, {"attached", "dig1"}} {
		var d any
		if row[1] != "" {
			d = row[1]
		}
		exec(`INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, state, digest_id, created_at, updated_at)
			VALUES (?, 'u1', 's1', ?, 'article', 'https://example.com/'||?, 't', 'pending_light', ?, 'c', 'c')`, row[0], row[0], row[0], d)
	}
	exec(`INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, state, created_at, updated_at)
		VALUES ('theirs', 'u2', 's3', 'x', 'article', 'https://example.com/t', 't', 'pending_light', 'c', 'c')`)
	exec(`UPDATE users SET timezone = 'Pacific/Auckland' WHERE id = 'u1'`)

	in := Settings{Granularity: "individual", MaxDepth: "deep", Carryover: "carry", Enabled: true}
	if err := s.Update(ctx, "u1", "s1", in); err != nil {
		t.Fatal(err)
	}
	state := func(id string) (st, day string) {
		var d *string
		if err := s.db.QueryRow(`SELECT state, batch_date FROM items WHERE id = ?`, id).Scan(&st, &d); err != nil {
			t.Fatal(err)
		}
		if d != nil {
			day = *d
		}
		return
	}
	if st, day := state("free"); st != "light" || day == "" {
		t.Fatalf("free entry = %s on %q: it should be ready to triage today", st, day)
	}
	if st, _ := state("attached"); st != "pending_light" {
		t.Fatalf("an entry already in a digest = %s: the digest still owns it", st)
	}
	if st, _ := state("theirs"); st != "pending_light" {
		t.Fatalf("another user's entry = %s", st)
	}
}

func TestGetIsScopedToUser(t *testing.T) {
	s := setup(t)
	ctx := context.Background()
	got, err := s.Get(ctx, "u1", "s1")
	if err != nil || got.Kind != "miniflux_feed" || !got.Enabled {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if _, err := s.Get(ctx, "u2", "s1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("other user's source: %v", err)
	}
	if _, err := s.Get(ctx, "u1", "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing source: %v", err)
	}
}
