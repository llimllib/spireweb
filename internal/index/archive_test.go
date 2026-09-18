package index

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func archivedCount(t *testing.T, db *DB, sessionID string) int {
	t.Helper()
	var n int
	if err := db.SQL().QueryRow(
		`SELECT COUNT(*) FROM messages WHERE session_id = ?`, sessionID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestArchiveStoresEveryMessageVerbatim(t *testing.T) {
	dir := writeCorpus(t, map[string][]string{
		"s1": {userMsg("how do I center a div"), assistantMsg("use flexbox")},
	})
	db := openTest(t)
	p, err := Build(context.Background(), db, BuildOptions{Dirs: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Messages != 2 {
		t.Errorf("Progress.Messages = %d, want 2", p.Messages)
	}

	rows, err := db.SQL().Query(
		`SELECT idx, role, content FROM messages WHERE session_id = 's1' ORDER BY idx`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var idx int
		var role, content string
		if err := rows.Scan(&idx, &role, &content); err != nil {
			t.Fatal(err)
		}
		got = append(got, role)
		// Stored as the object pi wrote, not as a re-marshalling of our struct.
		var m map[string]any
		if err := json.Unmarshal([]byte(content), &m); err != nil {
			t.Errorf("message %d is not valid JSON: %v", idx, err)
		}
		if m["role"] != role {
			t.Errorf("message %d: column role %q, content role %v", idx, role, m["role"])
		}
	}
	if len(got) != 2 || got[0] != "user" || got[1] != "assistant" {
		t.Errorf("roles = %v, want [user assistant]", got)
	}
}

// An archive that stores only the roles this code understands is not an
// archive. pi already writes at least one role nothing here handles.
func TestArchiveKeepsUnknownRolesAndFields(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "--Users-me-code-proj--")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"session","version":3,"id":"s1","timestamp":"2026-03-31T12:26:01.076Z","cwd":"/Users/me/code/proj"}
{"type":"message","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}
{"type":"message","message":{"role":"bashExecution","command":"ls -la","exitCode":0,"futureField":{"nested":true}}}
`
	if err := os.WriteFile(filepath.Join(sub, "s1.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	db := openTest(t)
	if _, err := Build(context.Background(), db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}

	var content string
	if err := db.SQL().QueryRow(
		`SELECT content FROM messages WHERE session_id = 's1' AND idx = 1`).Scan(&content); err != nil {
		t.Fatalf("the unknown role was not archived: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(content), &m); err != nil {
		t.Fatal(err)
	}
	// Fields no Go struct in this repo has a field for.
	if m["command"] != "ls -la" {
		t.Errorf("command = %v, want it preserved", m["command"])
	}
	if m["futureField"] == nil {
		t.Error("futureField dropped; the archive is not verbatim")
	}
}

// Sessions are append-only and get reindexed on every new message, so a
// session that grew by one must archive one row rather than rewriting all of
// them. On this corpus the difference is 265MB of writes per message.
func TestArchiveIsIncremental(t *testing.T) {
	dir := writeCorpus(t, map[string][]string{"s1": {userMsg("first")}})
	db := openTest(t)
	if _, err := Build(context.Background(), db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}

	p := filepath.Join(dir, "--Users-me-code-proj--", "s1.jsonl")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(userMsg("second") + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := os.Chtimes(p, time.Now(), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	pr, err := Build(context.Background(), db, BuildOptions{Dirs: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	if pr.Messages != 1 {
		t.Errorf("archived %d messages, want 1: only the new one", pr.Messages)
	}
	if n := archivedCount(t, db, "s1"); n != 2 {
		t.Errorf("archived total = %d, want 2", n)
	}
}

// A file truncated mid-write must not leave rows claiming messages the
// session no longer has.
func TestArchiveDropsMessagesThatWentAway(t *testing.T) {
	dir := writeCorpus(t, map[string][]string{
		"s1": {userMsg("first"), userMsg("second"), userMsg("third")},
	})
	db := openTest(t)
	if _, err := Build(context.Background(), db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}
	if n := archivedCount(t, db, "s1"); n != 3 {
		t.Fatalf("archived = %d, want 3", n)
	}

	// Rewrite it shorter.
	p := filepath.Join(dir, "--Users-me-code-proj--", "s1.jsonl")
	short := `{"type":"session","version":3,"id":"s1","timestamp":"2026-03-31T12:26:01.076Z","cwd":"/Users/me/code/proj"}` +
		"\n" + userMsg("first") + "\n"
	if err := os.WriteFile(p, []byte(short), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, time.Now(), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(context.Background(), db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}
	if n := archivedCount(t, db, "s1"); n != 1 {
		t.Errorf("archived = %d, want 1 after the file shrank", n)
	}
}

// Deleting a session takes its messages with it: the foreign key cascades,
// unlike the FTS and vector tables.
func TestArchiveCascadesWithTheSession(t *testing.T) {
	dir := writeCorpus(t, map[string][]string{"s1": {userMsg("first")}})
	db := openTest(t)
	if _, err := Build(context.Background(), db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "--Users-me-code-proj--", "s1.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(context.Background(), db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}
	if n := archivedCount(t, db, "s1"); n != 0 {
		t.Errorf("archived = %d after the session was removed, want 0", n)
	}
}

// An index built before the archive existed would otherwise never gain one:
// the skip test compares mtime and size, and those do not change.
func TestArchiveBackfillsAnOlderIndex(t *testing.T) {
	dir := writeCorpus(t, map[string][]string{
		"s1": {userMsg("first")},
		"s2": {userMsg("second")},
	})
	db := openTest(t)
	if _, err := Build(context.Background(), db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}

	// Simulate an index from before the table existed.
	if _, err := db.SQL().Exec(`DELETE FROM messages`); err != nil {
		t.Fatal(err)
	}

	p, err := Build(context.Background(), db, BuildOptions{Dirs: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	if !p.ArchiveBackfill {
		t.Error("the run was not promoted to a full pass")
	}
	if p.Messages != 2 {
		t.Errorf("archived %d messages, want 2", p.Messages)
	}

	// And a run after that has nothing to do.
	p, err = Build(context.Background(), db, BuildOptions{Dirs: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	if p.ArchiveBackfill || p.Messages != 0 {
		t.Errorf("progress = %+v, want a quiet run", p)
	}
}
