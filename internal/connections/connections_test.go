package connections

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Jolls/Siftstr/internal/store"
)

var key = bytes.Repeat([]byte{7}, 32)

type env struct {
	svc *Service
	st  *store.Store
}

func setup(t *testing.T) env {
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
	svc, err := New(st.DB(), key)
	if err != nil {
		t.Fatal(err)
	}
	return env{svc: svc, st: st}
}

func str(s string) *string { return &s }

func TestCreateAndSecretRoundTrip(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	c, err := e.svc.Create(ctx, "u1", Input{Kind: "miniflux", BaseURL: "https://miniflux.example.com", Secret: str("s3cret-token")})
	if err != nil {
		t.Fatal(err)
	}
	if !c.HasSecret || string(c.Config) != "{}" {
		t.Fatalf("conn = %+v", c)
	}
	got, err := e.svc.Secret(ctx, "u1", c.ID)
	if err != nil || got != "s3cret-token" {
		t.Fatalf("secret = %q, %v", got, err)
	}
}

func TestSecretIsNotStoredInPlaintext(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	c, _ := e.svc.Create(ctx, "u1", Input{Kind: "karakeep", Secret: str("s3cret-token")})
	var blob []byte
	_ = e.st.DB().QueryRow(`SELECT secret_enc FROM connections WHERE id = ?`, c.ID).Scan(&blob)
	if len(blob) == 0 || bytes.Contains(blob, []byte("s3cret-token")) {
		t.Fatalf("stored blob looks like plaintext: %q", blob)
	}
	// Two encryptions of the same value must differ (random nonce).
	c2, _ := e.svc.Create(ctx, "u1", Input{Kind: "metube", Secret: str("s3cret-token")})
	var blob2 []byte
	_ = e.st.DB().QueryRow(`SELECT secret_enc FROM connections WHERE id = ?`, c2.ID).Scan(&blob2)
	if bytes.Equal(blob, blob2) {
		t.Fatal("identical ciphertexts")
	}
}

// A saved secret must never come back through the listing types.
func TestConnectionHasNoSecretField(t *testing.T) {
	e := setup(t)
	c, _ := e.svc.Create(context.Background(), "u1", Input{Kind: "miniflux", Secret: str("s3cret-token")})
	typ := reflect.TypeOf(c)
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		// A HasSecret flag is fine; anything else secret-like could hold the value.
		if strings.Contains(strings.ToLower(f.Name), "secret") && !(f.Name == "HasSecret" && f.Type.Kind() == reflect.Bool) {
			t.Errorf("Connection has field %q", f.Name)
		}
	}
	if strings.Contains(fmt.Sprintf("%+v", c), "s3cret-token") {
		t.Error("secret appears in formatted connection")
	}
	list, _ := e.svc.List(context.Background(), "u1")
	b, _ := json.Marshal(list)
	if strings.Contains(string(b), "s3cret-token") {
		t.Error("secret appears in serialized list")
	}
}

func TestUpdateKeepsReplacesAndClearsSecret(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	c, _ := e.svc.Create(ctx, "u1", Input{Kind: "miniflux", BaseURL: "https://a.example.com", Secret: str("one")})

	c, err := e.svc.Update(ctx, "u1", c.ID, UpdateInput{BaseURL: str("https://b.example.com"), Config: json.RawMessage(`{"category":"news"}`)})
	if err != nil || c.BaseURL != "https://b.example.com" || !c.HasSecret {
		t.Fatalf("keep: %+v %v", c, err)
	}
	if s, _ := e.svc.Secret(ctx, "u1", c.ID); s != "one" {
		t.Fatalf("secret changed when Secret was nil: %q", s)
	}

	c, _ = e.svc.Update(ctx, "u1", c.ID, UpdateInput{Secret: str("two")})
	if s, _ := e.svc.Secret(ctx, "u1", c.ID); s != "two" {
		t.Fatalf("replace: %q", s)
	}

	c, _ = e.svc.Update(ctx, "u1", c.ID, UpdateInput{ClearSecret: true})
	if c.HasSecret {
		t.Fatal("clear left HasSecret set")
	}
	if _, err := e.svc.Secret(ctx, "u1", c.ID); !errors.Is(err, ErrNoSecret) {
		t.Fatalf("after clear: %v", err)
	}
}

// Isolation: every operation is scoped to the owner, and a foreign ID looks
// exactly like a missing one.
func TestConnectionsAreIsolatedBetweenUsers(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	c, _ := e.svc.Create(ctx, "u1", Input{Kind: "miniflux", Secret: str("s3cret-token")})

	if _, err := e.svc.Get(ctx, "u2", c.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get: %v", err)
	}
	if _, err := e.svc.Secret(ctx, "u2", c.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Secret: %v", err)
	}
	if _, err := e.svc.Update(ctx, "u2", c.ID, UpdateInput{Secret: str("hijacked")}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Update: %v", err)
	}
	if err := e.svc.Delete(ctx, "u2", c.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete: %v", err)
	}
	if list, _ := e.svc.List(ctx, "u2"); len(list) != 0 {
		t.Errorf("List leaked %+v", list)
	}
	if s, _ := e.svc.Secret(ctx, "u1", c.ID); s != "s3cret-token" {
		t.Errorf("owner's secret damaged: %q", s)
	}
}

// Copying a ciphertext onto another row or user must not decrypt.
func TestCiphertextIsBoundToRowAndOwner(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	a, _ := e.svc.Create(ctx, "u1", Input{Kind: "miniflux", Secret: str("s3cret-token")})
	b, _ := e.svc.Create(ctx, "u1", Input{Kind: "karakeep", Secret: str("other")})
	d, _ := e.svc.Create(ctx, "u2", Input{Kind: "miniflux", Secret: str("theirs")})

	for _, dst := range []struct{ user, id string }{{"u1", b.ID}, {"u2", d.ID}} {
		if _, err := e.st.DB().Exec(
			`UPDATE connections SET secret_enc = (SELECT secret_enc FROM connections WHERE id = ?) WHERE id = ?`, a.ID, dst.id); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.Secret(ctx, dst.user, dst.id); !errors.Is(err, ErrUnreadable) {
			t.Errorf("transplanted ciphertext into %s decrypted: %v", dst.id, err)
		}
	}
}

func TestWrongKeyIsUnreadableNotACrash(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	c, _ := e.svc.Create(ctx, "u1", Input{Kind: "miniflux", Secret: str("s3cret-token")})
	other, _ := New(e.st.DB(), bytes.Repeat([]byte{9}, 32))
	if _, err := other.Secret(ctx, "u1", c.ID); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("got %v", err)
	}
	// Metadata stays usable so the user can re-enter the secret.
	if got, err := other.Get(ctx, "u1", c.ID); err != nil || !got.HasSecret {
		t.Fatalf("get: %+v %v", got, err)
	}
}

func TestValidation(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	for name, in := range map[string]Input{
		"unknown kind": {Kind: "nope"},
		"bad url":      {Kind: "miniflux", BaseURL: "miniflux.example.com"},
		"ftp url":      {Kind: "miniflux", BaseURL: "ftp://x.example.com"},
		"array config": {Kind: "miniflux", Config: json.RawMessage(`[1]`)},
		"junk config":  {Kind: "miniflux", Config: json.RawMessage(`{`)},
		"userinfo url": {Kind: "miniflux", BaseURL: "https://admin:pw@x.example.com"},
		"query url":    {Kind: "miniflux", BaseURL: "https://x.example.com/?token=1"},
		"nostr http":   {Kind: "nostr", BaseURL: "https://relay.example.com"},
		"ws miniflux":  {Kind: "miniflux", BaseURL: "wss://x.example.com"},
	} {
		if _, err := e.svc.Create(ctx, "u1", in); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := New(e.st.DB(), []byte("short")); err == nil {
		t.Error("short key accepted")
	}
	if _, err := New(e.st.DB(), make([]byte, 16)); err == nil {
		t.Error("16-byte key accepted")
	}
	if _, err := e.svc.Create(ctx, "u1", Input{Kind: "nostr", BaseURL: "wss://relay.example.com"}); err != nil {
		t.Errorf("nostr wss rejected: %v", err)
	}
}

func TestPartialUpdateKeepsOtherFields(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	c, _ := e.svc.Create(ctx, "u1", Input{Kind: "miniflux", BaseURL: "https://a.example.com", Secret: str("one"), Config: json.RawMessage(`{"category":"news"}`)})
	c, err := e.svc.Update(ctx, "u1", c.ID, UpdateInput{Secret: str("")}) // blank form field
	if err != nil || c.BaseURL != "https://a.example.com" || string(c.Config) != `{"category":"news"}` || !c.HasSecret {
		t.Fatalf("partial update lost data: %+v %v", c, err)
	}
	if s, _ := e.svc.Secret(ctx, "u1", c.ID); s != "one" {
		t.Fatalf("blank secret changed it: %q", s)
	}
}

func TestListDeleteAndNoSecretConnection(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	a, _ := e.svc.Create(ctx, "u1", Input{Kind: "metube", BaseURL: "http://metube.local:8081"})
	_, _ = e.svc.Create(ctx, "u1", Input{Kind: "karakeep", Secret: str("k")})
	list, _ := e.svc.List(ctx, "u1")
	if len(list) != 2 || list[0].Kind != "karakeep" || list[1].Kind != "metube" || list[1].HasSecret {
		t.Fatalf("list = %+v", list)
	}
	if _, err := e.svc.Secret(ctx, "u1", a.ID); !errors.Is(err, ErrNoSecret) {
		t.Fatalf("no-secret conn: %v", err)
	}
	if err := e.svc.Delete(ctx, "u1", a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Get(ctx, "u1", a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}
