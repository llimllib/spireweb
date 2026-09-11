package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/llimllib/spireweb/internal/indexer"
)

// stubStatus stands in for the indexer.
type stubStatus struct{ st indexer.Status }

func (s stubStatus) Status() indexer.Status { return s.st }

func (f *fixture) withIndexer(t *testing.T, st indexer.Status) *fixture {
	t.Helper()
	f.srv.indexer = stubStatus{st: st}
	return f
}

func TestStatusPollsFasterWhileIndexing(t *testing.T) {
	f := newFixture(t, map[string][]string{"aaa": {userMsg("hi")}})

	cases := []struct {
		phase    indexer.Phase
		wantPoll string
	}{
		{indexer.PhaseIndexing, pollBusy},
		{indexer.PhaseStarting, pollBusy},
		{indexer.PhaseTitling, pollBusy},
		{indexer.PhaseWatching, pollIdle},
		{indexer.PhaseStopped, pollIdle},
	}
	for _, c := range cases {
		f.withIndexer(t, indexer.Status{Phase: c.phase})
		_, doc := f.get(t, "/status")

		el := doc.Find("#status")
		if el.Length() != 1 {
			t.Fatalf("%s: no status element", c.phase)
		}
		// The element replaces itself, so the server decides the next
		// interval by rendering it.
		trigger, _ := el.Attr("hx-trigger")
		if trigger != "every "+c.wantPoll {
			t.Errorf("%s: hx-trigger = %q, want every %s", c.phase, trigger, c.wantPoll)
		}
		if swap, _ := el.Attr("hx-swap"); swap != "outerHTML" {
			t.Errorf("%s: hx-swap = %q", c.phase, swap)
		}
	}
}

func TestStatusShowsProgressWhileIndexing(t *testing.T) {
	f := newFixture(t, map[string][]string{"aaa": {userMsg("hi")}})
	f.withIndexer(t, indexer.Status{Phase: indexer.PhaseIndexing, Done: 42, Total: 100})

	_, doc := f.get(t, "/status")
	text := strings.Join(strings.Fields(doc.Find("#status").Text()), " ")
	if !strings.Contains(text, "indexing 42/100") {
		t.Errorf("status text = %q", text)
	}
}

// Titling is slow enough that saying "indexing" and then going quiet for a
// minute would look like a hang.
func TestStatusDistinguishesTitlingFromIndexing(t *testing.T) {
	f := newFixture(t, map[string][]string{"aaa": {userMsg("hi")}})
	f.withIndexer(t, indexer.Status{Phase: indexer.PhaseTitling, Done: 7, Total: 30})

	_, doc := f.get(t, "/status")
	text := strings.Join(strings.Fields(doc.Find("#status").Text()), " ")
	if !strings.Contains(text, "titling 7/30") {
		t.Errorf("status text = %q", text)
	}
}

// Rather than the server tracking what each open tab has seen, the page
// carries the count it rendered with and the poll compares against it.
func TestStatusReportsNewSessions(t *testing.T) {
	f := newFixture(t, map[string][]string{"aaa": {userMsg("hi")}})
	f.withIndexer(t, indexer.Status{Phase: indexer.PhaseWatching, Sessions: 5})

	_, doc := f.get(t, "/status?n=2")
	link := doc.Find("#status a")
	if link.Length() != 1 {
		t.Fatalf("no refresh link; status was %q", doc.Find("#status").Text())
	}
	if got := strings.TrimSpace(link.Text()); got != "3 new sessions" {
		t.Errorf("link text = %q", got)
	}

	// Singular, because "1 new sessions" is the kind of thing that makes an
	// interface feel unfinished.
	f.withIndexer(t, indexer.Status{Phase: indexer.PhaseWatching, Sessions: 3})
	_, doc = f.get(t, "/status?n=2")
	if got := strings.TrimSpace(doc.Find("#status a").Text()); got != "1 new session" {
		t.Errorf("link text = %q, want singular", got)
	}

	// Nothing new: no link, and no stale count left behind.
	f.withIndexer(t, indexer.Status{Phase: indexer.PhaseWatching, Sessions: 2})
	_, doc = f.get(t, "/status?n=2")
	if doc.Find("#status a").Length() != 0 {
		t.Error("reported new sessions when there were none")
	}
}

func TestStatusCarriesTheRenderedCountForward(t *testing.T) {
	f := newFixture(t, map[string][]string{"aaa": {userMsg("hi")}})
	f.withIndexer(t, indexer.Status{Phase: indexer.PhaseWatching, Sessions: 9})

	_, doc := f.get(t, "/status?n=4")
	// Each poll replaces the element, so the next poll's URL has to keep the
	// count or the comparison is lost after the first tick.
	if got, _ := doc.Find("#status").Attr("hx-get"); got != "/status?n=4" {
		t.Errorf("hx-get = %q, want the count preserved", got)
	}
}

func TestStatusShowsErrors(t *testing.T) {
	f := newFixture(t, map[string][]string{"aaa": {userMsg("hi")}})
	f.withIndexer(t, indexer.Status{Phase: indexer.PhaseWatching, LastErr: "disk on fire"})

	_, doc := f.get(t, "/status")
	el := doc.Find(".status-error")
	if el.Length() != 1 {
		t.Fatal("error not surfaced")
	}
	if title, _ := el.Attr("title"); title != "disk on fire" {
		t.Errorf("title = %q", title)
	}
}

// With --no-watch, or a read-only index, there is no indexer. The element
// should disappear rather than poll forever against a server with nothing to
// say.
func TestStatusEmptyWithoutAnIndexer(t *testing.T) {
	f := newFixture(t, map[string][]string{"aaa": {userMsg("hi")}})
	f.srv.indexer = nil

	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != "" {
		t.Errorf("body = %q, want empty so hx-swap removes the element", rec.Body.String())
	}
}

// The page has to seed the poll, or the indicator never starts.
func TestPageSeedsTheStatusPoll(t *testing.T) {
	f := newFixture(t, map[string][]string{
		"aaa": {userMsg("one")},
		"bbb": {userMsg("two")},
	})
	_, doc := f.get(t, "/")

	el := doc.Find("#status")
	if el.Length() != 1 {
		t.Fatal("no status element on the page")
	}
	if got, _ := el.Attr("hx-get"); got != "/status?n=2" {
		t.Errorf("hx-get = %q, want the rendered session count", got)
	}
	if got, _ := el.Attr("hx-trigger"); got != "load" {
		t.Errorf("hx-trigger = %q, want load", got)
	}
}
