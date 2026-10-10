package nostr

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/coder/websocket"

	"github.com/Jolls/Siftstr/internal/connections"
	"github.com/Jolls/Siftstr/internal/store"
)

var now = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// key is a throwaway test identity.
type key struct {
	priv *btcec.PrivateKey
	pub  string
}

func newKey(t *testing.T, seed string) key {
	t.Helper()
	sum := sha256.Sum256([]byte(seed))
	priv, _ := btcec.PrivKeyFromBytes(sum[:])
	return key{priv: priv, pub: hex.EncodeToString(priv.PubKey().SerializeCompressed()[1:])}
}

func (k key) sign(t *testing.T, kind int, at time.Time, content string, tags ...[]string) Event {
	t.Helper()
	e := Event{PubKey: k.pub, CreatedAt: at.Unix(), Kind: kind, Tags: tags, Content: content}
	if e.Tags == nil {
		e.Tags = [][]string{}
	}
	e.ID = e.ComputeID()
	id, _ := hex.DecodeString(e.ID)
	sig, err := schnorr.Sign(k.priv, id)
	if err != nil {
		t.Fatal(err)
	}
	e.Sig = hex.EncodeToString(sig.Serialize())
	return e
}

type fakeFetcher struct {
	events  []Event
	relays  []string
	authors []string
	since   time.Time
	err     error
}

func (f *fakeFetcher) Fetch(_ context.Context, relays, authors []string, since time.Time) ([]Event, error) {
	f.relays, f.authors, f.since = relays, authors, since
	return f.events, f.err
}

func setup(t *testing.T) (*Ingester, *fakeFetcher, *store.Store) {
	t.Helper()
	st, err := store.OpenFile(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, u := range []string{"u1", "u2"} {
		if _, err := st.DB().Exec(`INSERT INTO users (id, username, password_hash, created_at) VALUES (?, ?, 'x', 'now')`, u, u); err != nil {
			t.Fatal(err)
		}
	}
	cs, err := connections.New(st.DB(), bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeFetcher{}
	return &Ingester{DB: st.DB(), Conns: cs, Fetch: f, Now: func() time.Time { return now }}, f, st
}

func connect(t *testing.T, g *Ingester, user string, cfg Config) {
	t.Helper()
	if _, err := g.Conns.Create(context.Background(), user, connections.Input{Kind: "nostr", Config: cfg.JSON()}); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, st *store.Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRunMirrorsSourcesAndPullsPostsOnce(t *testing.T) {
	ctx := context.Background()
	g, f, st := setup(t)
	alice, bob := newKey(t, "alice"), newKey(t, "bob")
	connect(t, g, "u1", Config{Relays: []string{"wss://relay.example.com"}, Npubs: []string{alice.pub, bob.pub}})
	at := now.Add(-time.Hour)
	f.events = []Event{
		alice.sign(t, KindProfile, at, `{"name":"alice","display_name":"Alice A"}`),
		alice.sign(t, KindNote, at, "First line of a note\nand more"),
		alice.sign(t, KindArticle, at, "# Long body", []string{"title", "A Long Read"}, []string{"d", "x"}),
		bob.sign(t, KindNote, at, "bob says hi"),
		alice.sign(t, KindNote, at, "a reply", []string{"e", strings.Repeat("a", 64)}),
	}
	res, err := g.Run(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Sources != 2 || res.Items != 3 || res.Invalid != 0 {
		t.Fatalf("result = %+v", res)
	}
	if len(f.relays) != 1 || len(f.authors) != 2 || !f.since.Equal(now.AddDate(0, 0, -14)) {
		t.Fatalf("fetch got %v %v %v", f.relays, f.authors, f.since)
	}
	var name, cat string
	if err := st.DB().QueryRow(`SELECT name, category FROM sources WHERE external_id = ?`, alice.pub).Scan(&name, &cat); err != nil {
		t.Fatal(err)
	}
	if name != "Alice A" || cat != "Nostr" {
		t.Fatalf("source = %q %q", name, cat)
	}
	if err := st.DB().QueryRow(`SELECT name FROM sources WHERE external_id = ?`, bob.pub).Scan(&name); err != nil || !strings.HasPrefix(name, "npub1") {
		t.Fatalf("bob's source name = %q, %v", name, err)
	}
	var title, excerpt, state, mt, url string
	err = st.DB().QueryRow(`SELECT title, excerpt, state, media_type, url FROM items WHERE title = 'First line of a note'`).Scan(&title, &excerpt, &state, &mt, &url)
	if err != nil {
		t.Fatal(err)
	}
	if excerpt != "First line of a note and more" || state != "pending_light" || mt != "post" || !strings.HasPrefix(url, "https://njump.me/note1") {
		t.Fatalf("note = %q %q %q %q", excerpt, state, mt, url)
	}
	if got := count(t, st, `SELECT COUNT(*) FROM items WHERE title = 'A Long Read' AND media_type = 'article'`); got != 1 {
		t.Fatal("article not mapped")
	}

	res, err = g.Run(ctx, "u1") // second run: dedupe by event ID
	if err != nil || res.Items != 0 {
		t.Fatalf("rerun = %+v, %v", res, err)
	}
	if got := count(t, st, `SELECT COUNT(*) FROM items`); got != 3 {
		t.Fatalf("items = %d", got)
	}
}

func TestRunDropsForgedStrangerAndOldEvents(t *testing.T) {
	g, f, st := setup(t)
	alice, mallory := newKey(t, "alice"), newKey(t, "mallory")
	connect(t, g, "u1", Config{Relays: []string{"wss://relay.example.com"}, Npubs: []string{alice.pub}})

	forged := alice.sign(t, KindNote, now.Add(-time.Hour), "real")
	forged.Content = "tampered" // ID no longer matches
	badSig := alice.sign(t, KindNote, now.Add(-time.Hour), "signed by someone else")
	badSig.Sig = mallory.sign(t, KindNote, now.Add(-time.Hour), "signed by someone else").Sig
	f.events = []Event{
		forged,
		badSig,
		mallory.sign(t, KindNote, now.Add(-time.Hour), "not followed"),
		alice.sign(t, KindNote, now.AddDate(0, 0, -30), "too old"),
		alice.sign(t, KindNote, now.Add(-time.Hour), "   "),
		alice.sign(t, KindNote, now.Add(-time.Hour), "kept"),
	}
	res, err := g.Run(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Items != 1 || res.Invalid != 3 || res.TooOld != 1 {
		t.Fatalf("result = %+v", res)
	}
	if got := count(t, st, `SELECT COUNT(*) FROM items WHERE title = 'kept'`); got != 1 {
		t.Fatal("good event was not stored")
	}
}

func TestRunKeepsUserSourceSettingsAndSkipsDisabled(t *testing.T) {
	g, f, st := setup(t)
	alice := newKey(t, "alice")
	connect(t, g, "u1", Config{Relays: []string{"wss://relay.example.com"}, Npubs: []string{alice.pub}})
	f.events = []Event{alice.sign(t, KindNote, now.Add(-time.Hour), "one")}
	if _, err := g.Run(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE sources SET granularity = 'digest', enabled = 0, name = 'Mine' WHERE external_id = ?`, alice.pub); err != nil {
		t.Fatal(err)
	}
	f.events = []Event{alice.sign(t, KindNote, now.Add(-time.Minute), "two")}
	if _, err := g.Run(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	var gran, name string
	var enabled bool
	if err := st.DB().QueryRow(`SELECT granularity, enabled, name FROM sources WHERE external_id = ?`, alice.pub).Scan(&gran, &enabled, &name); err != nil {
		t.Fatal(err)
	}
	if gran != "digest" || enabled || name != "Mine" {
		t.Fatalf("settings reset: %q %v %q", gran, enabled, name)
	}
	if got := count(t, st, `SELECT COUNT(*) FROM items`); got != 1 {
		t.Fatalf("a disabled source still took items: %d", got)
	}
}

func TestRunIsolatesUsers(t *testing.T) {
	ctx := context.Background()
	g, f, st := setup(t)
	alice := newKey(t, "alice")
	connect(t, g, "u1", Config{Relays: []string{"wss://relay.example.com"}, Npubs: []string{alice.pub}})
	connect(t, g, "u2", Config{Relays: []string{"wss://relay.example.com"}, Npubs: []string{alice.pub}})
	f.events = []Event{alice.sign(t, KindNote, now.Add(-time.Hour), "shared author")}

	if _, err := g.Run(ctx, "u1"); err != nil {
		t.Fatal(err)
	}
	if got := count(t, st, `SELECT COUNT(*) FROM items WHERE user_id = 'u2'`); got != 0 {
		t.Fatalf("u2 got %d items from u1's run", got)
	}
	if _, err := g.Run(ctx, "u2"); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"u1", "u2"} {
		if got := count(t, st, `SELECT COUNT(*) FROM items WHERE user_id = ?`, u); got != 1 {
			t.Fatalf("%s items = %d", u, got)
		}
		if got := count(t, st, `SELECT COUNT(*) FROM items i JOIN sources s ON s.id = i.source_id WHERE i.user_id = ? AND s.user_id != i.user_id`, u); got != 0 {
			t.Fatalf("%s has items on another user's source", u)
		}
	}
}

func TestRunWithoutConnectionOrConfig(t *testing.T) {
	g, f, _ := setup(t)
	if _, err := g.Run(context.Background(), "u1"); !errors.Is(err, ErrNoConnection) {
		t.Fatalf("err = %v", err)
	}
	connect(t, g, "u1", Config{})
	res, err := g.Run(context.Background(), "u1")
	if err != nil || res != (Result{}) || f.relays != nil {
		t.Fatalf("empty config: %+v %v, fetched %v", res, err, f.relays)
	}
}

func TestRunFetchErrorChangesNothing(t *testing.T) {
	g, f, st := setup(t)
	alice := newKey(t, "alice")
	connect(t, g, "u1", Config{Relays: []string{"wss://relay.example.com"}, Npubs: []string{alice.pub}})
	f.err = ErrNoRelay
	if _, err := g.Run(context.Background(), "u1"); !errors.Is(err, ErrNoRelay) {
		t.Fatalf("err = %v", err)
	}
	if got := count(t, st, `SELECT COUNT(*) FROM sources`); got != 0 {
		t.Fatalf("sources = %d", got)
	}
}

func TestNewConfig(t *testing.T) {
	alice := newKey(t, "alice")
	c, err := NewConfig("wss://a.example.com\nwss://a.example.com, ws://b.example.com", "nostr:"+Npub(alice.pub)+" "+alice.pub)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Relays) != 2 || len(c.Npubs) != 1 || c.Npubs[0] != alice.pub {
		t.Fatalf("config = %+v", c)
	}
	for _, bad := range [][2]string{
		{"https://a.example.com", ""},
		{"wss://user:pw@a.example.com", ""},
		{"wss://a.example.com", "npub1notvalid"},
		{"wss://a.example.com", "nsec1" + strings.Repeat("q", 58)},
	} {
		if _, err := NewConfig(bad[0], bad[1]); err == nil {
			t.Errorf("NewConfig(%q, %q) accepted", bad[0], bad[1])
		}
	}
}

func TestBech32RoundTripAndChecksum(t *testing.T) {
	// Known vector from NIP-19.
	const npub = "npub10elfcs4fr0l0r8af98jlmgdh9c8tcxjvz9qkw038js35mp4dma8qzvjptg"
	const hexKey = "7e7e9c42a91bfef19fa929e5fda1b72e0ebc1a4c1141673e2794234d86addf4e"
	got, err := ParsePubKey(npub)
	if err != nil || got != hexKey {
		t.Fatalf("ParsePubKey = %q, %v", got, err)
	}
	if Npub(hexKey) != npub {
		t.Fatalf("Npub = %q", Npub(hexKey))
	}
	if _, err := ParsePubKey(npub[:len(npub)-1] + "q"); err == nil {
		t.Fatal("bad checksum accepted")
	}
	if !strings.HasPrefix(Note(strings.Repeat("ab", 32)), "note1") {
		t.Fatal("note prefix")
	}
}

func TestEventIDKnownSerialization(t *testing.T) {
	// Characters NIP-01 leaves unescaped must not be escaped.
	e := Event{PubKey: strings.Repeat("0", 64), CreatedAt: 1, Kind: 1, Tags: [][]string{{"t", "<a&b>"}}, Content: "x<y>& \"\n"}
	sum := sha256.Sum256([]byte(`[0,"` + e.PubKey + `",1,1,[["t","<a&b>"]],"x<y>&` + " " + `\"\n"]`))
	if e.ComputeID() != hex.EncodeToString(sum[:]) {
		t.Fatal("serialization differs from NIP-01")
	}
}

// relay serves canned events to one REQ, then EOSE.
func relay(t *testing.T, events []Event, reqs chan<- []json.RawMessage) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		_, data, err := c.Read(r.Context())
		if err != nil {
			return
		}
		var req []json.RawMessage
		_ = json.Unmarshal(data, &req)
		reqs <- req
		var sub string
		_ = json.Unmarshal(req[1], &sub)
		for _, e := range events {
			b, _ := json.Marshal([]any{"EVENT", sub, e})
			_ = c.Write(r.Context(), websocket.MessageText, b)
		}
		b, _ := json.Marshal([]any{"EOSE", sub})
		_ = c.Write(r.Context(), websocket.MessageText, b)
		_, _, _ = c.Read(r.Context()) // CLOSE
	}))
}

func TestRelayFetcherReadsUntilEOSEAndSkipsDeadRelays(t *testing.T) {
	alice := newKey(t, "alice")
	want := alice.sign(t, KindNote, now, "hello")
	reqs := make(chan []json.RawMessage, 1)
	srv := relay(t, []Event{want}, reqs)
	defer srv.Close()
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := "ws" + strings.TrimPrefix(dead.URL, "http")
	dead.Close()

	f := RelayFetcher{Timeout: 5 * time.Second}
	got, err := f.Fetch(context.Background(), []string{deadURL, "ws" + strings.TrimPrefix(srv.URL, "http")}, []string{alice.pub}, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != want.ID || got[0].Verify() != nil {
		t.Fatalf("got %+v", got)
	}
	req := <-reqs
	var filter struct {
		Authors []string `json:"authors"`
		Kinds   []int    `json:"kinds"`
		Since   int64    `json:"since"`
	}
	_ = json.Unmarshal(req[2], &filter)
	if string(req[0]) != `"REQ"` || len(filter.Authors) != 1 || len(filter.Kinds) != 3 || filter.Since != now.Add(-time.Hour).Unix() {
		t.Fatalf("REQ = %s", req)
	}

	_, err = f.Fetch(context.Background(), []string{deadURL}, []string{alice.pub}, time.Time{})
	if !errors.Is(err, ErrNoRelay) {
		t.Fatalf("all-dead err = %v", err)
	}
}
