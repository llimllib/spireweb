package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
)

// htmxGet issues a request the way HTMX does, which is the only thing that
// distinguishes a fragment response from a full page.
func (f *fixture) htmxGet(t *testing.T, path string) (*httptest.ResponseRecorder, *goquery.Document) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(rec.Body.String()))
	if err != nil {
		t.Fatal(err)
	}
	return rec, doc
}

func searchCorpus() map[string][]string {
	return map[string][]string{
		"aaa": {userMsg("the database deadlocked during deploy"), assistantMsg("check the lock ordering")},
		"bbb": {userMsg("how do I center a div"), assistantMsg("use flexbox")},
		"ccc": {userMsg("rename the branch on the remote"), assistantMsg("git push origin :old")},
	}
}

func TestSearchFiltersTheList(t *testing.T) {
	f := newFixture(t, searchCorpus())

	_, doc := f.get(t, "/search?q=deadlocked")
	rows := doc.Find(".row")
	if rows.Length() != 1 {
		t.Fatalf("rows = %d, want 1", rows.Length())
	}
	href, _ := rows.First().Attr("href")
	if !strings.HasPrefix(href, "/sessions/aaa") {
		t.Errorf("matched %s, want session aaa", href)
	}
}

// The only HX-Request branch in the codebase. HTMX gets the rows alone; a
// pasted link gets a real page, so a search URL is shareable rather than a
// fragment that renders as naked markup.
func TestSearchServesFragmentOrPage(t *testing.T) {
	f := newFixture(t, searchCorpus())

	recFrag, docFrag := f.htmxGet(t, "/search?q=flexbox")
	if recFrag.Code != http.StatusOK {
		t.Fatalf("status = %d", recFrag.Code)
	}
	if strings.Contains(recFrag.Body.String(), "<html") {
		t.Error("HTMX request got a full page")
	}
	if docFrag.Find("#rows").Length() != 1 {
		t.Error("fragment is not the rows list")
	}
	if docFrag.Find(".reading").Length() != 0 {
		t.Error("fragment included the reading pane")
	}

	recPage, docPage := f.get(t, "/search?q=flexbox")
	if recPage.Code != http.StatusOK {
		t.Fatalf("status = %d", recPage.Code)
	}
	if !strings.Contains(recPage.Body.String(), "<html") {
		t.Error("plain request did not get a full page")
	}
	// A pasted search link opens the best match, not an empty pane.
	if docPage.Find(".reading .turn").Length() == 0 {
		t.Error("full-page search did not open the top result")
	}
	if got, _ := docPage.Find(`input[name="q"]`).Attr("value"); got != "flexbox" {
		t.Errorf("query not reflected in the box: %q", got)
	}
}

func TestEmptyQueryRestoresTheFullList(t *testing.T) {
	f := newFixture(t, searchCorpus())
	for _, path := range []string{"/search?q=", "/search?q=%20%20", "/"} {
		_, doc := f.get(t, path)
		if n := doc.Find(".row").Length(); n != 3 {
			t.Errorf("GET %s: rows = %d, want all 3", path, n)
		}
	}
}

func TestSearchWithNoMatchesSaysSo(t *testing.T) {
	f := newFixture(t, searchCorpus())
	_, doc := f.get(t, "/search?q=xyzzyplughnothing")
	if n := doc.Find(".row").Length(); n != 0 {
		t.Fatalf("rows = %d, want 0", n)
	}
	if !strings.Contains(doc.Text(), "No sessions match") {
		t.Errorf("no empty-state message: %q", doc.Find(".list").Text())
	}
}

// Showing why a result matched is most of what makes search trustworthy.
func TestResultRowsShowTheMatchingText(t *testing.T) {
	f := newFixture(t, searchCorpus())
	_, doc := f.get(t, "/search?q=deadlocked")

	preview := doc.Find(".row-preview").First()
	if !strings.Contains(preview.Text(), "deadlocked") {
		t.Errorf("excerpt does not contain the match: %q", preview.Text())
	}
	// Browsing shows the opening exchange; searching replaces it with the
	// chunk that matched.
	if strings.TrimSpace(preview.Text()) == "check the lock ordering" {
		t.Error("row still shows the reply rather than the match")
	}
	if preview.Find("mark").Length() == 0 {
		t.Error("matched term is not marked")
	}
	if got := preview.Find("mark").First().Text(); got != "deadlocked" {
		t.Errorf("marked %q, want the query term", got)
	}
}

// The excerpt is session text with markers pasted into it by SQLite. Escaping
// has to happen before the markers become tags, or one of the two is wrong.
func TestExcerptMarkupIsEscaped(t *testing.T) {
	f := newFixture(t, map[string][]string{
		"aaa": {userMsg("the <script>alert(1)</script> deadlocked thing")},
	})
	rec, doc := f.get(t, "/search?q=deadlocked")

	preview := doc.Find(".row-preview").First()
	if preview.Find("script").Length() != 0 {
		t.Error("session markup became a live element in a result row")
	}
	if !strings.Contains(preview.Text(), "alert(1)") {
		t.Errorf("content lost from the excerpt: %q", preview.Text())
	}
	if preview.Find("mark").Length() == 0 {
		t.Error("marking broke when the excerpt contained markup")
	}
	if strings.Contains(rec.Body.String(), "\x01") || strings.Contains(rec.Body.String(), "\x02") {
		t.Error("sentinel characters leaked into the response")
	}
}

// Opening a result has to keep the search beside it, since navigation is a
// full page load and the query would otherwise be lost.
func TestQueryIsCarriedThroughLinks(t *testing.T) {
	f := newFixture(t, searchCorpus())

	_, doc := f.get(t, "/search?q=deadlocked")
	href, _ := doc.Find(".row").First().Attr("href")
	if href != "/sessions/aaa?q=deadlocked" {
		t.Fatalf("href = %q, want the query carried", href)
	}

	_, doc = f.get(t, href)
	if got, _ := doc.Find(`input[name="q"]`).Attr("value"); got != "deadlocked" {
		t.Errorf("query box = %q after following the link", got)
	}
	if n := doc.Find(".row").Length(); n != 1 {
		t.Errorf("rows = %d, want the list still filtered", n)
	}
	if !strings.Contains(doc.Find(".reading").Text(), "deadlocked") {
		t.Error("the linked session did not open")
	}
}

func TestQueryIsEscapedInLinks(t *testing.T) {
	f := newFixture(t, searchCorpus())
	// A query with characters that are significant in URLs and in HTML.
	q := `deadlocked" onmouseover=alert(1) &foo`
	_, doc := f.get(t, "/search?q="+url.QueryEscape(q))

	sel := doc.Find(".row").First()
	if _, bad := sel.Attr("onmouseover"); bad {
		t.Error("query text escaped its attribute and became a handler")
	}
	href, _ := sel.Attr("href")
	if strings.Contains(href, `"`) || strings.Contains(href, " ") {
		t.Errorf("href not escaped: %q", href)
	}
}

// FTS5 has its own query syntax, and characters common in developer searches
// are operators there. Unbalanced quotes and leading hyphens are syntax
// errors, which with search-as-you-type would surface constantly.
func TestAwkwardQueriesDoNotError(t *testing.T) {
	f := newFixture(t, searchCorpus())
	for _, q := range []string{
		`-flag`, `"unbalanced`, `foo:bar`, `a AND`, `NEAR(`, `*`, `()`,
		`--pooling`, `x*`, `'`, `\`, `OR OR OR`, `café`, `日本語`,
	} {
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, httptest.NewRequest(
			http.MethodGet, "/search?q="+url.QueryEscape(q), nil))
		if rec.Code != http.StatusOK {
			t.Errorf("query %q returned %d", q, rec.Code)
		}
	}
}

func TestSearchBoxIsWiredForLiveSearch(t *testing.T) {
	f := newFixture(t, searchCorpus())
	_, doc := f.get(t, "/")

	input := doc.Find(`input[name="q"]`)
	if _, disabled := input.Attr("disabled"); disabled {
		t.Fatal("search box is disabled when an engine is present")
	}
	if got, _ := input.Attr("hx-get"); got != "/search" {
		t.Errorf("hx-get = %q", got)
	}
	trigger, _ := input.Attr("hx-trigger")
	if !strings.Contains(trigger, "delay:") {
		t.Errorf("hx-trigger = %q, want a debounce", trigger)
	}
	if got, _ := input.Attr("hx-target"); got != "#rows" {
		t.Errorf("hx-target = %q", got)
	}
	// The rows list must carry the id the input targets.
	if doc.Find("#rows").Length() != 1 {
		t.Error("no #rows element for the search box to target")
	}
}
