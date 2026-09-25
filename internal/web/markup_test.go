package web

import (
	"strings"
	"testing"
)

// app.ts addresses the page through selectors. Nothing in Go's type system
// connects the two, so renaming a class or an id in a template breaks the
// keyboard silently and no other test notices. This pins the contract.
//
// It is not a substitute for driving a browser -- it cannot tell whether j
// actually moves the cursor -- but it catches the drift that is most likely.
func TestMarkupContractForKeyboardNavigation(t *testing.T) {
	f := newFixture(t, map[string][]string{
		"aaa": {userMsg("one")},
		"bbb": {userMsg("two")},
	})
	_, doc := f.get(t, "/")

	// The cursor is DOM focus on a row anchor, so rows must be anchors and
	// must be focusable. An anchor without href is not.
	rows := doc.Find("a.row")
	if rows.Length() != 2 {
		t.Fatalf("a.row count = %d, want 2", rows.Length())
	}
	for i := range rows.Nodes {
		if href, ok := rows.Eq(i).Attr("href"); !ok || href == "" {
			t.Errorf("row %d has no href, so it cannot take focus", i)
		}
	}

	// Selected row is found via this attribute, so the cursor can start
	// where the reading pane is.
	if doc.Find(`a.row[aria-current="page"]`).Length() != 1 {
		t.Error(`no a.row[aria-current="page"] for the cursor to start from`)
	}

	// The scroll-restore target.
	if doc.Find(".list").Length() != 1 {
		t.Error("no .list pane for scroll restoration")
	}

	// Search focus target.
	if doc.Find(`input[name="q"]`).Length() != 1 {
		t.Error(`no input[name="q"] for / and Cmd-K to focus`)
	}

	// Help sheet and its opener.
	if doc.Find("dialog#help").Length() != 1 {
		t.Error("no dialog#help")
	}
	if doc.Find("#help-open").Length() != 1 {
		t.Error("no #help-open button")
	}

	// The htmx swap handler keys off this id to reset the scroll position.
	if doc.Find("#rows").Length() != 1 {
		t.Error("no #rows container")
	}

	// scrollToMatch reads the target off this element.
	if doc.Find("article.reading").Length() != 1 {
		t.Error("no article.reading for the match scroll to read data-scroll-to from")
	}
}

// The help sheet is the only discovery mechanism for j/k, so it should
// actually list them.
func TestHelpSheetListsTheBindings(t *testing.T) {
	f := newFixture(t, map[string][]string{"aaa": {userMsg("one")}})
	_, doc := f.get(t, "/")

	help := doc.Find("dialog#help").Text()
	for _, key := range []string{"j", "k", "G", "Enter", "/", "?"} {
		if !strings.Contains(help, key) {
			t.Errorf("help sheet does not mention %q", key)
		}
	}
	// Closing without JavaScript.
	if doc.Find(`dialog#help form[method="dialog"]`).Length() != 1 {
		t.Error("help sheet has no dialog-method form to close it")
	}
}

func TestEmptyIndexOffersTheFix(t *testing.T) {
	f := newFixture(t, map[string][]string{})
	_, doc := f.get(t, "/")

	blank := doc.Find(".blank")
	if blank.Length() != 1 {
		t.Fatal("no empty state in the reading pane")
	}
	// A command the reader can run, marked up as one rather than rendered as
	// literal backticks.
	if got := blank.Find("code").Text(); got != "spireweb index" {
		t.Errorf("empty state code = %q, want the command to run", got)
	}
}

// The favicon is a link in the layout and a file in the embedded static
// directory; either one alone is a blank tab.
func TestLayoutLinksTheFavicon(t *testing.T) {
	f := newFixture(t, map[string][]string{"aaa": {userMsg("hi")}})
	_, doc := f.get(t, "/")
	sel := doc.Find(`link[rel="icon"]`)
	if sel.Length() == 0 {
		t.Fatal("no favicon link in the layout")
	}
	if href, _ := sel.Attr("href"); href != "/static/favicon.svg" {
		t.Errorf("favicon href = %q", href)
	}
}

// Copy as markdown reads the source out of a <template> beside the prose. It
// has to be the text as written -- the point is to get back what rendering
// threw away -- and it has to be inert, because session content routinely
// contains HTML.
func TestTurnCarriesItsMarkdownSource(t *testing.T) {
	src := "Run `ls`, then:\n\n```html\n<script>alert(1)</script>\n```\n\n**done**"
	f := newFixture(t, map[string][]string{"aaa": {userMsg("hi"), assistantMsg(src)}})
	_, doc := f.get(t, "/sessions/aaa")

	turn := doc.Find(".turn-assistant")
	if turn.Find("button.turn-copy").Length() != 1 {
		t.Fatal("assistant turn has no button.turn-copy")
	}
	tpl := turn.Find("template.turn-source")
	if tpl.Length() != 1 {
		t.Fatal("assistant turn has no template.turn-source for the copy button to read")
	}
	if got := tpl.Text(); got != src {
		t.Errorf("template source = %q, want %q", got, src)
	}
	if tpl.Find("script").Length() != 0 {
		t.Error("markdown source was parsed as markup")
	}
}
