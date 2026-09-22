package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLines(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func claudeLine(entrypoint, text string) string {
	return fmt.Sprintf(`{"type":"user","sessionId":"s","timestamp":"2026-09-11T13:35:05Z",`+
		`"cwd":"/tmp","entrypoint":%q,"message":{"role":"user","content":[{"type":"text","text":%q}]}}`,
		entrypoint, text)
}

func TestSkipReason(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  string
	}{
		{"interactive claude session", []string{claudeLine("cli", "hello")}, ""},
		{"title prompt from claude -p", []string{claudeLine("sdk-cli", "write a title")}, SkipSDK},
		{"pi through claude-bridge", []string{claudeLine("sdk-ts", "hello")}, SkipSDK},
		{
			// pi's own format has no entrypoint anywhere and must never be
			// excluded by a rule about Claude Code.
			"pi session",
			[]string{
				`{"type":"session","version":1,"id":"p","timestamp":"2026-09-01T10:00:00Z","cwd":"/tmp"}`,
				`{"type":"message","timestamp":"2026-09-01T10:00:01Z","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`,
			},
			"",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SkipReason(writeLines(t, tc.lines...)); got != tc.want {
				t.Errorf("SkipReason = %q, want %q", got, tc.want)
			}
		})
	}
}

// The case a bounded prefix scan gets wrong. 94 bridge sessions in the corpus
// open with cli records before the bridge takes over, the deepest at record
// 429. Admitting one indexes a two-message fragment of a session that is
// already in the index under pi.
func TestSkipReasonScansPastALongCLIPrefix(t *testing.T) {
	lines := make([]string, 0, 431)
	for i := 0; i < 429; i++ {
		lines = append(lines, claudeLine("cli", fmt.Sprintf("message %d", i)))
	}
	lines = append(lines, claudeLine("sdk-ts", "the bridge takes over"))

	if got := SkipReason(writeLines(t, lines...)); got != SkipSDK {
		t.Errorf("SkipReason = %q, want %q: a prefix scan would have missed this", got, SkipSDK)
	}
}

// The literal only ever appears as a key: inside a JSON string the quotes are
// escaped, so a conversation about the field cannot disqualify the session
// that discusses it.
func TestSkipReasonIgnoresTheKeyAppearingInContent(t *testing.T) {
	p := writeLines(t, claudeLine("cli", `look for "entrypoint":"sdk-ts" in the file`))
	if got := SkipReason(p); got != "" {
		t.Errorf("SkipReason = %q, want \"\": the mention is escaped content, not a key", got)
	}
}

// The records Claude Code writes for a session someone opened, ran a slash
// command in, and quit. Taken from a real file: none of them is a user or
// assistant record, so the session parses to no messages at all and reaches
// the list as a row with no preview and nothing to title.
var slashCommandOnly = []string{
	`{"type":"mode","mode":"normal","sessionId":"f1ecd8cc"}`,
	`{"type":"permission-mode","permissionMode":"default","sessionId":"f1ecd8cc"}`,
	`{"type":"system","subtype":"local_command","uuid":"6762df6f","parentUuid":null,` +
		`"entrypoint":"cli","sessionId":"f1ecd8cc","cwd":"/tmp",` +
		`"content":"<command-name>/model</command-name>"}`,
	`{"type":"system","subtype":"local_command","uuid":"a0b33bd3","parentUuid":"6762df6f",` +
		`"entrypoint":"cli","sessionId":"f1ecd8cc","cwd":"/tmp",` +
		`"content":"<local-command-stdout>Kept model as ` + "`Opus 5.5`" + `</local-command-stdout>"}`,
	`{"type":"cost-state","sessionId":"f1ecd8cc"}`,
	`{"type":"last-prompt","leafUuid":"a0b33bd3","sessionId":"f1ecd8cc"}`,
}

func TestSkipParsedReason(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  string
	}{
		{"slash command and nothing else", slashCommandOnly, SkipEmpty},
		{"a conversation", []string{claudeLine("cli", "hello")}, ""},
		{
			"a pi session",
			[]string{
				`{"type":"session","version":1,"id":"p","timestamp":"2026-09-01T10:00:00Z","cwd":"/tmp"}`,
				`{"type":"message","timestamp":"2026-09-01T10:00:01Z","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`,
			},
			"",
		},
		{
			// Nothing in it yet. Excluding it is right for exactly as long as
			// that is true; the next write is what brings it back.
			"a pi session with only a header",
			[]string{`{"type":"session","version":1,"id":"p","timestamp":"2026-09-01T10:00:00Z","cwd":"/tmp"}`},
			SkipEmpty,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Parse(writeLines(t, tc.lines...))
			if err != nil {
				t.Fatal(err)
			}
			if got := SkipParsedReason(s); got != tc.want {
				t.Errorf("SkipParsedReason = %q, want %q (%d messages)", got, tc.want, len(s.Messages))
			}
		})
	}
}

// The slash-command file is a real cli session, so the byte-level rule has no
// opinion on it. Only the parse can say it is empty, which is why the two
// tests exist separately.
func TestSkipReasonKeepsSlashCommandSessions(t *testing.T) {
	if got := SkipReason(writeLines(t, slashCommandOnly...)); got != "" {
		t.Errorf("SkipReason = %q, want \"\": entrypoint is cli", got)
	}
}

// An unreadable file is the parser's problem to report, so that one code path
// explains it rather than two.
func TestSkipReasonDoesNotClaimMissingFiles(t *testing.T) {
	if got := SkipReason(filepath.Join(t.TempDir(), "absent.jsonl")); got != "" {
		t.Errorf("SkipReason = %q, want \"\"", got)
	}
}
