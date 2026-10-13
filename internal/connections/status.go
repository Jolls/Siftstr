package connections

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"time"
	"unicode/utf8"

	"github.com/Jolls/Siftstr/internal/dest"
)

// Check states (connections.check_state). An empty state means never checked.
const (
	StateOK          = "ok"
	StateUnreachable = "unreachable"
	StateTLS         = "tls"
	StateAuth        = "auth"
	StateUnexpected  = "unexpected"
)

// Where a check came from (connections.check_via).
const (
	ViaSave   = "save"
	ViaTest   = "test"
	ViaIngest = "ingest"
	ViaOutbox = "outbox"
)

// StatusRefresh is how old a stored result may get before an unchanged
// background observation writes it again.
const StatusRefresh = time.Hour

const maxDetail = 200 // runes

// Status is a connection's last check. It never holds a secret.
type Status struct {
	State, Detail, Via string
	At                 time.Time
}

// Classify turns the error of a call to an upstream into a check state and a
// short reason. probe is true for a save or test, where any failure is about
// the connection. In the background, errors about a single item (a 400, 422 or
// 429, or an error that is not about the connection) are not recorded.
func Classify(err error, probe bool) (state, detail string, record bool) {
	state, detail, record = classify(err, probe)
	return state, truncate(detail), record
}

func classify(err error, probe bool) (state, detail string, record bool) {
	if err == nil {
		return StateOK, "", true
	}
	if errors.Is(err, context.Canceled) {
		return "", "", false
	}
	if errors.Is(err, ErrNoSecret) {
		return StateAuth, "No key is saved.", true
	}
	if errors.Is(err, ErrUnreadable) {
		return StateAuth, "The saved key cannot be decrypted; enter it again.", true
	}
	var se *dest.StatusError
	if errors.As(err, &se) {
		switch c := se.Code; {
		case c == 401 || c == 403:
			return StateAuth, fmt.Sprintf("HTTP %d", c), true
		case c >= 300 && c < 400:
			return StateUnexpected, fmt.Sprintf("HTTP %d redirect: save the final address (for example https://)", c), true
		case c >= 500:
			return StateUnexpected, fmt.Sprintf("HTTP %d", c), true
		case probe: // includes 404: in the background it can be one deleted item
			return StateUnexpected, fmt.Sprintf("HTTP %d", c), true
		}
		return "", "", false
	}
	var ua x509.UnknownAuthorityError
	var he x509.HostnameError
	var ci x509.CertificateInvalidError
	var rh tls.RecordHeaderError
	var cv *tls.CertificateVerificationError
	switch {
	case errors.As(err, &ua):
		return StateTLS, ua.Error(), true
	case errors.As(err, &he):
		return StateTLS, he.Error(), true
	case errors.As(err, &ci):
		return StateTLS, ci.Error(), true
	case errors.As(err, &rh):
		return StateTLS, "The port does not speak HTTPS.", true
	case errors.As(err, &cv):
		return StateTLS, cv.Err.Error(), true
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return StateUnreachable, "Timed out.", true
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return StateUnreachable, dns.Error(), true
	}
	var op *net.OpError
	if errors.As(err, &op) {
		return StateUnreachable, op.Error(), true
	}
	if errors.Is(err, dest.ErrUnexpectedReply) {
		// The innermost message is the reason; the wrapping adds only context.
		for e := err; e != nil; e = errors.Unwrap(e) {
			if x, ok := e.(interface{ Is(error) bool }); ok && x.Is(dest.ErrUnexpectedReply) {
				return StateUnexpected, e.Error(), true
			}
		}
		return StateUnexpected, err.Error(), true
	}
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	if errors.As(err, &syn) || errors.As(err, &typ) {
		return StateUnexpected, "The reply is not the JSON this service sends.", true
	}
	if probe {
		return StateUnexpected, err.Error(), true
	}
	return "", "", false
}

func truncate(s string) string {
	if utf8.RuneCountInString(s) <= maxDetail {
		return s
	}
	r := []rune(s)
	return string(r[:maxDetail-1]) + "…"
}

// Observe records the outcome of a call to c's upstream (nil err means it
// worked) and returns what it classified. A save or test always writes; a
// background call writes only when the result changed or the stored one is
// older than StatusRefresh, so a busy upstream does not cost a write per
// call. It logs one line when a check fails and it was forced or changed. It
// never takes, stores or logs a secret, and a database error is logged, not
// returned: a status must never break the work that produced it.
func (s *Service) Observe(ctx context.Context, userID string, c Connection, via string, now time.Time, err error) Status {
	force := via == ViaSave || via == ViaTest
	state, detail, record := Classify(err, force)
	if !record {
		return Status{}
	}
	st := Status{State: state, Detail: detail, Via: via, At: now.UTC()}
	var prevState, prevDetail, prevAt string
	qerr := s.db.QueryRowContext(ctx, `SELECT COALESCE(check_state, ''), check_detail, COALESCE(checked_at, '') FROM connections WHERE id = ? AND user_id = ?`,
		c.ID, userID).Scan(&prevState, &prevDetail, &prevAt)
	if errors.Is(qerr, sql.ErrNoRows) {
		return Status{}
	}
	if qerr != nil {
		log.Printf("connection check: record %s status: %v", c.Kind, qerr)
		return st
	}
	changed := prevState != state || prevDetail != detail
	stale := true
	if t, perr := time.Parse(time.RFC3339, prevAt); perr == nil {
		stale = !t.After(now.Add(-StatusRefresh))
	}
	if force || changed || stale {
		if _, werr := s.db.ExecContext(ctx, `UPDATE connections SET check_state = ?, check_detail = ?, check_via = ?, checked_at = ? WHERE id = ? AND user_id = ?`,
			state, detail, via, st.At.Format(time.RFC3339), c.ID, userID); werr != nil {
			log.Printf("connection check: record %s status: %v", c.Kind, werr)
		}
	}
	if state != StateOK && (force || changed) {
		log.Printf("connection check: %s at %s failed (%s): %s: %s", c.Kind, dest.Redact(c.BaseURL), via, state, detail)
	}
	return st
}

// Statuses returns the last check of each of userID's checked connections,
// keyed by connection ID.
func (s *Service) Statuses(ctx context.Context, userID string) (map[string]Status, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, check_state, check_detail, check_via, COALESCE(checked_at, '') FROM connections WHERE user_id = ? AND check_state IS NOT NULL`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Status{}
	for rows.Next() {
		var id, at string
		var st Status
		if err := rows.Scan(&id, &st.State, &st.Detail, &st.Via, &at); err != nil {
			return nil, err
		}
		st.At, _ = time.Parse(time.RFC3339, at)
		out[id] = st
	}
	return out, rows.Err()
}
