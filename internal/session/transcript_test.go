package session

import (
	"strings"
	"testing"
)

// The TUI this code came from indexed prose and discarded everything else. A
// web transcript has to render tool calls and their output too, so parsing
// must retain the fields that pair them up.
func TestParseRetainsToolTraffic(t *testing.T) {
	p := writeSession(t,
		header,
		`{"type":"message","timestamp":"2026-03-31T12:26:01.076Z","message":{"role":"user","content":[{"type":"text","text":"what changed"}]}}`,
		`{"type":"message","timestamp":"2026-03-31T12:26:05.000Z","message":{"role":"assistant","content":[{"type":"text","text":"Let me look."},{"type":"toolCall","id":"tc_1","name":"bash","arguments":{"command":"git log --oneline"}}]}}`,
		`{"type":"message","timestamp":"2026-03-31T12:26:06.000Z","message":{"role":"toolResult","toolCallId":"tc_1","toolName":"bash","isError":false,"content":[{"type":"text","text":"2ea40de cli: add -version"}]}}`,
	)

	s, err := Parse(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 3 {
		t.Fatalf("got %d messages, want 3", len(s.Messages))
	}

	call := s.Messages[1].Content[1]
	if call.Type != BlockToolCall {
		t.Fatalf("block type = %q, want %q", call.Type, BlockToolCall)
	}
	if call.ID != "tc_1" || call.Name != "bash" {
		t.Errorf("tool call id/name = %q/%q, want tc_1/bash", call.ID, call.Name)
	}
	if !strings.Contains(string(call.Arguments), "git log") {
		t.Errorf("arguments = %s, want the command", call.Arguments)
	}

	result := s.Messages[2]
	if result.ToolCallID != "tc_1" || result.ToolName != "bash" || result.IsError {
		t.Errorf("result = %+v, want tc_1/bash/not-error", result)
	}

	// The record envelope's timestamp is the usable one: the message body's own
	// timestamp is a string on some roles and unix milliseconds on others.
	if s.Messages[0].At.IsZero() {
		t.Error("message timestamp not carried over from the record envelope")
	}
}

func TestToolResultsPairsByID(t *testing.T) {
	p := writeSession(t,
		header,
		`{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","id":"tc_1","name":"read","arguments":{}}]}}`,
		`{"type":"message","message":{"role":"toolResult","toolCallId":"tc_1","toolName":"read","content":[{"type":"text","text":"file body"}]}}`,
		`{"type":"message","message":{"role":"toolResult","toolCallId":"tc_missing","toolName":"read","content":[{"type":"text","text":"orphan"}]}}`,
	)
	s, err := Parse(p)
	if err != nil {
		t.Fatal(err)
	}

	byID := s.ToolResults()
	got, ok := byID["tc_1"]
	if !ok {
		t.Fatal("tc_1 has no result")
	}
	if got.Content[0].Text != "file body" {
		t.Errorf("result text = %q", got.Content[0].Text)
	}
	// An orphaned result still gets indexed; a transcript renders it on its own
	// rather than dropping it, since a truncated session can produce one.
	if _, ok := byID["tc_missing"]; !ok {
		t.Error("orphaned tool result was dropped")
	}
}

// Tool output is ~98% of a session's bytes and is mostly command output and
// file dumps. Indexing it would surface files that were read rather than
// discussions that happened.
func TestProseStillExcludesToolTraffic(t *testing.T) {
	p := writeSession(t,
		header,
		`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`,
		`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"hi"},{"type":"toolCall","id":"tc_1","name":"bash","arguments":{"command":"ls"}}]}}`,
		`{"type":"message","message":{"role":"toolResult","toolCallId":"tc_1","content":[{"type":"text","text":"secret tool output"}]}}`,
		`{"type":"message","message":{"role":"assistant","content":[{"type":"thinking","thinking":"pondering"}]}}`,
	)
	s, err := Parse(p)
	if err != nil {
		t.Fatal(err)
	}

	var texts []string
	for _, b := range s.Prose() {
		texts = append(texts, b.Text)
	}
	want := []string{"hello", "hi"}
	if len(texts) != len(want) {
		t.Fatalf("prose = %q, want %q", texts, want)
	}
	for i := range want {
		if texts[i] != want[i] {
			t.Errorf("prose[%d] = %q, want %q", i, texts[i], want[i])
		}
	}
}

func TestPreviewAndReply(t *testing.T) {
	p := writeSession(t,
		header,
		`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"system-ish preamble"}]}}`,
		`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"  the   question  "}]}}`,
		`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"the answer"}]}}`,
	)
	s, err := Parse(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Preview(100); got != "the question" {
		t.Errorf("Preview = %q, want %q", got, "the question")
	}
	// Reply takes the first assistant message even when it precedes the user's,
	// which is what makes the two list lines differ.
	if got := s.Reply(100); got != "system-ish preamble" {
		t.Errorf("Reply = %q, want %q", got, "system-ish preamble")
	}
	if got := s.Reply(7); got != "system…" {
		t.Errorf("Reply(7) = %q, want truncation", got)
	}
}

func TestPreviewAndReplyEmpty(t *testing.T) {
	p := writeSession(t,
		header,
		`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"only a question"}]}}`,
	)
	s, err := Parse(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Reply(100); got != "" {
		t.Errorf("Reply with no assistant message = %q, want empty", got)
	}
}
