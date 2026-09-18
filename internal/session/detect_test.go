package session

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// home redirects the probed locations at a temporary directory, so the machine
// running the test cannot decide the outcome.
func home(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	return h
}

// install creates dir and puts a session file in it.
func installSession(t *testing.T, dir string) {
	t.Helper()
	sub := filepath.Join(dir, "-Users-me-code-proj")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "s.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDetectFindsBothAgents(t *testing.T) {
	h := home(t)
	claude := filepath.Join(h, ".claude", "projects")
	pi := filepath.Join(h, ".pi", "agent", "sessions")
	installSession(t, claude)
	installSession(t, pi)

	got := Detect()
	for _, want := range []string{claude, pi} {
		if !slices.Contains(got, want) {
			t.Errorf("Detect() = %v, missing %s", got, want)
		}
	}
}

// ~/.claude survives as a home for settings and plugins after
// CLAUDE_CONFIG_DIR has moved the sessions elsewhere. Listing it would put a
// path in the interface that indexes nothing and explains nothing.
func TestDetectIgnoresADirectoryWithNoSessions(t *testing.T) {
	h := home(t)
	empty := filepath.Join(h, ".claude", "projects")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}

	if got := Detect(); len(got) != 0 {
		t.Errorf("Detect() = %v, want nothing: the directory holds no sessions", got)
	}
}

// CLAUDE_CONFIG_DIR moves the whole config directory, and is probed first.
func TestDetectHonoursClaudeConfigDir(t *testing.T) {
	h := home(t)
	moved := filepath.Join(h, "elsewhere")
	installSession(t, filepath.Join(moved, "projects"))
	t.Setenv("CLAUDE_CONFIG_DIR", moved)

	got := Detect()
	if len(got) == 0 || got[0] != filepath.Join(moved, "projects") {
		t.Errorf("Detect() = %v, want %s first", got, filepath.Join(moved, "projects"))
	}
}

// Nothing installed is a real state, and has to be reported rather than
// guessed at: the caller turns an empty result into an error naming every
// candidate.
func TestDetectFindsNothingOnACleanMachine(t *testing.T) {
	home(t)
	if got := Detect(); len(got) != 0 {
		t.Errorf("Detect() = %v, want nothing", got)
	}
	if len(Candidates()) == 0 {
		t.Error("Candidates() is empty; the error message would name nowhere")
	}
}

func TestHasSessionsLooksAtAnyDepth(t *testing.T) {
	h := home(t)
	deep := filepath.Join(h, "root", "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "s.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !HasSessions(filepath.Join(h, "root")) {
		t.Error("HasSessions() = false for a session nested several levels down")
	}
	if HasSessions(filepath.Join(h, "absent")) {
		t.Error("HasSessions() = true for a directory that does not exist")
	}
}

// The overlap that actually happens: CLAUDE_CONFIG_DIR is commonly set to
// ~/.config/claude, which is also probed on its own. Discover would not index
// those files twice, but listing the same path twice is its own kind of wrong.
func TestCandidatesDeduplicatesTheCommonOverlap(t *testing.T) {
	h := home(t)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(h, ".config", "claude"))
	installSession(t, filepath.Join(h, ".config", "claude", "projects"))

	for _, got := range [][]string{Candidates(), Detect()} {
		seen := map[string]bool{}
		for _, d := range got {
			if seen[d] {
				t.Errorf("%v lists %s twice", got, d)
			}
			seen[d] = true
		}
	}
	if got := Detect(); len(got) != 1 {
		t.Errorf("Detect() = %v, want one directory", got)
	}
}
