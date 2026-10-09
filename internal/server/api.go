package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Jolls/Siftstr/internal/auth"
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
		writeJSON(w, http.StatusOK, res)
	}
}
