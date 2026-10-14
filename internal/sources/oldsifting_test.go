package sources

import (
	"context"
	"testing"
)

func TestOldSiftingSettings(t *testing.T) {
	ctx := context.Background()
	s := setup(t)
	got, err := s.Ingest(ctx, "u1")
	if err != nil || got.OldSiftingDaily != 5 || got.OldSiftingBacklogCap != 20 {
		t.Fatalf("defaults = %+v, %v", got, err)
	}
	in := got
	in.OldSiftingDaily, in.OldSiftingBacklogCap = 0, 7
	if err := s.SetIngest(ctx, "u1", in); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.Ingest(ctx, "u1"); got.OldSiftingDaily != 0 || got.OldSiftingBacklogCap != 7 {
		t.Fatalf("saved = %+v", got)
	}
	if other, _ := s.Ingest(ctx, "u2"); other.OldSiftingDaily != 5 || other.OldSiftingBacklogCap != 20 {
		t.Fatalf("u1's setting leaked to u2: %+v", other)
	}
	for _, bad := range []func(*IngestSettings){
		func(i *IngestSettings) { i.OldSiftingDaily = -1 },
		func(i *IngestSettings) { i.OldSiftingDaily = 51 },
		func(i *IngestSettings) { i.OldSiftingBacklogCap = -1 },
		func(i *IngestSettings) { i.OldSiftingBacklogCap = 501 },
	} {
		b := in
		bad(&b)
		if b.Validate() == nil {
			t.Errorf("%+v accepted", b)
		}
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO user_settings (user_id, key, value) VALUES ('u2', 'old_sifting_daily', '-3')`); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Ingest(ctx, "u2"); got.OldSiftingDaily != 5 {
		t.Fatalf("a stored -3 gave %d, want the default 5", got.OldSiftingDaily)
	}
}

func TestSetIngestLeavesOtherKeysAlone(t *testing.T) {
	ctx := context.Background()
	s := setup(t)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO user_settings (user_id, key, value) VALUES ('u1', 'old_sifting_cursor', '40')`); err != nil {
		t.Fatal(err)
	}
	in, _ := s.Ingest(ctx, "u1")
	if err := s.SetIngest(ctx, "u1", in); err != nil {
		t.Fatal(err)
	}
	var v string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM user_settings WHERE user_id = 'u1' AND key = 'old_sifting_cursor'`).Scan(&v); err != nil || v != "40" {
		t.Fatalf("cursor = %q, %v", v, err)
	}
}
