// Package triage is the item state machine and the action log. Actions are
// stored as they arrive and their upstream effects are held for a grace
// period, during which an undo cancels them (ARCHITECTURE.md sections 5 and 7).
package triage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"
)

// DefaultHold is how long an action's effects are held for undo.
const DefaultHold = 15 * time.Minute

// maxSkew caps how far ahead of the server a client timestamp may be, so a
// wrong clock cannot win every last-write-wins comparison.
const maxSkew = 5 * time.Minute

// Action kinds and subject types.
const (
	Archive = "archive"
	Promote = "promote"
	Keep    = "keep"
	Undo    = "undo"

	SubjectItem   = "item"
	SubjectDigest = "digest"
)

// Statuses reported for an action.
const (
	Applied   = "applied"
	Duplicate = "duplicate" // the action_id was seen before; nothing changed
	Rejected  = "rejected"
)

// Rejection reasons.
const (
	ReasonUnknownSubject = "unknown subject"
	ReasonBadAction      = "invalid action"
	ReasonNotActionable  = "not actionable in this state"
	ReasonPromoteOff     = "promote is not available for this item"
	ReasonOlder          = "a newer action already applies"
	ReasonSent           = "already sent upstream"
	ReasonHoldEnded      = "undo window has ended"
	ReasonNothingToUndo  = "nothing to undo"
	ReasonIDInUse        = "action id in use"
)

// Action is one client action.
type Action struct {
	ActionID       string    `json:"action_id"`
	SubjectType    string    `json:"subject_type"` // empty means item
	SubjectID      string    `json:"subject_id"`
	Kind           string    `json:"kind"`
	TargetActionID string    `json:"target_action_id,omitempty"`
	At             time.Time `json:"at"`
}

// Result is the outcome of one action, with the subject's current state so the
// client can reconcile its badge.
type Result struct {
	ActionID    string `json:"action_id"`
	Status      string `json:"status"`
	Reason      string `json:"reason,omitempty"`
	SubjectType string `json:"subject_type"`
	SubjectID   string `json:"subject_id"`
	State       string `json:"state"`
}

// Service applies actions. Every method is scoped to one user.
type Service struct {
	DB  *sql.DB
	Now func() time.Time
	// OnRelease, when set, is called inside ReleaseDue's transaction with the
	// actions being released. The write-back planner queues their effects here.
	OnRelease func(ctx context.Context, tx *sql.Tx, userID string, released []Released) error
}

// New returns a Service using the real clock.
func New(db *sql.DB) *Service { return &Service{DB: db, Now: time.Now} }

// stampLayout is fixed width, so stored timestamps sort correctly as text.
// Unlike the RFC3339 used elsewhere it keeps nanoseconds, which the hold and
// last-write-wins comparisons need.
const stampLayout = "2006-01-02T15:04:05.000000000Z"

func stamp(t time.Time) string { return t.UTC().Format(stampLayout) }

// holdFor returns the user's undo window.
func holdFor(ctx context.Context, tx *sql.Tx, userID string) time.Duration {
	var v string
	err := tx.QueryRowContext(ctx, `SELECT value FROM user_settings WHERE user_id = ? AND key = 'undo_hold_minutes'`, userID).Scan(&v)
	if err == nil {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return time.Duration(n) * time.Minute
		}
	}
	return DefaultHold
}

// Apply applies a batch for userID in client-timestamp order. Replaying a
// batch is harmless: an action_id already stored returns its recorded outcome.
// Results are in the same order as the input.
func (s *Service) Apply(ctx context.Context, userID string, in []Action) ([]Result, error) {
	now := s.Now().UTC()
	// Normalise first, then order, so a missing or far-future timestamp sorts
	// where it is treated as being (now).
	acts := make([]Action, len(in))
	for i, a := range in {
		if a.SubjectType == "" {
			a.SubjectType = SubjectItem
		}
		if a.At.IsZero() || a.At.After(now.Add(maxSkew)) {
			a.At = now
		}
		acts[i] = a
	}
	order := make([]int, len(acts))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(x, y int) bool { return acts[order[x]].At.Before(acts[order[y]].At) })

	out := make([]Result, len(acts))
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	hold := holdFor(ctx, tx, userID)
	for _, i := range order {
		r, err := s.one(ctx, tx, userID, acts[i], now, hold)
		if err != nil {
			return nil, fmt.Errorf("action %s: %w", acts[i].ActionID, err)
		}
		out[i] = r
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

type subject struct {
	typ, id, state, maxDepth string
	promotedAt               sql.NullString
}

func loadSubject(ctx context.Context, tx *sql.Tx, userID, typ, id string) (subject, error) {
	sub := subject{typ: typ, id: id}
	switch typ {
	case SubjectItem:
		err := tx.QueryRowContext(ctx,
			`SELECT i.state, i.promoted_at, s.max_depth FROM items i
			 JOIN sources s ON s.user_id = i.user_id AND s.id = i.source_id WHERE i.user_id = ? AND i.id = ?`,
			userID, id).Scan(&sub.state, &sub.promotedAt, &sub.maxDepth)
		return sub, err
	case SubjectDigest:
		err := tx.QueryRowContext(ctx,
			`SELECT d.state, s.max_depth FROM digests d
			 JOIN sources s ON s.user_id = d.user_id AND s.id = d.source_id WHERE d.user_id = ? AND d.id = ?`,
			userID, id).Scan(&sub.state, &sub.maxDepth)
		return sub, err
	}
	return sub, sql.ErrNoRows
}

// stateOf is a subject's current state, or "" if the user has no such subject.
func stateOf(ctx context.Context, tx *sql.Tx, userID, typ, id string) string {
	sub, err := loadSubject(ctx, tx, userID, typ, id)
	if err != nil {
		return ""
	}
	return sub.state
}

type stored struct {
	id, userID, typ, subjectID, kind, status, reason string
	target, prev                                     sql.NullString
	clientAt, releaseAt                              time.Time
}

const storedCols = `action_id, user_id, subject_type, subject_id, kind, target_action_id, prev_state, status, reason, client_at, release_at`

func scanStored(row *sql.Row) (stored, error) {
	var st stored
	var c, r string
	err := row.Scan(&st.id, &st.userID, &st.typ, &st.subjectID, &st.kind, &st.target, &st.prev, &st.status, &st.reason, &c, &r)
	st.clientAt, _ = time.Parse(time.RFC3339Nano, c)
	st.releaseAt, _ = time.Parse(time.RFC3339Nano, r)
	return st, err
}

func (s *Service) one(ctx context.Context, tx *sql.Tx, userID string, a Action, now time.Time, hold time.Duration) (Result, error) {
	res := Result{ActionID: a.ActionID, SubjectType: a.SubjectType, SubjectID: a.SubjectID}
	reject := func(reason string) (Result, error) {
		res.Status, res.Reason = Rejected, reason
		res.State = stateOf(ctx, tx, userID, a.SubjectType, a.SubjectID)
		return res, s.record(ctx, tx, userID, a, now, now, "", Rejected, reason)
	}

	// Replay: answer from the record, with the subject's state as it is now.
	prior, err := scanStored(tx.QueryRowContext(ctx, `SELECT `+storedCols+` FROM actions WHERE action_id = ?`, a.ActionID))
	if err == nil {
		if prior.userID != userID { // never reveal or touch another user's row
			res.Status, res.Reason = Rejected, ReasonIDInUse
			return res, nil
		}
		res.Status = Duplicate
		res.SubjectType, res.SubjectID = prior.typ, prior.subjectID
		res.State = stateOf(ctx, tx, userID, prior.typ, prior.subjectID)
		if prior.status == Rejected {
			res.Status, res.Reason = Rejected, prior.reason
		}
		return res, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return res, err
	}

	if a.ActionID == "" || (a.SubjectType != SubjectItem && a.SubjectType != SubjectDigest) || (a.Kind != Archive && a.Kind != Promote && a.Kind != Keep && a.Kind != Undo) {
		res.Status, res.Reason = Rejected, ReasonBadAction
		return res, nil // not stored: there is no usable key
	}
	sub, err := loadSubject(ctx, tx, userID, a.SubjectType, a.SubjectID)
	if errors.Is(err, sql.ErrNoRows) {
		return reject(ReasonUnknownSubject)
	} else if err != nil {
		return res, err
	}
	res.State = sub.state

	if a.Kind == Undo {
		return s.undo(ctx, tx, userID, a, sub, now, res, reject)
	}

	// The newest held action on this subject is what its state came from. A
	// later action replaces it (last write wins); an older one loses.
	cur, err := latestHeld(ctx, tx, userID, sub, now)
	if err != nil {
		return res, err
	}
	effective := sub.state
	if cur != nil && sub.state != resultOf(*cur) {
		// The state moved on since that action (a summary arrived), so it
		// can no longer be taken back.
		return reject(ReasonSent)
	}
	if cur != nil {
		if a.At.Before(cur.clientAt) {
			return reject(ReasonOlder)
		}
		effective = cur.prev.String
	} else if sub.state == "archived" || sub.state == "kept" || sub.state == "pending_deep" {
		return reject(ReasonSent) // its effects were already released
	}
	next, reason := transition(sub, effective, a.Kind)
	if reason != "" {
		return reject(reason)
	}
	if cur != nil {
		if err := revert(ctx, tx, userID, sub, *cur, now); err != nil {
			return res, err
		}
	}
	if err := setState(ctx, tx, userID, sub, effective, next, now); err != nil {
		return res, err
	}
	if err := s.record(ctx, tx, userID, a, now, now.Add(hold), effective, "held", ""); err != nil {
		return res, err
	}
	res.Status, res.State = Applied, next
	return res, nil
}

// resultOf is the state a stored action left its subject in.
func resultOf(a stored) string {
	switch a.kind {
	case Archive:
		return "archived"
	case Keep:
		return "kept"
	case Promote:
		if a.prev.String == "deep" {
			return "kept"
		}
		return "pending_deep"
	}
	return ""
}

// transition returns the state an action leads to from state, or a reason it
// cannot apply. It reads only the subject's settings, never its source type.
func transition(sub subject, state, kind string) (string, string) {
	if state != "light" && state != "deep" {
		return "", ReasonNotActionable
	}
	switch kind {
	case Archive:
		return "archived", ""
	case Keep:
		return "kept", ""
	case Promote:
		if sub.typ == SubjectDigest || (state == "light" && sub.maxDepth == "light") {
			return "", ReasonPromoteOff
		}
		if state == "deep" {
			return "kept", "" // at this stage promote and keep are the same
		}
		if sub.promotedAt.Valid && sub.promotedAt.String != "" {
			return "", ReasonPromoteOff // allowed once per item
		}
		return "pending_deep", ""
	}
	return "", ReasonBadAction
}

// latestHeld returns the newest action on sub whose hold is still open. A hold
// that has run out counts as released even before the release timer marks it,
// so correctness never depends on the timer's timing.
func latestHeld(ctx context.Context, tx *sql.Tx, userID string, sub subject, now time.Time) (*stored, error) {
	st, err := scanStored(tx.QueryRowContext(ctx,
		`SELECT `+storedCols+` FROM actions WHERE user_id = ? AND subject_type = ? AND subject_id = ?
		 AND status = 'held' AND kind != 'undo' AND release_at > ? ORDER BY client_at DESC, received_at DESC LIMIT 1`,
		userID, sub.typ, sub.id, stamp(now)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &st, nil
}

func (s *Service) undo(ctx context.Context, tx *sql.Tx, userID string, a Action, sub subject, now time.Time,
	res Result, reject func(string) (Result, error)) (Result, error) {
	if a.TargetActionID == "" {
		return reject(ReasonBadAction)
	}
	target, err := scanStored(tx.QueryRowContext(ctx, `SELECT `+storedCols+` FROM actions WHERE action_id = ? AND user_id = ?`,
		a.TargetActionID, userID))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (target.typ != sub.typ || target.subjectID != sub.id || target.kind == Undo)) {
		return reject(ReasonNothingToUndo)
	} else if err != nil {
		return res, err
	}
	switch {
	case target.status == "cancelled" || target.status == Rejected:
		return reject(ReasonNothingToUndo) // already undone or replaced
	case target.status != "held" || !now.Before(target.releaseAt):
		return reject(ReasonHoldEnded)
	}
	if cur, err := latestHeld(ctx, tx, userID, sub, now); err != nil {
		return res, err
	} else if cur == nil || cur.id != target.id {
		return reject(ReasonOlder) // a newer action now decides the state
	} else if sub.state != resultOf(*cur) {
		return reject(ReasonHoldEnded) // the state moved on; nothing to restore
	}
	if err := revert(ctx, tx, userID, sub, target, now); err != nil {
		return res, err
	}
	// An undo has no upstream effect, so it is stored as already done.
	if err := s.record(ctx, tx, userID, a, now, now, "", "released", ""); err != nil {
		return res, err
	}
	res.Status, res.State = Applied, target.prev.String
	return res, nil
}

// revert cancels a held action and puts the subject back as it was.
func revert(ctx context.Context, tx *sql.Tx, userID string, sub subject, held stored, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE actions SET status = 'cancelled' WHERE action_id = ? AND user_id = ?`, held.id, userID); err != nil {
		return err
	}
	return setState(ctx, tx, userID, sub, sub.state, held.prev.String, now)
}

// setState moves a subject, and a digest's children, from one state to
// another. Leaving pending_deep for light clears promoted_at so an undone
// promote does not spend the once-only rule.
func setState(ctx context.Context, tx *sql.Tx, userID string, sub subject, from, to string, now time.Time) error {
	ts := now.UTC().Format(time.RFC3339) // items use second precision like the rest of the app
	switch sub.typ {
	case SubjectItem:
		promoted := "promoted_at" // unchanged
		switch {
		case to == "pending_deep":
			promoted = "?"
		case from == "pending_deep":
			promoted = "NULL"
		}
		args := []any{to, ts}
		if to == "pending_deep" {
			args = append(args, ts)
		}
		_, err := tx.ExecContext(ctx, `UPDATE items SET state = ?, updated_at = ?, promoted_at = `+promoted+` WHERE user_id = ? AND id = ?`,
			append(args, userID, sub.id)...)
		return err
	case SubjectDigest:
		if _, err := tx.ExecContext(ctx, `UPDATE digests SET state = ? WHERE user_id = ? AND id = ?`, to, userID, sub.id); err != nil {
			return err
		}
		// Children follow: archived or kept with the digest, light again on undo.
		childFrom := "light"
		if to == "light" {
			childFrom = from
		}
		_, err := tx.ExecContext(ctx, `UPDATE items SET state = ?, updated_at = ? WHERE user_id = ? AND digest_id = ? AND state = ?`,
			to, ts, userID, sub.id, childFrom)
		return err
	}
	return nil
}

func (s *Service) record(ctx context.Context, tx *sql.Tx, userID string, a Action, received, release time.Time, prev, status, reason string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO actions (action_id, user_id, subject_type, subject_id, kind, target_action_id, prev_state, client_at, received_at, release_at, status, reason)
		 VALUES (?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?, ?)`,
		a.ActionID, userID, a.SubjectType, a.SubjectID, a.Kind, a.TargetActionID, prev, stamp(a.At), stamp(received), stamp(release), status, reason)
	return err
}

// Released is an action whose hold has ended. Outcome is the state it left
// its subject in (archived, kept or pending_deep); Prev is the state before.
type Released struct {
	ActionID    string
	SubjectType string
	SubjectID   string
	Kind        string
	Prev        string
	Outcome     string
}

// ReleaseDue marks userID's held actions whose hold has ended as released and
// returns their IDs. When OnRelease is set it runs in the same transaction,
// so an action is released if and only if its upstream effects were queued;
// a failure rolls both back and the next tick tries again. Held actions live
// in SQLite, so a restart loses nothing.
func (s *Service) ReleaseDue(ctx context.Context, userID string) ([]string, error) {
	return s.release(ctx, userID, stamp(s.Now().UTC()))
}

// ReleaseNow releases all of userID's held actions without waiting for their
// holds to end. It is the same release as ReleaseDue, so the effects are
// queued in the outbox exactly as they would be later. Undo is no longer
// possible for these actions afterwards.
func (s *Service) ReleaseNow(ctx context.Context, userID string) ([]string, error) {
	return s.release(ctx, userID, "9999")
}

// release releases held actions whose release_at is at or before cutoff.
func (s *Service) release(ctx context.Context, userID, cutoff string) ([]string, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx,
		`UPDATE actions SET status = 'released' WHERE user_id = ? AND status = 'held' AND release_at <= ?
		 RETURNING action_id, subject_type, subject_id, kind, COALESCE(prev_state, '')`,
		userID, cutoff)
	if err != nil {
		return nil, err
	}
	var rel []Released
	for rows.Next() {
		var r Released
		if err := rows.Scan(&r.ActionID, &r.SubjectType, &r.SubjectID, &r.Kind, &r.Prev); err != nil {
			rows.Close()
			return nil, err
		}
		r.Outcome = resultOf(stored{kind: r.Kind, prev: sql.NullString{String: r.Prev, Valid: true}})
		rel = append(rel, r)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	sort.Slice(rel, func(i, j int) bool { return rel[i].ActionID < rel[j].ActionID })
	if s.OnRelease != nil && len(rel) > 0 {
		if err := s.OnRelease(ctx, tx, userID, rel); err != nil {
			return nil, fmt.Errorf("queue effects: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	due := make([]string, len(rel))
	for i, r := range rel {
		due[i] = r.ActionID
	}
	return due, nil
}
