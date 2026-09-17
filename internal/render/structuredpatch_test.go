package render

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/llimllib/spireweb/internal/session"
)

// writeClaudeFixture writes a Claude Code session, whose records differ from
// pi's in every way that matters here: no header, tool results arriving as
// user messages, and the diff on the record rather than in the message.
func writeClaudeFixture(t *testing.T, lines ...string) *session.Session {
	t.Helper()
	p := filepath.Join(t.TempDir(), "5b8b4bea.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := session.Parse(p)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The whole chain: a Claude Code file parses, the record's toolUseResult
// reaches Details, and Tool renders it as a diff instead of as arguments.
//
// toolUseResult sits on the record rather than inside the message, so this is
// also what pins Raw and Details keeping the envelope.
func TestToolRendersAClaudeEdit(t *testing.T) {
	s := writeClaudeFixture(t,
		`{"type":"assistant","sessionId":"s","timestamp":"2026-09-11T13:35:05Z","cwd":"/tmp",`+
			`"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Edit",`+
			`"input":{"file_path":"/tmp/a.json","old_string":"true","new_string":"false"}}]}}`,
		`{"type":"user","sessionId":"s","timestamp":"2026-09-11T13:35:06Z","cwd":"/tmp",`+
			`"toolUseResult":{"structuredPatch":[{"oldStart":11,"oldLines":1,"newStart":11,"newLines":1,`+
			`"lines":["-  \"flag\": true","+  \"flag\": false"]}]},`+
			`"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1",`+
			`"content":"Applied 1 edit"}]}}`,
	)

	d, err := Tool(s, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Diff) != 2 {
		t.Fatalf("Diff = %+v, want two lines", d.Diff)
	}
	if d.Diff[0].Kind != DiffDel || d.Diff[0].Num != "11" {
		t.Errorf("first line = %+v, want a deletion at 11", d.Diff[0])
	}
	if d.Diff[1].Kind != DiffAdd || d.Diff[1].Num != "11" {
		t.Errorf("second line = %+v, want an addition at 11", d.Diff[1])
	}
	// The arguments are the same edit as escaped JSON.
	if d.Args != "" {
		t.Errorf("Args = %q, want them hidden behind the diff", d.Args)
	}
	if d.Output != "Applied 1 edit" {
		t.Errorf("Output = %q", d.Output)
	}
}

// A failed edit records no patch, and there the arguments are the useful part:
// the text that could not be found.
func TestToolShowsArgumentsForAFailedClaudeEdit(t *testing.T) {
	s := writeClaudeFixture(t,
		`{"type":"assistant","sessionId":"s","timestamp":"2026-09-11T13:35:05Z","cwd":"/tmp",`+
			`"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Edit",`+
			`"input":{"file_path":"/tmp/a.json","old_string":"missing text"}}]}}`,
		`{"type":"user","sessionId":"s","timestamp":"2026-09-11T13:35:06Z","cwd":"/tmp",`+
			`"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1",`+
			`"content":"String not found","is_error":true}]}}`,
	)

	d, err := Tool(s, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Diff) != 0 {
		t.Errorf("Diff = %+v, want none", d.Diff)
	}
	if !strings.Contains(d.Args, "missing text") {
		t.Errorf("Args = %q, want the text that was not found", d.Args)
	}
	if !d.IsError {
		t.Error("IsError = false")
	}
}

// A real patch from the corpus. The single number column follows pi's: a
// deleted line is numbered in the old file, everything else in the new one.
func TestParseStructuredPatch(t *testing.T) {
	details := json.RawMessage(`{"structuredPatch":[{
		"oldStart":11,"oldLines":5,"newStart":11,"newLines":6,
		"lines":[
		  "     \"pyright-lsp@claude-plugins-official\": true",
		  "   },",
		  "   \"tui\": \"fullscreen\",",
		  "-  \"skipDangerousModePermissionPrompt\": true",
		  "+  \"skipDangerousModePermissionPrompt\": true,",
		  "+  \"disabledMcpjsonServers\": [\"linear\", \"LaunchDarkly\"]",
		  " }"]}]}`)

	lines, truncated := ParseDiff(details)
	if truncated {
		t.Error("a seven line patch was reported as clipped")
	}

	want := []DiffLine{
		{Kind: DiffContext, Num: "11", Text: `    "pyright-lsp@claude-plugins-official": true`},
		{Kind: DiffContext, Num: "12", Text: "  },"},
		{Kind: DiffContext, Num: "13", Text: `  "tui": "fullscreen",`},
		{Kind: DiffDel, Num: "14", Text: `  "skipDangerousModePermissionPrompt": true`},
		{Kind: DiffAdd, Num: "14", Text: `  "skipDangerousModePermissionPrompt": true,`},
		{Kind: DiffAdd, Num: "15", Text: `  "disabledMcpjsonServers": ["linear", "LaunchDarkly"]`},
		{Kind: DiffContext, Num: "16", Text: "}"},
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d: %+v", len(lines), len(want), lines)
	}
	for i, w := range want {
		if lines[i] != w {
			t.Errorf("line %d = %+v, want %+v", i, lines[i], w)
		}
	}
}

// Numbering has to diverge once a hunk adds or removes lines: after two
// deletions the old file is two ahead of the new one.
func TestParseStructuredPatchTracksBothFilesSeparately(t *testing.T) {
	details := json.RawMessage(`{"structuredPatch":[{
		"oldStart":100,"oldLines":4,"newStart":100,"newLines":2,
		"lines":[" keep","-gone one","-gone two"," after"]}]}`)

	lines, _ := ParseDiff(details)
	want := []DiffLine{
		{Kind: DiffContext, Num: "100", Text: "keep"},
		{Kind: DiffDel, Num: "101", Text: "gone one"},
		{Kind: DiffDel, Num: "102", Text: "gone two"},
		// New file: only one line preceded this one, so it is 101 there even
		// though it is 103 in the old file.
		{Kind: DiffContext, Num: "101", Text: "after"},
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d: %+v", len(lines), len(want), lines)
	}
	for i, w := range want {
		if lines[i] != w {
			t.Errorf("line %d = %+v, want %+v", i, lines[i], w)
		}
	}
}

// The space between two hunks is skipped unchanged lines, which is what pi's
// "..." means, so it renders the same way.
func TestParseStructuredPatchSeparatesHunksWithAGap(t *testing.T) {
	details := json.RawMessage(`{"structuredPatch":[
		{"oldStart":1,"oldLines":1,"newStart":1,"newLines":1,"lines":["-a","+b"]},
		{"oldStart":90,"oldLines":1,"newStart":90,"newLines":1,"lines":["-y","+z"]}]}`)

	lines, _ := ParseDiff(details)
	if len(lines) != 5 {
		t.Fatalf("got %d lines, want 5: %+v", len(lines), lines)
	}
	if lines[2].Kind != DiffGap {
		t.Errorf("line 2 = %+v, want a gap between the hunks", lines[2])
	}
	if lines[3].Num != "90" {
		t.Errorf("second hunk starts at %q, want 90", lines[3].Num)
	}
}

// "\ No newline at end of file" describes the line before it rather than being
// a line of the file, so it takes no number and must not shift the ones after.
func TestParseStructuredPatchHandlesNoNewlineMarker(t *testing.T) {
	details := json.RawMessage(`{"structuredPatch":[{
		"oldStart":1,"oldLines":2,"newStart":1,"newLines":2,
		"lines":["-old","\\ No newline at end of file","+new"]}]}`)

	lines, _ := ParseDiff(details)
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3: %+v", len(lines), lines)
	}
	if lines[1].Num != "" {
		t.Errorf("marker line numbered %q, want none", lines[1].Num)
	}
	if lines[2].Kind != DiffAdd || lines[2].Num != "1" {
		t.Errorf("line after the marker = %+v, want an addition at 1", lines[2])
	}
}

// Every other tool records something under toolUseResult, and none of it is a
// diff.
func TestParseDiffIgnoresOtherClaudeToolResults(t *testing.T) {
	cases := map[string]string{
		"bash":        `{"stdout":"ok","stderr":"","interrupted":false,"isImage":false}`,
		"read":        `{"type":"text","file":{"filePath":"/a.go","numLines":10}}`,
		"failed edit": `{"structuredPatch":[]}`,
	}
	for name, raw := range cases {
		if lines, _ := ParseDiff(json.RawMessage(raw)); lines != nil {
			t.Errorf("%s: got %d lines, want none", name, len(lines))
		}
	}
}
