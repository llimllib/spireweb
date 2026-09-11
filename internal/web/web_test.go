package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"

	"github.com/llimllib/spireweb/internal/index"
	"github.com/llimllib/spireweb/internal/search"
)

// Assertions run against the parsed document rather than golden files. The
// markup is still in flux, and a golden test on markup in flux trains you to
// regenerate the file without reading the diff.

func userMsg(text string) string {
	return `{"type":"message","timestamp":"2026-03-31T12:26:01.076Z","message":{"role":"user","content":[{"type":"text","text":` + quote(text) + `}]}}`
}

func assistantMsg(text string) string {
	return `{"type":"message","timestamp":"2026-03-31T12:26:02.076Z","message":{"role":"assistant","content":[{"type":"text","text":` + quote(text) + `}]}}`
}

func toolCallMsg(id, name, args string) string {
	return `{"type":"message","timestamp":"2026-03-31T12:26:03.076Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"` + id + `","name":"` + name + `","arguments":` + args + `}]}}`
}

func toolResultMsg(id, text string) string {
	return `{"type":"message","timestamp":"2026-03-31T12:26:04.076Z","message":{"role":"toolResult","toolCallId":"` + id + `","toolName":"bash","isError":false,"content":[{"type":"text","text":` + quote(text) + `}]}}`
}

// quote produces a JSON string literal.
func quote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`)
	return `"` + r.Replace(s) + `"`
}

type fixture struct {
	srv     *Server
	dir     string
	dbPath  string
	handler http.Handler
}

// newFixture indexes a corpus and returns a server over it. started is used
// to order sessions, since the list is newest first.
func newFixture(t *testing.T, sessions map[string][]string) *fixture {
	t.Helper()

	dir := t.TempDir()
	sub := filepath.Join(dir, "--Users-me-code-proj--")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// Timestamps ascend with the id, so tests can say which session is newest.
	// Ranging over the map directly would assign them in Go's randomized
	// iteration order, which makes any "newest session" assertion a coin flip.
	ids := make([]string, 0, len(sessions))
	for id := range sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	stamps := map[string]string{}
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	for i, id := range ids {
		stamps[id] = base.Add(time.Duration(i) * time.Hour).Format(time.RFC3339)
	}
	for id, msgs := range sessions {
		header := `{"type":"session","version":3,"id":"` + id +
			`","timestamp":"` + stamps[id] + `","cwd":"/Users/me/code/proj"}`
		body := header + "\n" + strings.Join(msgs, "\n") + "\n"
		if err := os.WriteFile(filepath.Join(sub, id+".jsonl"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	dbPath := filepath.Join(t.TempDir(), "i.db")
	db, err := index.Open(dbPath, index.DriverName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := index.Build(context.Background(), db, index.BuildOptions{Dir: dir}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	reader, err := index.OpenReader(dbPath, index.DriverName)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reader.Close() })

	// Lexical only: these tests exercise the web layer, and requiring a
	// loadable embedding model would make the suite depend on a GPU.
	engine := &search.Engine{
		Rankers: []search.Ranker{&search.Lexical{DB: reader.SQL()}},
		Fusion:  search.DefaultFusion(),
	}
	srv, err := New(reader, engine, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{srv: srv, dir: sub, dbPath: dbPath, handler: srv.Handler()}
}

func (f *fixture) get(t *testing.T, path string) (*httptest.ResponseRecorder, *goquery.Document) {
	t.Helper()
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(rec.Body.String()))
	if err != nil {
		t.Fatal(err)
	}
	return rec, doc
}

func TestIndexListsSessionsAndOpensTheNewest(t *testing.T) {
	f := newFixture(t, map[string][]string{
		"aaa": {userMsg("first question"), assistantMsg("first answer")},
		"bbb": {userMsg("second question"), assistantMsg("second answer")},
	})

	rec, doc := f.get(t, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if n := doc.Find(".row").Length(); n != 2 {
		t.Errorf("rows = %d, want 2", n)
	}

	// Exactly one row is current, and it is the one the reading pane shows.
	current := doc.Find(`.row[aria-current="page"]`)
	if current.Length() != 1 {
		t.Fatalf("aria-current rows = %d, want 1", current.Length())
	}
	href, _ := current.Attr("href")

	// bbb is newest by started_at.
	if href != "/sessions/bbb" {
		t.Errorf("selected %s, want /sessions/bbb", href)
	}
	if !strings.Contains(doc.Find(".reading").Text(), "second question") {
		t.Error("reading pane does not show the newest session")
	}
}

func TestRowsShowProjectTitleAndSubtitle(t *testing.T) {
	f := newFixture(t, map[string][]string{
		"aaa": {userMsg("how do I center a div"), assistantMsg("use flexbox")},
	})
	_, doc := f.get(t, "/")

	row := doc.Find(".row").First()
	if got := strings.TrimSpace(row.Find(".row-project").Text()); got != "code/proj" {
		t.Errorf("project = %q", got)
	}
	// Until titles are generated the heading falls back to the opening
	// message, and the third line shows the reply so the two do not repeat.
	if got := strings.TrimSpace(row.Find(".row-title").Text()); got != "how do I center a div" {
		t.Errorf("title = %q", got)
	}
	if got := strings.TrimSpace(row.Find(".row-preview").Text()); got != "use flexbox" {
		t.Errorf("preview = %q", got)
	}
	if row.Find(".row-date").Length() != 1 {
		t.Error("no date")
	}
}

func TestSessionRouteSelectsThatSession(t *testing.T) {
	f := newFixture(t, map[string][]string{
		"aaa": {userMsg("older one")},
		"bbb": {userMsg("newer one")},
	})

	rec, doc := f.get(t, "/sessions/aaa")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	href, _ := doc.Find(`.row[aria-current="page"]`).Attr("href")
	if href != "/sessions/aaa" {
		t.Errorf("selected %s, want /sessions/aaa", href)
	}
	if !strings.Contains(doc.Find(".reading").Text(), "older one") {
		t.Error("reading pane shows the wrong session")
	}
	// The list is still fully rendered: navigation is a page load, so every
	// response carries both panes.
	if n := doc.Find(".row").Length(); n != 2 {
		t.Errorf("rows = %d, want 2", n)
	}
}

func TestUnknownSessionIs404(t *testing.T) {
	f := newFixture(t, map[string][]string{"aaa": {userMsg("hi")}})
	rec, _ := f.get(t, "/sessions/nope")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// The index records what was on disk when it last ran. A session deleted
// since then should still show its metadata and say what happened, rather
// than producing an error page.
func TestMissingFileDegradesGracefully(t *testing.T) {
	f := newFixture(t, map[string][]string{"aaa": {userMsg("hello there")}})
	if err := os.Remove(filepath.Join(f.dir, "aaa.jsonl")); err != nil {
		t.Fatal(err)
	}

	rec, doc := f.get(t, "/sessions/aaa")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	notice := doc.Find(".notice").Text()
	if !strings.Contains(notice, "no longer on disk") {
		t.Errorf("notice = %q", notice)
	}
	// Metadata still comes from the index.
	if !strings.Contains(doc.Find(".reading-head").Text(), "hello there") {
		t.Error("metadata missing for a session whose file is gone")
	}
}

func TestEmptyIndexExplainsItself(t *testing.T) {
	f := newFixture(t, map[string][]string{})
	rec, doc := f.get(t, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(doc.Text(), "spireweb index") {
		t.Error("empty state does not say how to fix it")
	}
}

// Session content is arbitrary text and frequently contains HTML. This is the
// one plausible bug in the web layer that is a vulnerability rather than a
// defect, so it is asserted end to end and not only in the render package.
func TestSessionContentIsNotExecutable(t *testing.T) {
	f := newFixture(t, map[string][]string{
		"aaa": {
			userMsg("<script>alert('pwned')</script> and <img src=x onerror=alert(2)>"),
			assistantMsg("A <div onclick=\"steal()\">block</div> of markup."),
		},
	})
	rec, doc := f.get(t, "/sessions/aaa")
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}

	// No scripts beyond the two the layout itself loads, and none inline.
	doc.Find("script").Each(func(_ int, s *goquery.Selection) {
		src, _ := s.Attr("src")
		if src != "/static/htmx.min.js" && src != "/static/app.js" {
			t.Errorf("unexpected script: src=%q content=%q", src, s.Text())
		}
	})
	if doc.Find(".reading img, .reading div[onclick]").Length() != 0 {
		t.Error("session markup became live elements")
	}
	// The text is still there, as text.
	if !strings.Contains(doc.Find(".reading").Text(), "alert('pwned')") {
		t.Error("content was discarded instead of escaped")
	}
}

func TestToolCallsAreCollapsedAndLazy(t *testing.T) {
	f := newFixture(t, map[string][]string{
		"aaa": {
			userMsg("what changed"),
			toolCallMsg("tc1", "bash", `{"command":"git log --oneline"}`),
			toolResultMsg("tc1", "abc123 first commit"),
		},
	})
	rec, doc := f.get(t, "/sessions/aaa")
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}

	tool := doc.Find("details.tool")
	if tool.Length() != 1 {
		t.Fatalf("tool details = %d, want 1", tool.Length())
	}
	if got := strings.TrimSpace(tool.Find(".tool-name").Text()); got != "bash" {
		t.Errorf("tool name = %q", got)
	}
	if got := strings.TrimSpace(tool.Find(".tool-summary").Text()); got != "git log --oneline" {
		t.Errorf("tool summary = %q", got)
	}
	// Collapsed by default: no open attribute.
	if _, open := tool.Attr("open"); open {
		t.Error("tool call rendered expanded")
	}
	// The output is not in the page; that is the entire point.
	if strings.Contains(rec.Body.String(), "abc123 first commit") {
		t.Error("tool output was inlined rather than fetched on expand")
	}
	body := tool.Find(".tool-body")
	if got, _ := body.Attr("hx-get"); got != "/sessions/aaa/tool/1/0" {
		t.Errorf("hx-get = %q", got)
	}
	if got, _ := body.Attr("hx-trigger"); !strings.Contains(got, "toggle") {
		t.Errorf("hx-trigger = %q, want a toggle trigger", got)
	}
}

func TestToolFragmentReturnsArgsAndOutput(t *testing.T) {
	f := newFixture(t, map[string][]string{
		"aaa": {
			userMsg("what changed"),
			toolCallMsg("tc1", "bash", `{"command":"git log --oneline"}`),
			toolResultMsg("tc1", "abc123 first commit"),
		},
	})

	rec, doc := f.get(t, "/sessions/aaa/tool/1/0")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	// A fragment, not a page.
	if strings.Contains(rec.Body.String(), "<html") {
		t.Error("tool route returned a full page")
	}
	if !strings.Contains(doc.Find(".tool-args").Text(), "git log --oneline") {
		t.Errorf("arguments missing: %q", doc.Find(".tool-args").Text())
	}
	if !strings.Contains(doc.Find(".tool-output").Text(), "abc123 first commit") {
		t.Errorf("output missing: %q", doc.Find(".tool-output").Text())
	}
}

func TestToolFragmentRejectsBadReferences(t *testing.T) {
	f := newFixture(t, map[string][]string{
		"aaa": {userMsg("hi"), toolCallMsg("tc1", "bash", `{"command":"ls"}`)},
	})
	cases := map[string]int{
		"/sessions/aaa/tool/99/0":  http.StatusNotFound, // no such message
		"/sessions/aaa/tool/1/99":  http.StatusNotFound, // no such block
		"/sessions/aaa/tool/0/0":   http.StatusNotFound, // not a tool call
		"/sessions/aaa/tool/x/0":   http.StatusBadRequest,
		"/sessions/nope/tool/0/0":  http.StatusNotFound,
		"/sessions/aaa/tool/-1/0":  http.StatusNotFound,
		"/sessions/aaa/tool/1/-1":  http.StatusNotFound,
		"/sessions/aaa/tool/1/0/2": http.StatusNotFound, // not a route
	}
	for path, want := range cases {
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
		}
	}
}

// The reading pane reads the file, not the index, so a session that has grown
// since indexing shows its newest messages. This is why transcripts are
// parsed on demand.
func TestTranscriptReflectsFileNotIndex(t *testing.T) {
	f := newFixture(t, map[string][]string{"aaa": {userMsg("original message")}})

	path := filepath.Join(f.dir, "aaa.jsonl")
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteString(assistantMsg("appended after indexing") + "\n"); err != nil {
		t.Fatal(err)
	}
	fh.Close()

	_, doc := f.get(t, "/sessions/aaa")
	text := doc.Find(".reading").Text()
	if !strings.Contains(text, "appended after indexing") {
		t.Error("reading pane did not pick up a message added after indexing")
	}
}

func TestStaticAssetsAreServed(t *testing.T) {
	f := newFixture(t, map[string][]string{"aaa": {userMsg("hi")}})
	for _, path := range []string{
		"/static/app.css", "/static/app.js", "/static/htmx.min.js", "/static/chroma.css",
	} {
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d", path, rec.Code)
			continue
		}
		if rec.Body.Len() == 0 {
			t.Errorf("GET %s served an empty body", path)
		}
	}
}

// The id is how a session is resumed in a terminal, so it has to be somewhere
// a reader can copy it from.
func TestTranscriptHeaderShowsSessionID(t *testing.T) {
	f := newFixture(t, map[string][]string{"aaa": {userMsg("hello there")}})

	_, doc := f.get(t, "/sessions/aaa")
	if got := strings.TrimSpace(doc.Find(".reading-head .reading-id").Text()); got != "aaa" {
		t.Errorf("session id in header = %q, want %q", got, "aaa")
	}
}

// editResultMsg is an edit's result, carrying the rendered diff pi records
// alongside the textual outcome.
func editResultMsg(id, text, diff string) string {
	return `{"type":"message","timestamp":"2026-03-31T12:26:04.076Z","message":{"role":"toolResult","toolCallId":"` +
		id + `","toolName":"edit","isError":false,"content":[{"type":"text","text":` + quote(text) +
		`}],"details":{"diff":` + quote(diff) + `}}}`
}

// An edit used to expand into its arguments: a JSON blob with the old and new
// text run together as escaped newlines. The session carries the diff pi drew
// in the terminal, so show that instead.
func TestEditToolRendersADiff(t *testing.T) {
	f := newFixture(t, map[string][]string{
		"aaa": {
			userMsg("tidy the import"),
			toolCallMsg("tc1", "edit", `{"path":"a.py","edits":[{"oldText":"import (\n    X,\n)","newText":"import X"}]}`),
			editResultMsg("tc1", "Successfully replaced 1 block(s) in a.py.",
				"     ...\n  45 # before\n- 46 import (\n- 47     X,\n- 48 )\n+ 46 import X\n  49 # after"),
		},
	})

	rec, doc := f.get(t, "/sessions/aaa/tool/1/0")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	if n := doc.Find(".diff-line.is-del").Length(); n != 3 {
		t.Errorf("removed lines = %d, want 3", n)
	}
	if n := doc.Find(".diff-line.is-add").Length(); n != 1 {
		t.Errorf("added lines = %d, want 1", n)
	}
	if n := doc.Find(".diff-line.is-gap").Length(); n != 1 {
		t.Errorf("gap markers = %d, want 1", n)
	}

	// Line numbers are the reason to use pi's diff rather than computing one
	// from the arguments, which cannot know where in the file the edit landed.
	nums := doc.Find(".diff-line.is-del .diff-num").Map(func(_ int, s *goquery.Selection) string {
		return strings.TrimSpace(s.Text())
	})
	if strings.Join(nums, ",") != "46,47,48" {
		t.Errorf("line numbers = %v, want 46,47,48", nums)
	}

	// The marker is real text, so the diff still reads without colour.
	if got := strings.TrimSpace(doc.Find(".diff-line.is-add .diff-mark").First().Text()); got != "+" {
		t.Errorf("marker = %q, want +", got)
	}

	// And the arguments are gone: the diff says all of it, readably.
	if doc.Find(".tool-args").Length() != 0 {
		t.Error("arguments still rendered alongside the diff")
	}
	if strings.Contains(rec.Body.String(), `oldText`) {
		t.Error("raw edit arguments leaked into the fragment")
	}
}

// Every message gets a unique anchor, which is what a search hit's msg_idx
// resolves against.
func TestTranscriptAnchorsAreUnique(t *testing.T) {
	f := newFixture(t, map[string][]string{
		"aaa": {
			userMsg("what changed"),
			toolCallMsg("tc1", "bash", `{"command":"git log"}`),
			toolResultMsg("tc1", "abc123"),
			assistantMsg("two commits"),
		},
	})
	_, doc := f.get(t, "/sessions/aaa")

	seen := map[string]int{}
	doc.Find(".reading [id]").Each(func(_ int, sel *goquery.Selection) {
		id, _ := sel.Attr("id")
		if strings.HasPrefix(id, "m") {
			seen[id]++
		}
	})
	if len(seen) == 0 {
		t.Fatal("no message anchors in the transcript")
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("anchor %s appears %d times; ids must be unique", id, n)
		}
	}
	// The user turn is message 0 and the tool call is message 1.
	if doc.Find("#m0.turn-user").Length() != 1 {
		t.Error("no #m0 on the opening user turn")
	}
	if doc.Find("details.tool#m1").Length() != 1 {
		t.Error("no #m1 on the tool call")
	}
}
