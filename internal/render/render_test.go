package render

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/llimllib/spireweb/internal/session"
)

// The one bug in this package that would be a vulnerability rather than a
// defect. Session content is arbitrary text and routinely contains HTML and
// JavaScript, because pasting both into an agent is an ordinary thing to do.
func TestMarkdownEscapesRawHTML(t *testing.T) {
	cases := []string{
		`<script>alert('xss')</script>`,
		`<img src=x onerror=alert(1)>`,
		`<iframe src="evil"></iframe>`,
		`Here is some markdown with <b>inline html</b> in it.`,
		`<div onclick="steal()">click</div>`,
	}
	// Substring checks are the wrong tool: escaped output legitimately
	// contains the text "onerror=". What matters is whether the browser would
	// build those nodes, so parse the result and look at the tree.
	dangerous := map[string]bool{
		"script": true, "iframe": true, "img": true, "object": true,
		"embed": true, "svg": true, "div": true, "b": true,
	}

	for _, src := range cases {
		got := string(Markdown(src))
		doc, err := html.Parse(strings.NewReader(got))
		if err != nil {
			t.Fatal(err)
		}
		var walk func(*html.Node)
		walk = func(n *html.Node) {
			if n.Type == html.ElementNode {
				if dangerous[n.Data] {
					t.Errorf("rendered a live <%s> element\n in: %s\nout: %s", n.Data, src, got)
				}
				for _, a := range n.Attr {
					if strings.HasPrefix(a.Key, "on") {
						t.Errorf("rendered a live %s handler\n in: %s\nout: %s", a.Key, src, got)
					}
				}
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
		}
		walk(doc)

		// Escaped, not discarded. goldmark's safe default drops raw HTML and
		// leaves "<!-- raw HTML omitted -->", which loses content that
		// sessions frequently contain as subject matter.
		if !strings.Contains(got, "&lt;") {
			t.Errorf("expected escaped angle brackets:\n in: %s\nout: %s", src, got)
		}
		if strings.Contains(got, "raw HTML omitted") {
			t.Errorf("content discarded rather than escaped:\n in: %s\nout: %s", src, got)
		}
	}
}

func TestMarkdownRendersOrdinaryMarkdown(t *testing.T) {
	got := string(Markdown("# Title\n\nSome **bold** text and `code`.\n"))
	for _, want := range []string{"<h1", "<strong>bold</strong>", "<code>code</code>"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestMarkdownHighlightsFencedCode(t *testing.T) {
	got := string(Markdown("```go\nfunc main() {}\n```\n"))
	// Classes rather than inline styles, so one stylesheet restyles every
	// block and dark mode is a media query.
	if !strings.Contains(got, "class=\"chroma\"") {
		t.Errorf("expected chroma classes, got:\n%s", got)
	}
	if strings.Contains(got, "style=\"color:") {
		t.Errorf("expected classes, not inline styles:\n%s", got)
	}
}

func TestChromaCSSHasBothThemes(t *testing.T) {
	css := ChromaCSS()
	if !strings.Contains(css, "@media (prefers-color-scheme: dark)") {
		t.Error("no dark theme block")
	}
	if strings.Count(css, "/* Keyword */") < 2 {
		t.Error("expected the same classes emitted twice, once per theme")
	}
}

func TestToolSummary(t *testing.T) {
	cases := []struct {
		name string
		args string
		want string
	}{
		{"bash", `{"command":"git log --oneline | head"}`, "git log --oneline | head"},
		{"read", `{"path":"/etc/hosts"}`, "/etc/hosts"},
		{"grep", `{"pattern":"TODO","path":"/src"}`, "TODO"},
		// Unknown tool: the shortest string value is almost always the
		// identifying one rather than a body of content.
		{"mystery", `{"body":"a very long thing indeed here","id":"x1"}`, "x1"},
		{"bash", `{}`, ""},
		{"bash", `not json`, ""},
		{"read", `{"path":""}`, ""},
	}
	for _, c := range cases {
		got := ToolSummary(c.name, json.RawMessage(c.args))
		if got != c.want {
			t.Errorf("ToolSummary(%q, %s) = %q, want %q", c.name, c.args, got, c.want)
		}
	}
}

func TestToolSummaryCollapsesAndTruncates(t *testing.T) {
	long := strings.Repeat("word ", 100)
	got := ToolSummary("bash", json.RawMessage(`{"command":"`+long+`"}`))
	if strings.Contains(got, "\n") || strings.Contains(got, "  ") {
		t.Errorf("whitespace not collapsed: %q", got)
	}
	if len([]rune(got)) > 160 {
		t.Errorf("got %d runes, want <= 160", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected an ellipsis, got %q", got)
	}
}

func writeFixture(t *testing.T, lines ...string) *session.Session {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "2026-03-31T12-26-01-076Z_fix.jsonl")
	header := `{"type":"session","version":3,"id":"fix","timestamp":"2026-03-31T12:26:01.076Z","cwd":"/Users/me/code/proj"}`
	body := header + "\n" + strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := session.Parse(p)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTranscriptOrdersProseAndToolCalls(t *testing.T) {
	s := writeFixture(t,
		`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"what changed"}]}}`,
		`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"Looking."},{"type":"toolCall","id":"tc1","name":"bash","arguments":{"command":"git log"}},{"type":"text","text":"Two commits."}]}}`,
		`{"type":"message","message":{"role":"toolResult","toolCallId":"tc1","toolName":"bash","content":[{"type":"text","text":"abc123"}]}}`,
	)

	entries := Transcript(s)
	var kinds []string
	for _, e := range entries {
		kinds = append(kinds, e.Kind)
	}
	// A tool call sits between the prose that surrounds it, because that is
	// the order it happened in; nesting calls under the message would move it.
	want := []string{KindUser, KindAssistant, KindTool, KindAssistant}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}

	tool := entries[2].Tool
	if tool.Name != "bash" || tool.Summary != "git log" {
		t.Errorf("tool = %+v", tool)
	}
	if !tool.HasResult {
		t.Error("result not paired with its call")
	}
	// The output itself must not be in the transcript; it is fetched on
	// expand, which is the whole point of the tool route.
	for _, e := range entries {
		if strings.Contains(string(e.HTML), "abc123") {
			t.Error("tool output was inlined into the transcript")
		}
	}
}

func TestTranscriptSkipsEmptyAndToolResults(t *testing.T) {
	s := writeFixture(t,
		`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"  "}]}}`,
		`{"type":"message","message":{"role":"assistant","content":[{"type":"thinking","thinking":"","thinkingSignature":"abc"}]}}`,
		`{"type":"message","message":{"role":"toolResult","toolCallId":"orphan","content":[{"type":"text","text":"output"}]}}`,
		`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"real"}]}}`,
	)
	entries := Transcript(s)
	if len(entries) != 1 || entries[0].Kind != KindUser {
		t.Fatalf("entries = %+v, want one user entry", entries)
	}
}

func TestTranscriptMarksMissingResult(t *testing.T) {
	// An interrupted session ends with a call whose result never arrived.
	s := writeFixture(t,
		`{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","id":"tc1","name":"bash","arguments":{"command":"sleep 100"}}]}}`,
	)
	entries := Transcript(s)
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
	if entries[0].Tool.HasResult {
		t.Error("HasResult set for a call with no result")
	}
}

func TestToolDetail(t *testing.T) {
	s := writeFixture(t,
		`{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","id":"tc1","name":"bash","arguments":{"command":"ls"}}]}}`,
		`{"type":"message","message":{"role":"toolResult","toolCallId":"tc1","isError":true,"content":[{"type":"text","text":"no such file"}]}}`,
	)
	d, err := Tool(s, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Args, `"command"`) {
		t.Errorf("args = %q", d.Args)
	}
	if d.Output != "no such file" || !d.IsError {
		t.Errorf("detail = %+v", d)
	}

	for _, bad := range [][2]int{{9, 0}, {0, 9}, {-1, 0}} {
		if _, err := Tool(s, bad[0], bad[1]); err == nil {
			t.Errorf("Tool(%d, %d) succeeded, want an error", bad[0], bad[1])
		}
	}
}

func TestToolDetailClipsLongOutput(t *testing.T) {
	// Multi-byte characters, so a naive byte cut would produce invalid UTF-8.
	big, err := json.Marshal(strings.Repeat("日本語テキスト", 20000))
	if err != nil {
		t.Fatal(err)
	}
	s := writeFixture(t,
		`{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","id":"tc1","name":"read","arguments":{"path":"/big"}}]}}`,
		`{"type":"message","message":{"role":"toolResult","toolCallId":"tc1","content":[{"type":"text","text":`+string(big)+`}]}}`,
	)
	d, err := Tool(s, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Truncated {
		t.Fatal("long output not marked truncated")
	}
	if len(d.Output) > MaxOutputBytes {
		t.Errorf("output is %d bytes, over the %d cap", len(d.Output), MaxOutputBytes)
	}
	if !utf8Valid(d.Output) {
		t.Error("clipping split a multi-byte character")
	}
	if d.FullBytes <= len(d.Output) {
		t.Errorf("FullBytes = %d, should exceed the clipped length", d.FullBytes)
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '\uFFFD' {
			return false
		}
	}
	return true
}

// The diff pi records is the same text its terminal draws: a marker column, a
// right-aligned file line number, then the line itself.
func TestParseDiffSplitsColumns(t *testing.T) {
	details := json.RawMessage(`{"diff":` + quoteJSON(strings.Join([]string{
		`     ...`,
		`  45     parse_filters,`,
		`- 46 from core.api import (`,
		`+ 46 from core.api import ALLOWED`,
		`  47 `,
		`     ...`,
	}, "\n")) + `,"firstChangedLine":46}`)

	lines, truncated := ParseDiff(details)
	if truncated {
		t.Error("a six line diff was reported as clipped")
	}
	if len(lines) != 6 {
		t.Fatalf("got %d lines, want 6", len(lines))
	}

	want := []DiffLine{
		{Kind: DiffGap},
		{Kind: DiffContext, Num: "45", Text: "    parse_filters,"},
		{Kind: DiffDel, Num: "46", Text: "from core.api import ("},
		{Kind: DiffAdd, Num: "46", Text: "from core.api import ALLOWED"},
		{Kind: DiffContext, Num: "47", Text: ""},
		{Kind: DiffGap},
	}
	for i, w := range want {
		if lines[i] != w {
			t.Errorf("line %d = %+v, want %+v", i, lines[i], w)
		}
	}
	// Indentation is code, so it survives the split exactly.
	if lines[1].Text != "    parse_filters," {
		t.Errorf("indentation lost: %q", lines[1].Text)
	}
	// Colour is not the only signal.
	if lines[2].Mark() != "-" || lines[3].Mark() != "+" {
		t.Errorf("marks = %q %q", lines[2].Mark(), lines[3].Mark())
	}
}

func TestParseDiffIgnoresResultsWithoutOne(t *testing.T) {
	cases := map[string]string{
		"failed edit":   `{}`,
		"another tool":  `{"pattern":"foo","matchCount":3}`,
		"not an object": `"nope"`,
	}
	for name, raw := range cases {
		if lines, _ := ParseDiff(json.RawMessage(raw)); lines != nil {
			t.Errorf("%s: got %d lines, want none", name, len(lines))
		}
	}
	if lines, _ := ParseDiff(nil); lines != nil {
		t.Error("absent details produced lines")
	}
}

// An edit's diff says everything its arguments do, with the file's line
// numbers and its surrounding lines. Showing both would mean scrolling past
// JSON full of escaped newlines to reach the readable version.
func TestToolDetailPrefersTheDiffOverArguments(t *testing.T) {
	s := writeFixture(t,
		`{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","id":"tc1","name":"edit","arguments":{"path":"a.py","edits":[{"oldText":"x = 1","newText":"x = 2"}]}}]}}`,
		`{"type":"message","message":{"role":"toolResult","toolCallId":"tc1","toolName":"edit","content":[{"type":"text","text":"Successfully replaced 1 block(s) in a.py."}],"details":{"diff":"-  7 x = 1\n+  7 x = 2"}}}`,
	)
	d, err := Tool(s, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Diff) != 2 {
		t.Fatalf("diff = %+v, want two lines", d.Diff)
	}
	if d.Args != "" {
		t.Errorf("args = %q, want the diff to stand alone", d.Args)
	}
}

// A failed edit records no diff, and there its arguments are the useful
// thing: they are the text that could not be found.
func TestToolDetailKeepsArgumentsWhenAnEditFails(t *testing.T) {
	s := writeFixture(t,
		`{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","id":"tc1","name":"edit","arguments":{"path":"a.py","edits":[{"oldText":"missing","newText":"x"}]}}]}}`,
		`{"type":"message","message":{"role":"toolResult","toolCallId":"tc1","toolName":"edit","isError":true,"content":[{"type":"text","text":"Could not find edits[0] in a.py."}],"details":{}}}`,
	)
	d, err := Tool(s, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Diff) != 0 {
		t.Errorf("diff = %+v, want none", d.Diff)
	}
	if !strings.Contains(d.Args, "missing") {
		t.Errorf("args = %q, want the attempted edit shown", d.Args)
	}
}

// quoteJSON renders s as a JSON string literal.
func quoteJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// A chunk records the message it came from, so a message has to be
// addressable in the page. One message can render as several entries -- prose,
// a tool call, more prose -- and only the first of them may carry the id.
func TestTranscriptAnchorsEachMessageOnce(t *testing.T) {
	s := writeFixture(t,
		`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"what changed"}]}}`,
		`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"Looking."},{"type":"toolCall","id":"tc1","name":"bash","arguments":{"command":"git log"}},{"type":"text","text":"Two commits."}]}}`,
		`{"type":"message","message":{"role":"toolResult","toolCallId":"tc1","toolName":"bash","content":[{"type":"text","text":"abc123"}]}}`,
	)

	entries := Transcript(s)
	if len(entries) != 4 {
		t.Fatalf("got %d entries, want 4", len(entries))
	}

	// Message 0 is the user turn; the other three entries all come from
	// message 1.
	wantIdx := []int{0, 1, 1, 1}
	wantAnchor := []bool{true, true, false, false}
	for i, e := range entries {
		if e.MsgIdx != wantIdx[i] {
			t.Errorf("entry %d MsgIdx = %d, want %d", i, e.MsgIdx, wantIdx[i])
		}
		if e.Anchor != wantAnchor[i] {
			t.Errorf("entry %d Anchor = %v, want %v", i, e.Anchor, wantAnchor[i])
		}
	}
}

// A message whose blocks are all empty renders nothing, so the anchor has to
// land on whatever the next message produces rather than being skipped.
func TestTranscriptAnchorsSurviveEmptyMessages(t *testing.T) {
	s := writeFixture(t,
		`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"   "}]}}`,
		`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"real"}]}}`,
	)
	entries := Transcript(s)
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if !entries[0].Anchor || entries[0].MsgIdx != 1 {
		t.Errorf("entry = {MsgIdx:%d Anchor:%v}, want message 1 anchored",
			entries[0].MsgIdx, entries[0].Anchor)
	}
}
