package web

import (
	"net/http"
	"strconv"

	"github.com/llimllib/spireweb/internal/indexer"
)

// Poll intervals. Busy is often enough to look live during a build; idle is
// slow enough that a tab left open overnight is not doing anything much, but
// quick enough to notice a conversation happening in another terminal.
const (
	pollBusy = "2s"
	pollIdle = "10s"
)

// statusData drives the header's indexing indicator.
type statusData struct {
	Status indexer.Status
	Poll   string

	// Rendered is the session count the open page was built with. The
	// difference is what makes "3 new sessions" possible without the server
	// tracking per-client state.
	Rendered int
	New      int
}

// handleStatus renders the indexing indicator.
//
// It replaces itself on every poll, so the server chooses the next interval
// by rendering it into the response: fast while indexing, slow when idle.
// Removing the element entirely when idle would be tidier, but then a session
// started later would never be noticed by an open tab.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if s.indexer == nil {
		// Nothing is indexing, so nothing should poll. An empty response with
		// hx-swap="outerHTML" removes the element.
		w.WriteHeader(http.StatusOK)
		return
	}

	st := s.indexer.Status()
	data := statusData{Status: st, Poll: pollIdle}
	if st.Busy() {
		data.Poll = pollBusy
	}

	// n carries what the page rendered with; absent or unparseable means the
	// caller does not want the comparison.
	if raw := r.URL.Query().Get("n"); raw != "" {
		if rendered, err := strconv.Atoi(raw); err == nil {
			data.Rendered = rendered
			if d := st.Sessions - rendered; d > 0 {
				data.New = d
			}
		}
	}

	s.render(w, r, "status.html", data)
}
