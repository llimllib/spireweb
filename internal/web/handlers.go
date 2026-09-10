package web

import (
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

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

// searchLimit caps how many sessions a query returns. Beyond a screenful or
// two nobody reads further, and every row costs an excerpt query.
const searchLimit = 100

// pageData is what every full page render receives.
type pageData struct {
	Sessions []row
	Selected *index.Summary
	Query    string

	// Searching distinguishes "no sessions indexed" from "no matches".
	Searching bool

	// SearchEnabled is false when no ranker could be built, which disables
	// the input rather than offering a box that silently does nothing.
	SearchEnabled bool

	// SessionCount is how many sessions were indexed when this page was
	// built, so the status poll can report arrivals since then.
	SessionCount int

	// Transcript is nil when no session is open.
	Transcript []render.Entry

	// Notice explains an empty or degraded pane: no index yet, or a session
	// whose file has since been deleted.
	Notice string
}

// newPage assembles the list pane, which every full page render needs.
func (s *Server) newPage(r *http.Request) (pageData, error) {
	q := r.URL.Query().Get("q")
	rows, err := s.rowsFor(r.Context(), q)
	if err != nil {
		return pageData{}, err
	}
	// Ignored on failure: the count drives a cosmetic affordance, and losing
	// it should not cost the page.
	count, _ := s.db.CountSessions(r.Context())

	return pageData{
		Sessions:      rows,
		Query:         q,
		Searching:     strings.TrimSpace(q) != "",
		SearchEnabled: s.engine != nil,
		SessionCount:  count,
	}, nil
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	data, err := s.newPage(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	// The top result opens by default: the newest session when browsing, the
	// best match when searching, which is the one you were looking for.
	switch {
	case len(data.Sessions) > 0:
		s.loadTranscript(&data, data.Sessions[0].Summary)
	case data.Searching:
		data.Notice = "No sessions match that search."
	default:
		data.Notice = "No sessions indexed yet. Run `spireweb index`."
	}
	s.render(w, r, "layout.html", data)
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	selected, err := s.db.Session(r.Context(), r.PathValue("id"))
	if errors.Is(err, index.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}

	// The query rides along in the link, so opening a result keeps the
	// filtered list beside it and going back behaves.
	data, err := s.newPage(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.loadTranscript(&data, selected)
	s.render(w, r, "layout.html", data)
}

// handleSearch serves the list pane for a query.
//
// The one place in the codebase that branches on HX-Request. HTMX asks for
// the rows alone and swaps them in place; a cold load or a pasted link gets
// the whole page, so a search URL is a real, shareable page rather than a
// fragment that renders as naked markup.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("HX-Request") != "true" {
		s.handleIndex(w, r)
		return
	}
	data, err := s.newPage(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "list.html", data)
}

// loadTranscript parses the session file and fills in the right pane.
//
// A missing file is reported in place rather than as an error page: the index
// is a cache of what was on disk when it last ran, and a session deleted since
// then should still show its metadata and say what happened.
func (s *Server) loadTranscript(data *pageData, sum index.Summary) {
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
