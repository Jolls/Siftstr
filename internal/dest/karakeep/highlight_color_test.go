package karakeep

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Jolls/Siftstr/internal/dest"
)

// colorServer is a Karakeep that knows one highlight, h1, and records the
// PATCH bodies it gets for it. status, when set, is returned instead.
type colorServer struct {
	mu     sync.Mutex
	bodies []string
	auth   string
	status int
	color  string
}

func (cs *colorServer) client(t *testing.T) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("PATCH /api/v1/highlights/{id}", func(w http.ResponseWriter, r *http.Request) {
		cs.mu.Lock()
		defer cs.mu.Unlock()
		b, _ := io.ReadAll(r.Body)
		cs.bodies = append(cs.bodies, string(b))
		cs.auth = r.Header.Get("Authorization")
		if cs.status != 0 {
			http.Error(w, "no", cs.status)
			return
		}
		if r.PathValue("id") != "h1" {
			http.Error(w, "no such highlight", http.StatusNotFound)
			return
		}
		if string(b) == `{"color":"red"}` {
			cs.color = "red"
		} else if string(b) == `{"color":"blue"}` {
			cs.color = "blue"
		}
		_, _ = w.Write([]byte(`{"id":"h1","bookmarkId":"b1","color":"` + cs.color + `","text":"t","note":null,"createdAt":"2026-10-01T10:00:00.000Z"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "tok", nil)
}

func TestHighlightColorsAreKarakeepsFixedSet(t *testing.T) {
	if want := []string{"yellow", "red", "green", "blue"}; !reflect.DeepEqual(HighlightColors, want) {
		t.Errorf("HighlightColors = %v, want %v", HighlightColors, want)
	}
	if got := HighlightColorLabel("yellow"); got != "Yellow" {
		t.Errorf("label = %q", got)
	}
	for _, c := range []string{"purple", "", "Yellow"} {
		if got := HighlightColorLabel(c); got != "" {
			t.Errorf("HighlightColorLabel(%q) = %q, want empty", c, got)
		}
	}
}

func TestUpdateHighlightColorPatchesOnlyTheColor(t *testing.T) {
	cs := &colorServer{}
	if err := cs.client(t).UpdateHighlightColor(context.Background(), "h1", "red"); err != nil {
		t.Fatal(err)
	}
	if len(cs.bodies) != 1 || cs.bodies[0] != `{"color":"red"}` {
		t.Errorf("bodies = %q, want only the color", cs.bodies)
	}
	if cs.auth != "Bearer tok" {
		t.Errorf("auth = %q", cs.auth)
	}
}

func TestUpdateHighlightColorMissingHighlight(t *testing.T) {
	cs := &colorServer{}
	err := cs.client(t).UpdateHighlightColor(context.Background(), "gone", "red")
	if !errors.Is(err, ErrHighlightNotFound) {
		t.Errorf("err = %v, want ErrHighlightNotFound", err)
	}
}

func isPermanent(err error) bool {
	var p interface{ Permanent() bool }
	return asPermanent(err, &p)
}

func TestSendHighlightColorIsIdempotent(t *testing.T) {
	cs := &colorServer{}
	d := &Destination{c: cs.client(t)}
	it := dest.Item{ID: "h1", Verb: dest.VerbHighlightColor, HighlightID: "h1", HighlightColor: "blue"}
	for range 2 {
		if err := d.Send(context.Background(), it); err != nil {
			t.Fatal(err)
		}
	}
	if cs.color != "blue" || len(cs.bodies) != 2 || cs.bodies[0] != cs.bodies[1] {
		t.Errorf("color = %q, bodies = %q", cs.color, cs.bodies)
	}
	if d.BookmarkID() != "" {
		t.Errorf("a highlight color change recorded bookmark %q", d.BookmarkID())
	}
}

func TestSendHighlightColorBadInputIsPermanent(t *testing.T) {
	cs := &colorServer{}
	d := &Destination{c: cs.client(t)}
	for _, it := range []dest.Item{
		{Verb: dest.VerbHighlightColor, HighlightColor: "red"},
		{Verb: dest.VerbHighlightColor, HighlightID: "h1", HighlightColor: "purple"},
		{Verb: dest.VerbHighlightColor, HighlightID: "h1"},
	} {
		if err := d.Send(context.Background(), it); err == nil || !isPermanent(err) || strings.Contains(err.Error(), "unknown verb") {
			t.Errorf("%+v: err = %v, want a permanent error that is about the input", it, err)
		}
	}
	if len(cs.bodies) != 0 {
		t.Errorf("bad input still reached Karakeep: %q", cs.bodies)
	}
}

func TestSendHighlightColorOfADeletedHighlightIsPermanent(t *testing.T) {
	cs := &colorServer{}
	d := &Destination{c: cs.client(t)}
	err := d.Send(context.Background(), dest.Item{Verb: dest.VerbHighlightColor, HighlightID: "gone", HighlightColor: "red"})
	if err == nil || !isPermanent(err) || len(cs.bodies) != 1 {
		t.Errorf("err = %v, requests = %d; want the request sent and a permanent error shown in Activity", err, len(cs.bodies))
	}
}

func TestSendHighlightColorServerErrorRetries(t *testing.T) {
	cs := &colorServer{status: 500}
	d := &Destination{c: cs.client(t)}
	it := dest.Item{Verb: dest.VerbHighlightColor, HighlightID: "h1", HighlightColor: "red"}
	if err := d.Send(context.Background(), it); err == nil || isPermanent(err) {
		t.Errorf("500: err = %v, want a retryable error", err)
	}
	cs.status = 400
	if err := d.Send(context.Background(), it); err == nil || !isPermanent(err) {
		t.Errorf("400: err = %v, want a permanent error", err)
	}
}
