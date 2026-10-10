package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/Jolls/Siftstr/internal/auth"
	"github.com/Jolls/Siftstr/internal/briefing"
	"github.com/Jolls/Siftstr/internal/runs"
)

// maxSummariesBody bounds a summary submission. MaxItemsPerRun summaries of
// MaxSummaryLen characters each fit well inside it.
const maxSummariesBody = 8 << 20

// createRun starts a run: carryover, digest bundling, then the work package.
func (s *server) createRun(w http.ResponseWriter, req *http.Request, u auth.User) {
	wp, err := s.Runs.Create(req.Context(), u.ID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Yesterday's file is closed with final outcomes now that carryover has
	// run, before today's file is written when summaries arrive. A file that
	// cannot be written never fails the run.
	if err := s.Briefing.Finalize(req.Context(), u.ID, wp.Day); err != nil {
		log.Printf("briefing: close earlier days for user %s: %v", u.ID, err)
	}
	writeJSON(w, http.StatusCreated, wp)
}

// submitSummaries stores the summaries for one of the caller's runs.
func (s *server) submitSummaries(w http.ResponseWriter, req *http.Request, u auth.User) {
	var body struct {
		Summaries []runs.Summary `json:"summaries"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, req.Body, maxSummariesBody))
	if err := dec.Decode(&body); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			apiError(w, http.StatusRequestEntityTooLarge, "request body too large; send fewer summaries per call")
			return
		}
		apiError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	res, err := s.Runs.Submit(req.Context(), u.ID, req.PathValue("run_id"), body.Summaries)
	switch {
	case errors.Is(err, runs.ErrRunNotFound):
		apiError(w, http.StatusNotFound, "run not found")
	case err != nil:
		apiError(w, http.StatusInternalServerError, "internal error")
	default:
		if res.Accepted > 0 {
			s.writeBriefing(req.Context(), u.ID, req.PathValue("run_id"))
		}
		writeJSON(w, http.StatusOK, res)
	}
}

// writeBriefing writes the run's day to the user's markdown file. Failures
// are logged and never fail the submission: the summaries are already stored.
func (s *server) writeBriefing(ctx context.Context, userID, runID string) {
	day, err := s.Runs.Day(ctx, userID, runID)
	if err == nil {
		err = s.Briefing.Write(ctx, userID, day, false)
	}
	if err != nil {
		log.Printf("briefing: write file for user %s: %v", userID, err)
	}
}

// readPage renders today's briefing as one linear page for reading mode and
// text to speech.
func (s *server) readPage(w http.ResponseWriter, req *http.Request, sess *session) {
	ctx := req.Context()
	day, err := s.Runs.Today(ctx, sess.User.ID)
	var b briefing.Briefing
	if err == nil {
		b, err = s.Briefing.Build(ctx, sess.User.ID, day, false)
	}
	if err != nil {
		serverError(w)
		return
	}
	p := s.page(sess, "read", "Read")
	p.Day = day
	p.Today, p.Backlog = b.Today, b.Backlog
	w.Header().Set("Cache-Control", "no-store")
	s.ui.Render(w, http.StatusOK, "read", p)
}
