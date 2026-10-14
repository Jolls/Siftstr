package server

import (
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/Jolls/Siftstr/internal/ui"
)

// briefingsList shows the days the user has a briefing for, newest first, with
// a link to view or download each. A day whose file is missing is listed
// without links.
func (s *server) briefingsList(w http.ResponseWriter, req *http.Request, sess *session) {
	ctx := req.Context()
	days, err := s.Briefing.Days(ctx, sess.User.ID)
	if err != nil {
		serverError(w)
		return
	}
	p := s.page(sess, "briefings", "Briefings")
	for _, d := range days {
		_, err := s.Briefing.File(ctx, sess.User.ID, d)
		switch {
		case err == nil:
			p.BriefingDays = append(p.BriefingDays, ui.BriefingDay{Day: d, HasFile: true})
		case errors.Is(err, os.ErrNotExist):
			p.BriefingDays = append(p.BriefingDays, ui.BriefingDay{Day: d})
		default:
			serverError(w)
			return
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	s.ui.Render(w, http.StatusOK, "briefings", p)
}

// briefingText returns the stored file for the requested day, or writes the
// response itself and reports false. A malformed day, a day with no file and
// another user's day all look the same: not found.
func (s *server) briefingText(w http.ResponseWriter, req *http.Request, sess *session) (day string, text []byte, ok bool) {
	day = req.PathValue("day")
	if _, err := time.Parse("2006-01-02", day); err != nil {
		http.NotFound(w, req)
		return "", nil, false
	}
	text, err := s.Briefing.File(req.Context(), sess.User.ID, day)
	if errors.Is(err, os.ErrNotExist) {
		http.NotFound(w, req)
		return "", nil, false
	}
	if err != nil {
		serverError(w)
		return "", nil, false
	}
	return day, text, true
}

// briefingView shows one day's file as plain text.
func (s *server) briefingView(w http.ResponseWriter, req *http.Request, sess *session) {
	day, text, ok := s.briefingText(w, req, sess)
	if !ok {
		return
	}
	p := s.page(sess, "briefings", "Briefing "+day)
	p.Day, p.BriefingText = day, string(text)
	w.Header().Set("Cache-Control", "no-store")
	s.ui.Render(w, http.StatusOK, "briefing", p)
}

// briefingDownload sends one day's file as written.
func (s *server) briefingDownload(w http.ResponseWriter, req *http.Request, sess *session) {
	day, text, ok := s.briefingText(w, req, sess)
	if !ok {
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/markdown; charset=utf-8")
	h.Set("Content-Disposition", `attachment; filename="siftstr-`+day+`.md"`)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	_, _ = w.Write(text)
}
