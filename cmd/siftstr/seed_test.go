package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jolls/Siftstr/internal/config"
	"github.com/Jolls/Siftstr/internal/connections"
	"github.com/Jolls/Siftstr/internal/store"
)

type seedEnv struct {
	db    *sql.DB
	conns *connections.Service
	logs  []string
}

func (e *seedEnv) logf(f string, a ...any) { e.logs = append(e.logs, fmt.Sprintf(f, a...)) }

func newSeedEnv(t *testing.T) *seedEnv {
	t.Helper()
	st, err := store.OpenFile(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	conns, err := connections.New(st.DB(), bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return &seedEnv{db: st.DB(), conns: conns}
}

func (e *seedEnv) user(t *testing.T, name, role, created string, disabled int) string {
	t.Helper()
	id := "usr_" + name
	if _, err := e.db.Exec(`INSERT INTO users (id, username, password_hash, role, disabled, created_at) VALUES (?, ?, 'x', ?, ?, ?)`, id, name, role, disabled, created); err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *seedEnv) rows(t *testing.T, id string) int {
	t.Helper()
	list, err := e.conns.List(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return len(list)
}

func (e *seedEnv) seed(adminUser string, seeds ...config.SeedConnection) error {
	return seedConnections(context.Background(), e.db, config.Config{AdminUser: adminUser, Seeds: seeds}, e.conns, e.logf)
}

var mfSeed = config.SeedConnection{Kind: "miniflux", URL: "http://mf:8080", APIKey: "k"}

func TestSeedConnectionsTargetsNamedAdmin(t *testing.T) {
	e := newSeedEnv(t)
	alice := e.user(t, "alice", "admin", "2026-01-01", 0)
	bob := e.user(t, "bob", "admin", "2026-02-01", 0)
	if err := e.seed("Bob", mfSeed); err != nil {
		t.Fatal(err)
	}
	if e.rows(t, bob) != 1 || e.rows(t, alice) != 0 {
		t.Fatal("wrong user seeded")
	}
}

func TestSeedConnectionsFallsBackToOldestAdmin(t *testing.T) {
	e := newSeedEnv(t)
	carol := e.user(t, "carol", "user", "2025-01-01", 0)
	alice := e.user(t, "alice", "admin", "2026-01-01", 0)
	bob := e.user(t, "bob", "admin", "2026-02-01", 0)
	if err := e.seed("", mfSeed); err != nil {
		t.Fatal(err)
	}
	if e.rows(t, alice) != 1 || e.rows(t, bob) != 0 || e.rows(t, carol) != 0 {
		t.Fatal("wrong user seeded")
	}
}

func TestSeedConnectionsSkipsMissingOrDisabledUser(t *testing.T) {
	e := newSeedEnv(t)
	alice := e.user(t, "alice", "admin", "2026-01-01", 1)
	if err := e.seed("ghost", mfSeed); err != nil {
		t.Fatal(err)
	}
	if err := e.seed("alice", mfSeed); err != nil {
		t.Fatal(err)
	}
	if e.rows(t, alice) != 0 || len(e.logs) != 2 {
		t.Fatalf("rows=%d logs=%v", e.rows(t, alice), e.logs)
	}
}

func TestSeedConnectionsLogsNoSecretOrURL(t *testing.T) {
	e := newSeedEnv(t)
	e.user(t, "alice", "admin", "2026-01-01", 0)
	if err := e.seed("alice", config.SeedConnection{Kind: "karakeep", URL: "https://kk.example.com", APIKey: "kk-SECRET"}); err != nil {
		t.Fatal(err)
	}
	if len(e.logs) == 0 {
		t.Fatal("nothing logged")
	}
	for _, l := range e.logs {
		if strings.Contains(l, "kk-SECRET") || strings.Contains(l, "kk.example.com") || !strings.Contains(l, "karakeep") || !strings.Contains(l, "alice") {
			t.Errorf("log line %q", l)
		}
	}
}

func TestSeedConnectionsNoSeedsTouchesNothing(t *testing.T) {
	e := newSeedEnv(t)
	alice := e.user(t, "alice", "admin", "2026-01-01", 0)
	if _, err := e.conns.Create(context.Background(), alice, connections.Input{Kind: "miniflux", BaseURL: "http://old:1"}); err != nil {
		t.Fatal(err)
	}
	if err := e.seed("alice"); err != nil {
		t.Fatal(err)
	}
	list, _ := e.conns.List(context.Background(), alice)
	if len(e.logs) != 0 || len(list) != 1 || list[0].BaseURL != "http://old:1" {
		t.Fatalf("%v %+v", e.logs, list)
	}
}
