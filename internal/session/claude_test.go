package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeClaude writes a Claude Code session file. The lines are given verbatim
// because the shapes here are copied from real sessions, and normalizing them
// through a helper would lose the thing being tested.
func writeClaude(t *testing.T, lines ...string) *Session {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "5b8b4bea-6e18-4ebe-8c31-3e7133e1d730.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := ParseWithRaw(p)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const (
	claudeUser = `{"type":"user","sessionId":"5b8b4bea-6e18-4ebe-8c31-3e7133e1d730",` +
		`"timestamp":"2026-09-11T13:35:05.056Z","cwd":"/Users/x/code/spireweb",` +
		`"version":"2.1.83","entrypoint":"cli","gitBranch":"main",` +
		`"message":{"role":"user","content":[{"type":"text","text":"how do I pin a dependency"}]}}`

	claudeAssistant = `{"type":"assistant","sessionId":"5b8b4bea-6e18-4ebe-8c31-3e7133e1d730",` +
		`"timestamp":"2026-09-11T13:35:09.000Z","cwd":"/Users/x/code/spireweb","entrypoint":"cli",` +
		`"message":{"role":"assistant","content":[{"type":"text","text":"Pin it in the lockfile."}]}}`
)

// There is no header line, so the first record has to be parsed rather than
// consumed, and the session's identity comes out of it.
func TestParseClaudeReadsMetadataFromTheFirstRecord(t *testing.T) {
	s := writeClaude(t, claudeUser, claudeAssistant)

	if s.ID != "5b8b4bea-6e18-4ebe-8c31-3e7133e1d730" {
		t.Errorf("ID = %q", s.ID)
	}
	if s.CWD != "/Users/x/code/spireweb" {
		t.Errorf("CWD = %q", s.CWD)
	}
	if s.StartedAt.IsZero() {
		t.Error("StartedAt is zero")
	}
	if len(s.Messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(s.Messages))
	}
	if got := s.Preview(50); got != "how do I pin a dependency" {
		t.Errorf("Preview = %q", got)
	}
	if got := s.Reply(50); got != "Pin it in the lockfile." {
		t.Errorf("Reply = %q", got)
	}
}

// Two thirds of the records in the corpus are neither user nor assistant.
// Skipping them is not an error, the same way pi's model_change is not.
func TestParseClaudeSkipsNonConversationRecords(t *testing.T) {
	s := writeClaude(t,
		claudeUser,
		`{"type":"attachment","sessionId":"5b8b4bea-6e18-4ebe-8c31-3e7133e1d730","attachment":{"type":"model"}}`,
		`{"type":"atis-latch","sessionId":"5b8b4bea-6e18-4ebe-8c31-3e7133e1d730","atis":{}}`,
		`{"type":"queue-operation","sessionId":"5b8b4bea-6e18-4ebe-8c31-3e7133e1d730","operation":"x"}`,
		claudeAssistant,
	)
	if len(s.Messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(s.Messages))
	}
	if s.SkippedLines != 0 {
		t.Errorf("SkippedLines = %d, want 0: unknown record types are expected", s.SkippedLines)
	}
}

// Older versions wrote user content as a bare string rather than an array of
// blocks: 1369 records in the corpus.
func TestParseClaudeAcceptsStringContent(t *testing.T) {
	s := writeClaude(t, `{"type":"user","sessionId":"s","timestamp":"2026-09-11T13:35:05Z",`+
		`"cwd":"/tmp","message":{"role":"user","content":"just a string"}}`)

	if len(s.Messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(s.Messages))
	}
	if got := s.Preview(50); got != "just a string" {
		t.Errorf("Preview = %q", got)
	}
}

// A user record answering several tool calls becomes several messages, because
// ToolCallID is singular and ToolResults keys by it.
func TestParseClaudeFansOutParallelToolResults(t *testing.T) {
	s := writeClaude(t,
		`{"type":"assistant","sessionId":"s","timestamp":"2026-09-11T13:35:05Z","cwd":"/tmp",`+
			`"message":{"role":"assistant","content":[`+
			`{"type":"tool_use","id":"toolu_a","name":"grep","input":{"pattern":"x"}},`+
			`{"type":"tool_use","id":"toolu_b","name":"find","input":{"glob":"*.go"}}]}}`,
		`{"type":"user","sessionId":"s","timestamp":"2026-09-11T13:35:06Z","cwd":"/tmp",`+
			`"toolUseResult":{"structuredPatch":[]},`+
			`"message":{"role":"user","content":[`+
			`{"type":"tool_result","tool_use_id":"toolu_a","content":"No matches found"},`+
			`{"type":"tool_result","tool_use_id":"toolu_b","content":"a.go\nb.go","is_error":true}]}}`,
	)

	if len(s.Messages) != 3 {
		t.Fatalf("got %d messages, want 3 (one assistant, two results)", len(s.Messages))
	}

	results := s.ToolResults()
	for _, id := range []string{"toolu_a", "toolu_b"} {
		if _, ok := results[id]; !ok {
			t.Errorf("no result for %s; a parallel call lost its output", id)
		}
	}
	if !results["toolu_b"].IsError {
		t.Error("toolu_b: IsError = false, want true")
	}
	if got := results["toolu_a"].Content[0].Text; got != "No matches found" {
		t.Errorf("toolu_a content = %q", got)
	}
}

// archiveMessages decides what is already stored by counting rows, so every
// message a record produces has to carry Raw. A gap would make the count
// disagree with the message list on every subsequent build.
func TestParseClaudeGivesEveryFannedMessageRawBytes(t *testing.T) {
	s := writeClaude(t,
		`{"type":"user","sessionId":"s","timestamp":"2026-09-11T13:35:06Z","cwd":"/tmp",`+
			`"message":{"role":"user","content":[`+
			`{"type":"tool_result","tool_use_id":"a","content":"one"},`+
			`{"type":"tool_result","tool_use_id":"b","content":"two"}]}}`,
	)
	for i, m := range s.Messages {
		if len(m.Raw) == 0 {
			t.Errorf("message %d has no Raw", i)
		}
	}
}

// The archive stores Raw verbatim, and toolUseResult -- which carries the diff
// an edit applied -- sits on the record rather than inside the message. Keeping
// only the message would make the archive lossy.
func TestParseClaudeRawKeepsTheWholeRecord(t *testing.T) {
	s := writeClaude(t, claudeUser)
	if len(s.Messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(s.Messages))
	}

	var got map[string]any
	if err := json.Unmarshal(s.Messages[0].Raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"sessionId", "cwd", "entrypoint", "gitBranch", "message"} {
		if _, ok := got[key]; !ok {
			t.Errorf("Raw is missing %q; it should be the whole record", key)
		}
	}
}

// Details is where the diff renderer will look for structuredPatch.
func TestParseClaudeCarriesToolUseResultAsDetails(t *testing.T) {
	s := writeClaude(t,
		`{"type":"user","sessionId":"s","timestamp":"2026-09-11T13:35:06Z","cwd":"/tmp",`+
			`"toolUseResult":{"structuredPatch":[{"oldStart":11,"newStart":11,"lines":["-a","+b"]}]},`+
			`"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"ok"}]}}`,
	)
	m := s.ToolResults()["a"]
	if m == nil {
		t.Fatal("no result for a")
	}
	if !strings.Contains(string(m.Details), "structuredPatch") {
		t.Errorf("Details = %q, want the record's toolUseResult", m.Details)
	}
}

// Thinking blocks here carry the reasoning, unlike pi's, where the model
// returns a signature. Prose still excludes them, but they have to survive
// parsing for the transcript to show them.
func TestParseClaudeKeepsThinking(t *testing.T) {
	s := writeClaude(t,
		`{"type":"assistant","sessionId":"s","timestamp":"2026-09-11T13:35:05Z","cwd":"/tmp",`+
			`"message":{"role":"assistant","content":[{"type":"thinking","thinking":"weighing options"}]}}`,
	)
	if len(s.Messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(s.Messages))
	}
	b := s.Messages[0].Content[0]
	if b.Type != BlockThinking || b.Thinking != "weighing options" {
		t.Errorf("block = %+v", b)
	}
	if len(s.Prose()) != 0 {
		t.Errorf("Prose() = %v, want thinking excluded", s.Prose())
	}
}

// Tool results are not prose, here as in pi: they are command output and file
// dumps, and they dwarf the conversation.
func TestParseClaudeProseExcludesToolResults(t *testing.T) {
	s := writeClaude(t, claudeUser,
		`{"type":"user","sessionId":"s","timestamp":"2026-09-11T13:35:06Z","cwd":"/tmp",`+
			`"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"SECRET FILE CONTENTS"}]}}`,
	)
	for _, b := range s.Prose() {
		if strings.Contains(b.Text, "SECRET") {
			t.Errorf("Prose() included a tool result: %q", b.Text)
		}
	}
}

// A file that is neither format is still an error. Detection must not turn a
// corrupt pi session into an empty Claude one.
func TestParseRejectsNeitherFormat(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.jsonl")
	if err := os.WriteFile(p, []byte(`{"type":"something-else","x":1}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(p); err == nil {
		t.Error("Parse() = nil error, want a complaint about the header")
	}
}

// pi sessions must keep parsing exactly as before.
func TestParseStillReadsPiSessions(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "pi.jsonl")
	body := `{"type":"session","version":1,"id":"pi-1","timestamp":"2026-09-01T10:00:00Z","cwd":"/tmp/demo"}` + "\n" +
		`{"type":"message","timestamp":"2026-09-01T10:00:01Z","message":{"role":"user","content":[{"type":"text","text":"hello pi"}]}}` + "\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Parse(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "pi-1" || s.Preview(20) != "hello pi" {
		t.Errorf("ID = %q, Preview = %q", s.ID, s.Preview(20))
	}
}
