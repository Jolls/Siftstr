package connections

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Jolls/Siftstr/internal/dest"
)

func statusErr(code int) error { return fmt.Errorf("GET x: %w", &dest.StatusError{Code: code}) }

func TestClassify(t *testing.T) {
	long := errors.New(strings.Repeat("é", 500))
	cases := []struct {
		name         string
		err          error
		probe        bool
		state        string
		detail       string // exact, or "" to skip
		contains     string
		wantRecorded bool
	}{
		{"nil", nil, false, StateOK, "", "", true},
		{"canceled", context.Canceled, true, "", "", "", false},
		{"no secret", ErrNoSecret, false, StateAuth, "No key is saved.", "", true},
		{"unreadable", ErrUnreadable, false, StateAuth, "", "decrypted", true},
		{"401", statusErr(401), false, StateAuth, "HTTP 401", "", true},
		{"403", statusErr(403), false, StateAuth, "HTTP 403", "", true},
		{"301", statusErr(301), false, StateUnexpected, "", "redirect", true},
		{"404 background", statusErr(404), false, "", "", "", false},
		{"404 probe", statusErr(404), true, StateUnexpected, "HTTP 404", "", true},
		{"502", statusErr(502), false, StateUnexpected, "HTTP 502", "", true},
		{"400 background", statusErr(400), false, "", "", "", false},
		{"429 background", statusErr(429), false, "", "", "", false},
		{"400 probe", statusErr(400), true, StateUnexpected, "HTTP 400", "", true},
		{"unknown authority", fmt.Errorf("GET x: %w", x509.UnknownAuthorityError{}), false, StateTLS, "", "", true},
		{"record header", fmt.Errorf("GET x: %w", tls.RecordHeaderError{}), false, StateTLS, "The port does not speak HTTPS.", "", true},
		{"deadline", context.DeadlineExceeded, false, StateUnreachable, "Timed out.", "", true},
		{"refused", fmt.Errorf("GET x: %w", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}), false, StateUnreachable, "", "refused", true},
		{"dns", fmt.Errorf("GET x: %w", &net.DNSError{Err: "no such host", Name: "mf.invalid"}), false, StateUnreachable, "", "mf.invalid", true},
		{"unexpected reply", dest.UnexpectedReply("not a user"), false, StateUnexpected, "not a user", "", true},
		{"json", fmt.Errorf("decode: %w", &json.SyntaxError{}), false, StateUnexpected, "The reply is not the JSON this service sends.", "", true},
		{"item error background", errors.New("item"), false, "", "", "", false},
		{"item error probe", errors.New("item"), true, StateUnexpected, "item", "", true},
	}
	for _, tc := range cases {
		state, detail, rec := Classify(tc.err, tc.probe)
		if rec != tc.wantRecorded || state != tc.state ||
			(tc.detail != "" && detail != tc.detail) || (tc.contains != "" && !strings.Contains(detail, tc.contains)) {
			t.Errorf("%s: got (%q, %q, %v)", tc.name, state, detail, rec)
		}
	}
	_, detail, _ := Classify(long, true)
	if n := utf8.RuneCountInString(detail); n != maxDetail || !strings.HasSuffix(detail, "…") {
		t.Errorf("long detail has %d runes", n)
	}
}

var t0 = time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)

func seedStatusConn(t *testing.T, e env, user, base string) Connection {
	t.Helper()
	c, err := e.svc.Create(context.Background(), user, Input{Kind: "miniflux", BaseURL: base, Secret: str("tok-SECRET")})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func checkedAt(t *testing.T, e env, id string) (state, via, at string) {
	t.Helper()
	if err := e.st.DB().QueryRow(`SELECT COALESCE(check_state,''), check_via, COALESCE(checked_at,'') FROM connections WHERE id = ?`, id).Scan(&state, &via, &at); err != nil {
		t.Fatal(err)
	}
	return
}

func TestObserveThrottlesAndForces(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	c := seedStatusConn(t, e, "u1", "https://mf.example.com")
	at := func(d time.Duration) string { return t0.Add(d).Format(time.RFC3339) }
	step := func(via string, d time.Duration, err error, wantState, wantAt, wantVia string) {
		t.Helper()
		e.svc.Observe(ctx, "u1", c, via, t0.Add(d), err)
		state, v, got := checkedAt(t, e, c.ID)
		if state != wantState || got != wantAt || v != wantVia {
			t.Fatalf("after %s +%v: state=%q at=%q via=%q", via, d, state, got, v)
		}
	}
	step(ViaIngest, 0, nil, StateOK, at(0), ViaIngest)
	step(ViaIngest, 10*time.Minute, nil, StateOK, at(0), ViaIngest)
	step(ViaIngest, 11*time.Minute, statusErr(401), StateAuth, at(11*time.Minute), ViaIngest)
	step(ViaIngest, 12*time.Minute, nil, StateOK, at(12*time.Minute), ViaIngest)
	step(ViaIngest, 73*time.Minute, nil, StateOK, at(73*time.Minute), ViaIngest)
	step(ViaTest, 74*time.Minute, nil, StateOK, at(74*time.Minute), ViaTest)
}

func TestStatusesAreScopedToUser(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	c1 := seedStatusConn(t, e, "u1", "https://mf.example.com")
	_ = seedStatusConn(t, e, "u2", "https://mf2.example.com")
	e.svc.Observe(ctx, "u1", c1, ViaSave, t0, nil)
	got, err := e.svc.Statuses(ctx, "u1")
	if err != nil || got[c1.ID].State != StateOK || got[c1.ID].Via != ViaSave || !got[c1.ID].At.Equal(t0) {
		t.Fatalf("u1: %+v %v", got, err)
	}
	if got, _ := e.svc.Statuses(ctx, "u2"); len(got) != 0 {
		t.Errorf("u2 sees %+v", got)
	}
	// Observing u1's row as u2 changes nothing.
	e.svc.Observe(ctx, "u2", c1, ViaSave, t0.Add(time.Hour), statusErr(401))
	if state, _, _ := checkedAt(t, e, c1.ID); state != StateOK {
		t.Errorf("u2 changed u1's row: %q", state)
	}
}

func TestObserveLogsFailuresOnceWithoutSecret(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	c := seedStatusConn(t, e, "u1", "https://mf.example.com")
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	lines := func() int { return strings.Count(buf.String(), "connection check:") }

	e.svc.Observe(ctx, "u1", c, ViaSave, t0, statusErr(401))
	if lines() != 1 {
		t.Fatalf("after forced failure: %q", buf.String())
	}
	out := buf.String()
	for _, want := range []string{"miniflux", "https://mf.example.com", "auth", "HTTP 401"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q lacks %q", out, want)
		}
	}
	if strings.Contains(out, "tok-SECRET") {
		t.Error("secret logged")
	}
	e.svc.Observe(ctx, "u1", c, ViaIngest, t0.Add(time.Minute), statusErr(401))
	if lines() != 1 {
		t.Errorf("unchanged background failure logged again: %q", buf.String())
	}
	e.svc.Observe(ctx, "u1", c, ViaIngest, t0.Add(2*time.Minute), nil)
	if lines() != 1 {
		t.Errorf("success logged: %q", buf.String())
	}
	e.svc.Observe(ctx, "u1", c, ViaIngest, t0.Add(3*time.Minute), statusErr(401))
	if lines() != 2 {
		t.Errorf("changed failure not logged: %q", buf.String())
	}
}
