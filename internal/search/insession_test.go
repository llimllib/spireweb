package search

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/llimllib/spireweb/internal/index"
)

// indexFixture builds a real index over sessions given as message text, one
// user message per string. In-session matching is entirely a question of what
// SQL does with FTS5, so there is nothing to learn from a fake here.
func indexFixture(t *testing.T, sessions map[string][]string) *sql.DB {
	t.Helper()

	dir := t.TempDir()
	sub := filepath.Join(dir, "--Users-me-code-proj--")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for id, msgs := range sessions {
		lines := []string{`{"type":"session","version":3,"id":"` + id +
			`","timestamp":"2026-03-31T12:26:01.076Z","cwd":"/Users/me/code/proj"}`}
		for _, m := range msgs {
			lines = append(lines,
				`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"`+m+`"}]}}`)
		}
		if err := os.WriteFile(filepath.Join(sub, id+".jsonl"),
			[]byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	db, err := index.Open(filepath.Join(t.TempDir(), "i.db"), index.DriverName)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := index.Build(context.Background(), db, index.BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}
	return db.SQL()
}

// Where in a session the words are, as opposed to which session to open.
func TestMessagesMatchingRanksAndDeduplicates(t *testing.T) {
	db := indexFixture(t, map[string][]string{
		"s1": {
			"a passing mention of deploy",
			"nothing relevant here",
			"deploy deploy deploy the deploy broke during deploy",
		},
		"s2": {"deploy in another session entirely"},
	})

	got, err := MessagesMatching(context.Background(), db, "s1", "deploy", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("matches = %v, want two messages", got)
	}
	// Best first: the message that is mostly the term, not the aside.
	if got[0] != 2 {
		t.Errorf("best match = %d, want message 2", got[0])
	}
	// Scoped to the session asked for.
	for _, idx := range got {
		if idx == 1 {
			t.Error("matched a message with none of the terms")
		}
	}
}

func TestMessagesMatchingIsEmptyWithoutTerms(t *testing.T) {
	db := indexFixture(t, map[string][]string{"s1": {"the database deadlocked"}})

	for _, q := range []string{"", "   ", "xylophone", "a"} {
		got, err := MessagesMatching(context.Background(), db, "s1", q, 0)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		if len(got) != 0 {
			t.Errorf("%q matched %v, want nothing", q, got)
		}
	}

	// An unknown session is empty rather than an error: the index is a cache
	// and a session can vanish from it between two requests.
	got, err := MessagesMatching(context.Background(), db, "nope", "deadlocked", 0)
	if err != nil || len(got) != 0 {
		t.Errorf("unknown session: %v, %v", got, err)
	}
}
