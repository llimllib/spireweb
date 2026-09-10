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
