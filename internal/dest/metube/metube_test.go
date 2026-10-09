package metube

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Jolls/Siftstr/internal/dest"
)

func TestSendAddsOnceAndSkipsKnownURLs(t *testing.T) {
	var added []string
	known := map[string]string{} // url -> which list
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/history":
			out := map[string][]map[string]string{"done": {}, "queue": {}, "pending": {}}
			for u, list := range known {
				out[list] = append(out[list], map[string]string{"url": u})
			}
			_ = json.NewEncoder(w).Encode(out)
		case "/add":
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			u := b["url"].(string)
			added = append(added, u)
			known[u] = "queue"
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		}
	}))
	defer srv.Close()
	d := New(srv.URL, "s3cret", nil)
	it := dest.Item{ID: "i", URL: "https://video.example.com/watch?v=1"}
	for i := 0; i < 3; i++ {
		if err := d.Send(context.Background(), it); err != nil {
			t.Fatal(err)
		}
	}
	if len(added) != 1 {
		t.Fatalf("added %v", added)
	}
	known["https://video.example.com/watch?v=2"] = "done"
	_ = d.Send(context.Background(), dest.Item{ID: "j", URL: "https://video.example.com/watch?v=2"})
	if len(added) != 1 {
		t.Fatalf("a URL MeTube already finished was added again: %v", added)
	}
	if auth != "Bearer s3cret" {
		t.Fatalf("auth %q", auth)
	}
}

func TestUnreachableMetubeIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "x", 503) }))
	defer srv.Close()
	if err := New(srv.URL, "", nil).Send(context.Background(), dest.Item{ID: "i", URL: "https://example.com/v"}); err == nil {
		t.Fatal("want error")
	}
}
