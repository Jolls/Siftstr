package server

import (
	"errors"
	"log"
	"net/http"

	"github.com/Jolls/Siftstr/internal/highlights"
	"github.com/Jolls/Siftstr/internal/scheduler"
)

// highlightColor changes the color of the signed-in user's highlight pick for
// today. It is a plain form post, not a triage action: it does not go through
// the page's action queue and needs a connection. The pick is found by the
// user, the day and the highlight together, so a stale form or another user's
// ID matches nothing.
func (s *server) highlightColor(w http.ResponseWriter, req *http.Request, sess *session) {
	ctx := req.Context()
	uid := sess.User.ID
	day, err := s.Runs.Today(ctx, uid)
	if err != nil {
		serverError(w)
		return
	}
	res, err := s.HighlightColors.SetColor(ctx, uid, day, req.PostFormValue("highlight_id"), req.PostFormValue("color"))
	switch {
	case errors.Is(err, highlights.ErrBadColor):
		http.Error(w, "unknown highlight color", http.StatusUnprocessableEntity)
		return
	case errors.Is(err, highlights.ErrNoPick):
		http.NotFound(w, req)
		return
	case err != nil:
		serverError(w)
		return
	}
	if res.Changed {
		// The change is saved either way; the hourly briefing timer catches a file
		// that could not be rewritten now.
		if err := s.Briefing.Refresh(ctx, uid, day); err != nil {
			log.Printf("refresh briefing file after a highlight color change: %v", err)
		}
	}
	if res.Queued {
		s.kick(ctx, uid, scheduler.JobOutbox)
	}
	http.Redirect(w, req, "/today", http.StatusSeeOther)
}
