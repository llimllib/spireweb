package session

import (
	"os"
	"path/filepath"
	"strings"
)

// Candidates lists where agent sessions are kept, in the order they are
// probed.
//
// Both Claude Code paths matter and neither is redundant. CLAUDE_CONFIG_DIR
// moves the whole config directory; ~/.config/claude is where it lands for
// someone who has set XDG_CONFIG_HOME or migrated; ~/.claude is the default a
// fresh install uses. A machine can have more than one, and an old one holding
// real history is worth indexing rather than guessing about.
// Deduplicated, because the candidates genuinely overlap in the common case:
// CLAUDE_CONFIG_DIR is usually set to ~/.config/claude, which is also probed
// on its own. Discover would not index those files twice, but listing the same
// path twice in the interface is its own kind of wrong.
func Candidates() []string {
	var raw []string
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		raw = append(raw, filepath.Join(d, "projects"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		raw = append(raw,
			filepath.Join(home, ".config", "claude", "projects"),
			filepath.Join(home, ".claude", "projects"),
			filepath.Join(home, ".pi", "agent", "sessions"),
		)
	}

	out := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, d := range raw {
		d = filepath.Clean(d)
		if seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}

// Detect returns the candidate directories that actually hold sessions.
//
// Existence is not the test. ~/.claude survives as a home for settings and
// plugins after CLAUDE_CONFIG_DIR has moved everything else, and listing a
// directory with no sessions in it would put a path in the interface that
// explains nothing and indexes nothing. Holding at least one .jsonl is the
// question worth asking, and it is cheap because the walk stops at the first
// one found.
func Detect() []string {
	var out []string
	for _, dir := range Candidates() {
		if HasSessions(dir) {
			out = append(out, dir)
		}
	}
	return out
}

// HasSessions reports whether dir contains at least one .jsonl, at any depth.
func HasSessions(dir string) bool {
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return false
	}
	found := false
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable subtree: keep looking
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".jsonl") {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}
