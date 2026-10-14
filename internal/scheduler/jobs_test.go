package scheduler

import "testing"

func TestIngestJobForSourceKind(t *testing.T) {
	for kind, want := range map[string]string{"miniflux_feed": JobIngestMiniflux, "nostr": JobIngestNostr, "karakeep": JobKarakeep} {
		if got, ok := IngestJobForSourceKind(kind); !ok || got != want {
			t.Errorf("%s = %q, %v", kind, got, ok)
		}
	}
	if got, ok := IngestJobForSourceKind("x"); ok || got != "" {
		t.Errorf("x = %q, %v", got, ok)
	}
}
