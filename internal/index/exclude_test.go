package index

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// writeClaudeSession writes a one-message Claude Code session with the given
// entrypoint, which is what decides whether it is indexed.
func writeClaudeSession(t *testing.T, dir, id, entrypoint, text string) string {
	t.Helper()
	sub := filepath.Join(dir, "-Users-me-code-proj")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"type":"user","sessionId":%q,"timestamp":"2026-09-11T13:35:05Z",`+
		`"cwd":"/Users/me/code/proj","entrypoint":%q,`+
		`"message":{"role":"user","content":[{"type":"text","text":%q}]}}`, id, entrypoint, text)
	p := filepath.Join(sub, id+".jsonl")
	if err := os.WriteFile(p, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func indexedIDs(t *testing.T, db *DB) map[string]bool {
	t.Helper()
	rows, err := db.SQL().Query(`SELECT id FROM sessions`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out[id] = true
	}
	return out
}

// Only the interactive session is indexed. The other two are a bridge
// duplicate of a pi session and one of spireweb's own title prompts.
func TestBuildExcludesSDKSessions(t *testing.T) {
	dir := t.TempDir()
	writeClaudeSession(t, dir, "interactive", "cli", "how do I pin a dependency")
	writeClaudeSession(t, dir, "bridged", "sdk-ts", "how do I pin a dependency")
	writeClaudeSession(t, dir, "titler", "sdk-cli", "You write short titles")

	db := openTest(t)
	p, err := Build(context.Background(), db, BuildOptions{Dirs: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}

	if p.Indexed != 1 || p.Excluded != 2 {
		t.Errorf("indexed %d, excluded %d; want 1 and 2", p.Indexed, p.Excluded)
	}
	got := indexedIDs(t, db)
	if !got["interactive"] {
		t.Error("the interactive session was not indexed")
	}
	for _, id := range []string{"bridged", "titler"} {
		if got[id] {
			t.Errorf("%s was indexed", id)
		}
	}
}

// A session indexed before the rule existed has to go. It is unchanged on
// disk, so the mtime test would otherwise keep it forever -- which is why the
// exclusion check deliberately does not mark the file as seen, leaving the
// sweep to remove it on a full pass.
func TestBuildDropsAlreadyIndexedSDKSessions(t *testing.T) {
	dir := t.TempDir()
	p := writeClaudeSession(t, dir, "bridged", "cli", "indexed before the rule")

	db := openTest(t)
	ctx := context.Background()
	if _, err := Build(ctx, db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}
	if !indexedIDs(t, db)["bridged"] {
		t.Fatal("setup: session was not indexed")
	}

	// Rewrite it as what it always was: a bridge session.
	line := `{"type":"user","sessionId":"bridged","timestamp":"2026-09-11T13:35:05Z",` +
		`"cwd":"/Users/me/code/proj","entrypoint":"sdk-ts",` +
		`"message":{"role":"user","content":[{"type":"text","text":"indexed before the rule"}]}}`
	if err := os.WriteFile(p, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pr, err := Build(ctx, db, BuildOptions{Dirs: []string{dir}, Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if pr.Excluded != 1 {
		t.Errorf("Excluded = %d, want 1", pr.Excluded)
	}
	if indexedIDs(t, db)["bridged"] {
		t.Error("an excluded session stayed in the index")
	}
}

// pi sessions carry no entrypoint, so the rule must not touch them.
func TestBuildKeepsPiSessions(t *testing.T) {
	dir := writeCorpus(t, map[string][]string{"s1": {userMsg("hello from pi")}})

	db := openTest(t)
	p, err := Build(context.Background(), db, BuildOptions{Dirs: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Indexed != 1 || p.Excluded != 0 {
		t.Errorf("indexed %d, excluded %d; want 1 and 0", p.Indexed, p.Excluded)
	}
}
