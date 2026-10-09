package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Jolls/Siftstr/internal/triage"
)

const (
	// maxSyncBody and maxSyncActions bound one batch. A page that was offline
	// for a long time sends several batches rather than one huge one.
	maxSyncBody    = 1 << 20
	maxSyncActions = 500
)

const errBatchTooLarge = "batch too large; send fewer actions per call"

// syncActions applies a batch of queued page actions. Session and CSRF are
// checked by protected; the results carry each subject's current state so the
// page can reconcile its badges. Replaying a batch is harmless.
func (s *server) syncActions(w http.ResponseWriter, req *http.Request, sess *session) {
	var body struct {
		Actions []triage.Action `json:"actions"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, req.Body, maxSyncBody))
	if err := dec.Decode(&body); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			apiError(w, http.StatusRequestEntityTooLarge, errBatchTooLarge)
			return
		}
		apiError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(body.Actions) > maxSyncActions {
		apiError(w, http.StatusRequestEntityTooLarge, errBatchTooLarge)
		return
	}
	results, err := s.Triage.Apply(req.Context(), sess.User.ID, body.Actions)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}
