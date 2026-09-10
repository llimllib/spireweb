package web

import (
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/llimllib/spireweb/internal/index"
	"github.com/llimllib/spireweb/internal/render"
)

// listLimit is how many rows the list pane renders.
//
// The whole list is sent at once: 1100 rows is a few hundred kilobytes of HTML
// and browsers scroll it without complaint, while pagination would cost either
// a scroll handler or a "load more" button for no benefit at this size. The
// limit exists so a corpus an order of magnitude larger degrades into a
// truncated list rather than a hung tab.
const listLimit = 2000

// pageData is what every full page render receives.
type pageData struct {
	Sessions []index.Summary
	Selected *index.Summary
	Query    string

	// Transcript is nil when no session is open.
	Transcript []render.Entry

	// Notice explains an empty or degraded pane: no index yet, or a session
	// whose file has since been deleted.
	Notice string
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.db.ListSessions(r.Context(), listLimit, 0)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	data := pageData{Sessions: sessions}

	// The newest session opens by default, which is almost always the one you
	// came back for.
	if len(sessions) > 0 {
		s.loadTranscript(r, &data, sessions[0])
	} else {
		data.Notice = "No sessions indexed yet. Run `spireweb index`."
	}
	s.render(w, r, "layout.html", data)
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	selected, err := s.db.Session(r.Context(), id)
	if errors.Is(err, index.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}

	sessions, err := s.db.ListSessions(r.Context(), listLimit, 0)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	data := pageData{Sessions: sessions}
	s.loadTranscript(r, &data, selected)
	s.render(w, r, "layout.html", data)
}

// loadTranscript parses the session file and fills in the right pane.
//
// A missing file is reported in place rather than as an error page: the index
// is a cache of what was on disk when it last ran, and a session deleted since
// then should still show its metadata and say what happened.
func (s *Server) loadTranscript(r *http.Request, data *pageData, sum index.Summary) {
	data.Selected = &sum

	parsed, err := s.cache.Get(sum.Path)
	if err != nil {
		if os.IsNotExist(err) {
			data.Notice = "This session's file is no longer on disk: " + sum.Path
		} else {
			data.Notice = "Could not read this session: " + err.Error()
		}
		return
	}
	data.Transcript = render.Transcript(parsed)
	if len(data.Transcript) == 0 {
		data.Notice = "This session has no conversation in it."
	}
}

// handleTool returns one tool call's arguments and output.
//
// Separate from the transcript because tool traffic is roughly 98% of a
// session's bytes; sending it with the page would make a 77KB conversation
// into a 7MB one.
func (s *Server) handleTool(w http.ResponseWriter, r *http.Request) {
	sum, err := s.db.Session(r.Context(), r.PathValue("id"))
	if errors.Is(err, index.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}

	msgIdx, err1 := strconv.Atoi(r.PathValue("msg"))
	blk, err2 := strconv.Atoi(r.PathValue("blk"))
	if err1 != nil || err2 != nil {
		http.Error(w, "bad tool reference", http.StatusBadRequest)
		return
	}

	parsed, err := s.cache.Get(sum.Path)
	if err != nil {
		http.Error(w, "session file unavailable", http.StatusGone)
		return
	}

	detail, err := render.Tool(parsed, msgIdx, blk)
	if err != nil {
		// A stale page whose session has since grown or been rewritten can
		// reference a block that no longer exists.
		http.Error(w, "tool call not found", http.StatusNotFound)
		return
	}
	s.render(w, r, "tooldetail.html", detail)
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, data any) {
	tmpl, err := s.templates()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		// The status line is already sent by now, so this cannot become a 500;
		// log it and let the truncated response speak for itself.
		log.Printf("render %s: %v", name, err)
	}
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("%s %s: %v", r.Method, r.URL.Path, err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
