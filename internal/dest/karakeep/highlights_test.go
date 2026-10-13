package karakeep

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func highlightServer(t *testing.T) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/highlights", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		switch r.URL.Query().Get("cursor") {
		case "":
			if r.URL.Query().Get("limit") != "2" {
				t.Errorf("limit = %q, want 2", r.URL.Query().Get("limit"))
			}
			_, _ = w.Write([]byte(`{"highlights":[
				{"id":"h1","bookmarkId":"b1","color":"yellow","text":"first","note":"why","createdAt":"2026-10-01T10:00:00.000Z"},
				{"id":"h2","bookmarkId":"b2","color":"red","text":null,"note":null,"createdAt":"2026-09-01T10:00:00.000Z"}],
				"nextCursor":"h2_2026-09-01T10:00:00.000Z"}`))
		case "h2_2026-09-01T10:00:00.000Z":
			_, _ = w.Write([]byte(`{"highlights":[{"id":"h3","bookmarkId":"b3","color":"blue","text":"last","note":null,"createdAt":"2026-08-01T10:00:00.000Z"}],"nextCursor":null}`))
		default:
			http.Error(w, "bad cursor", http.StatusBadRequest)
		}
	})
	mux.HandleFunc("GET /api/v1/highlights/h1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"h1","bookmarkId":"b1","color":"yellow","text":"first","note":"why","createdAt":"2026-10-01T10:00:00.000Z"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "tok", nil)
}

func TestListHighlightsPagesAndNulls(t *testing.T) {
	c := highlightServer(t)
	p, err := c.ListHighlights(context.Background(), "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Highlights) != 2 || p.NextCursor == "" {
		t.Fatalf("page 1 = %+v", p)
	}
	h := p.Highlights[0]
	if h.ID != "h1" || h.BookmarkID != "b1" || h.Text != "first" || h.Note != "why" || h.Color != "yellow" || h.CreatedAt.IsZero() {
		t.Errorf("h1 = %+v", h)
	}
	if h2 := p.Highlights[1]; h2.Text != "" || h2.Note != "" {
		t.Errorf("null text and note should read as empty: %+v", h2)
	}
	p, err = c.ListHighlights(context.Background(), p.NextCursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Highlights) != 1 || p.NextCursor != "" {
		t.Errorf("last page = %+v", p)
	}
}

func TestGetHighlight(t *testing.T) {
	c := highlightServer(t)
	h, err := c.GetHighlight(context.Background(), "h1")
	if err != nil || h.Text != "first" {
		t.Fatalf("GetHighlight = %+v, %v", h, err)
	}
	if _, err := c.GetHighlight(context.Background(), "gone"); !errors.Is(err, ErrHighlightNotFound) {
		t.Errorf("missing highlight error = %v, want ErrHighlightNotFound", err)
	}
}
