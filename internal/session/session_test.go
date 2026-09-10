package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestChunk(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		limit int
		want  []string
	}{
		{"empty", "", 10, nil},
		{"under limit", "hello world", 100, []string{"hello world"}},
		{"exact limit", "abcde", 5, []string{"abcde"}},
		{"splits on word boundary", "aaa bbb ccc ddd", 7, []string{"aaa bbb", "ccc ddd"}},
		{"oversized single word", "aaaaaaaaaa", 4, []string{"aaaa", "aaaa", "aa"}},
		{"oversized word among others", "hi aaaaaaaaaa bye", 4,
			[]string{"hi", "aaaa", "aaaa", "aa", "bye"}},
		{"zero limit falls back to default", "hello", 0, []string{"hello"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Chunk(tc.text, tc.limit)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d chunks %q, want %d %q", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("chunk %d: got %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// The limit is the whole point of the chunker: no chunk may exceed it, or
// embedding aborts at runtime.
func TestChunkNeverExceedsLimit(t *testing.T) {
	long := strings.Repeat("word ", 5000) +
		strings.Repeat("x", 3000) + // pathological unbroken run
		strings.Repeat(" tail", 500)
	for _, limit := range []int{1, 2, 13, 80, MaxChunkChars} {
		for i, c := range Chunk(long, limit) {
			if len(c) > limit {
				t.Fatalf("limit %d: chunk %d is %d bytes", limit, i, len(c))
			}
		}
	}
}

func TestChunkPreservesContent(t *testing.T) {
	text := "the quick brown fox jumps over the lazy dog"
	got := strings.Join(Chunk(text, 12), " ")
	if got != text {
		t.Errorf("content changed:\n got %q\nwant %q", got, text)
	}
}

const header = `{"type":"session","version":3,"id":"abc-123","timestamp":"2026-03-31T12:26:01.076Z","cwd":"/Users/me/code/myproj"}`

func writeSession(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "2026-03-31T12-26-01-076Z_abc-123.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParse(t *testing.T) {
	p := writeSession(t,
		header,
		`{"type":"model_change","provider":"x","modelId":"y"}`,
		`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"how do I  center a div?"}]}}`,
		`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"Use flexbox."},{"type":"toolCall","id":"t1","name":"bash"}]}}`,
		`{"type":"message","message":{"role":"toolResult","toolName":"bash","content":[{"type":"text","text":"lots of output"}]}}`,
		`{"type":"thinking_level_change","level":"low"}`,
		`{"type":"custom_message","content":"something"}`,
	)

	s, err := Parse(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "abc-123" {
		t.Errorf("ID = %q", s.ID)
	}
	if s.CWD != "/Users/me/code/myproj" {
		t.Errorf("CWD = %q", s.CWD)
	}
	if s.Project() != "code/myproj" {
		t.Errorf("Project() = %q, want code/myproj", s.Project())
	}
	if got, want := len(s.Messages), 3; got != want {
		t.Errorf("messages = %d, want %d", got, want)
	}
	if s.SkippedLines != 0 {
		t.Errorf("SkippedLines = %d, want 0", s.SkippedLines)
	}
	if s.StartedAt.Year() != 2026 {
		t.Errorf("StartedAt = %v", s.StartedAt)
	}

	// Prose excludes toolResult content and toolCall blocks, and collapses
	// internal whitespace.
	prose := s.Prose()
	if got, want := len(prose), 2; got != want {
		t.Fatalf("prose blocks = %d, want %d: %+v", got, want, prose)
	}
	if prose[0].Text != "how do I center a div?" {
		t.Errorf("prose[0] = %q (whitespace should collapse)", prose[0].Text)
	}
	if prose[1].Role != RoleAssistant || prose[1].Text != "Use flexbox." {
		t.Errorf("prose[1] = %+v", prose[1])
	}
	for _, b := range prose {
		if strings.Contains(b.Text, "lots of output") {
			t.Error("tool result leaked into prose")
		}
	}

	if got, want := s.Preview(80), "how do I center a div?"; got != want {
		t.Errorf("Preview() = %q, want %q", got, want)
	}
}

// A session pi is actively writing ends mid-line. That must not lose the
// messages that did parse.
func TestParseTruncatedFinalLine(t *testing.T) {
	p := writeSession(t,
		header,
		`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"complete"}]}}`,
		`{"type":"message","message":{"role":"assistant","conte`,
	)
	s, err := Parse(p)
	if err != nil {
		t.Fatalf("truncated line should not be fatal: %v", err)
	}
	if len(s.Messages) != 1 {
		t.Errorf("messages = %d, want 1", len(s.Messages))
	}
	if s.SkippedLines != 1 {
		t.Errorf("SkippedLines = %d, want 1", s.SkippedLines)
	}
}

func TestParseUnknownRecordType(t *testing.T) {
	p := writeSession(t,
		header,
		`{"type":"some_future_type","payload":{"nested":true}}`,
		`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`,
	)
	s, err := Parse(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 1 {
		t.Errorf("messages = %d, want 1", len(s.Messages))
	}
	if s.SkippedLines != 0 {
		t.Errorf("unknown types should be ignored, not counted: got %d", s.SkippedLines)
	}
}

func TestParseRejectsMissingHeader(t *testing.T) {
	p := writeSession(t, `{"type":"message","message":{"role":"user","content":[]}}`)
	if _, err := Parse(p); err == nil {
		t.Error("expected error for file with no session header")
	}
}

func TestParseHeaderOnly(t *testing.T) {
	s, err := Parse(writeSession(t, header))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 0 || len(s.Prose()) != 0 {
		t.Errorf("expected empty session, got %d messages", len(s.Messages))
	}
	if s.Preview(10) != "" {
		t.Errorf("Preview on empty session = %q", s.Preview(10))
	}
}

func TestProject(t *testing.T) {
	for _, tc := range []struct{ cwd, want string }{
		// The case that motivates two segments: worktree layouts.
		{"/Users/me/code/git-ls/main", "git-ls/main"},
		{"/Users/me/code/pi-session-tui/main", "pi-session-tui/main"},
		{"/Users/me/jellyfish/develop", "jellyfish/develop"},
		{"/Users/me/code/diffnav", "code/diffnav"},
		{"/trailing/slash/", "trailing/slash"},
		{"/single", "single"},
	} {
		s := &Session{CWD: tc.cwd, Path: "/tmp/x/y.jsonl"}
		if got := s.Project(); got != tc.want {
			t.Errorf("Project(%q) = %q, want %q", tc.cwd, got, tc.want)
		}
	}
}

func TestProjectFallback(t *testing.T) {
	p := writeSession(t,
		`{"type":"session","version":3,"id":"x","timestamp":"2026-01-01T00:00:00Z","cwd":""}`,
	)
	s, err := Parse(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.Project() == "" || s.Project() == "unknown" {
		t.Errorf("expected directory-derived fallback, got %q", s.Project())
	}
}

func TestTruncate(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello world", 8, "hello w…"},
		{"héllo wörld", 8, "héllo w…"}, // rune-aware, not byte-aware
	} {
		if got := truncate(tc.in, tc.n); got != tc.want {
			t.Errorf("truncate(%q,%d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

func TestDiscover(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "--Users-me-code-proj--")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a.jsonl", "b.jsonl", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(sub, n), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("Discover found %d files, want 2 (.jsonl only)", len(got))
	}
}

// Regression: Chunk sliced by byte offset, cutting multi-byte runes in half.
// The resulting invalid UTF-8 crashes sqlite-lembed with an uncaught C++
// exception, killing an entire index build. Found while indexing real sessions.
func TestChunkNeverSplitsRunes(t *testing.T) {
	inputs := []string{
		strings.Repeat("日", 400),    // 3-byte runes
		strings.Repeat("🎉", 300),    // 4-byte runes
		strings.Repeat("─┼│", 200),  // box drawing
		strings.Repeat("é", 500),    // 2-byte runes
		strings.Repeat("a日🎉─", 150), // mixed widths
	}
	// Limits deliberately not multiples of the rune widths, so a naive byte
	// slice would land mid-rune.
	for _, limit := range []int{16, 17, 31, 100, 101, 480, 481, 482, 483, 512} {
		for _, in := range inputs {
			for i, c := range Chunk(in, limit) {
				if !utf8.ValidString(c) {
					t.Fatalf("limit=%d chunk %d is invalid UTF-8 (% x)",
						limit, i, []byte(c)[max(0, len(c)-4):])
				}
				if len(c) > limit {
					t.Fatalf("limit=%d chunk %d is %d bytes", limit, i, len(c))
				}
			}
		}
	}
}

// A single rune wider than the limit must be emitted whole rather than
// corrupted, and must not loop forever.
func TestChunkRuneWiderThanLimit(t *testing.T) {
	got := Chunk(strings.Repeat("🎉", 5), 2) // 4-byte runes, 2-byte limit
	if len(got) == 0 {
		t.Fatal("no chunks produced")
	}
	for _, c := range got {
		if !utf8.ValidString(c) {
			t.Errorf("invalid UTF-8: % x", []byte(c))
		}
	}
	if joined := strings.Join(got, ""); joined != strings.Repeat("🎉", 5) {
		t.Errorf("content lost: %q", joined)
	}
}

type fakeTokenCounter struct{ perChar float64 }

func (f fakeTokenCounter) CountTokens(s string) (int, error) {
	return int(float64(len([]rune(s))) * f.perChar), nil
}

func TestSplitToTokenLimit(t *testing.T) {
	// Prose-like: ~0.2 tokens per char, nothing needs re-splitting.
	sparse := fakeTokenCounter{perChar: 0.2}
	in := []string{strings.Repeat("word ", 160)}
	got, err := SplitToTokenLimit(sparse, in, MaxTokens)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("sparse text was split into %d pieces, want 1", len(got))
	}

	// Dense: 1 token per char, so an 800-char chunk is 800 tokens and must split.
	dense := fakeTokenCounter{perChar: 1.0}
	got, err = SplitToTokenLimit(dense, in, MaxTokens)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 2 {
		t.Errorf("dense text was not split: %d piece(s)", len(got))
	}
	for i, p := range got {
		n, _ := dense.CountTokens(p)
		if n > MaxTokens {
			t.Errorf("piece %d still has %d tokens (limit %d)", i, n, MaxTokens)
		}
	}
}

func TestSplitToTokenLimitNilCounter(t *testing.T) {
	in := []string{"a", "b"}
	got, err := SplitToTokenLimit(nil, in, MaxTokens)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("nil counter should pass input through, got %d", len(got))
	}
}
