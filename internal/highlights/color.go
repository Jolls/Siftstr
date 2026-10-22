package highlights

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Jolls/Siftstr/internal/dest"
	kk "github.com/Jolls/Siftstr/internal/dest/karakeep"
	"github.com/Jolls/Siftstr/internal/ingest"
	"github.com/Jolls/Siftstr/internal/outbox"
)

// ErrBadColor means the color is not one of Karakeep's highlight colors.
var ErrBadColor = errors.New("not one of Karakeep's highlight colors")

// ErrNoPick means the user has no such highlight pick for that day.
var ErrNoPick = errors.New("no such highlight pick")

// opKarakeep is the outbox op for Karakeep (writeback.Karakeep, which this
// package does not import).
const opKarakeep = "karakeep"

// Colors changes the color of a day's highlight pick and queues the Karakeep update.
type Colors struct {
	DB  *sql.DB
	Now func() time.Time
}

// ColorResult says what SetColor did.
type ColorResult struct {
	Changed bool // the stored color is different now
	Queued  bool // an update for Karakeep was queued
}

// SetColor changes the color of userID's pick for day, and queues the matching
// update to Karakeep in the same transaction, so one never happens without the
// other. It is not a triage action: there is no hold and no undo, and changing
// the color back is another call. Only the user's own pick for that day and
// highlight matches, so a stale form or another user's ID is ErrNoPick.
func (c *Colors) SetColor(ctx context.Context, userID, day, highlightID, color string) (ColorResult, error) {
	if !kk.IsHighlightColor(color) {
		return ColorResult{}, ErrBadColor
	}
	tx, err := c.DB.BeginTx(ctx, nil)
	if err != nil {
		return ColorResult{}, err
	}
	defer tx.Rollback()

	var stored string
	err = tx.QueryRowContext(ctx, `SELECT color FROM highlight_picks WHERE user_id = ? AND day = ? AND highlight_id = ?`,
		userID, day, highlightID).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return ColorResult{}, ErrNoPick
	}
	if err != nil {
		return ColorResult{}, err
	}
	if stored == color {
		return ColorResult{}, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE highlight_picks SET color = ? WHERE user_id = ? AND day = ? AND highlight_id = ?`,
		color, userID, day, highlightID); err != nil {
		return ColorResult{}, err
	}

	res := ColorResult{Changed: true}
	// Without a Karakeep connection there is nothing to tell: the stored color
	// still changes.
	conn, err := ingest.KarakeepConnection(ctx, tx, userID)
	if err != nil {
		return ColorResult{}, err
	}
	if conn != "" {
		payload, err := json.Marshal(dest.Item{
			ID: highlightID, Verb: dest.VerbHighlightColor, HighlightID: highlightID, HighlightColor: color,
			Title: "Highlight color: " + kk.HighlightColorLabel(color),
		})
		if err != nil {
			return ColorResult{}, err
		}
		// No dedupe key: each change is its own entry, so changing back to an
		// earlier color is not dropped. The sender sends the color stored then.
		if _, err := outbox.Enqueue(ctx, tx, c.Now(), outbox.Entry{UserID: userID, ConnectionID: conn, Op: opKarakeep, Payload: payload}, ""); err != nil {
			return ColorResult{}, fmt.Errorf("queue highlight color: %w", err)
		}
		res.Queued = true
	}
	if err := tx.Commit(); err != nil {
		return ColorResult{}, err
	}
	return res, nil
}
