package index

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const hdr = `{"type":"session","version":3,"id":"%s","timestamp":"2026-03-31T12:26:01.076Z","cwd":"/Users/me/code/%s"}`

// writeCorpus creates a session directory with one file per spec entry.
func writeCorpus(t *testing.T, specs map[string][]string) string {
	t.Helper()
	dir := t.TempDir()
	sub := filepath.Join(dir, "--Users-me-code-proj--")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for id, msgs := range specs {
		lines := []string{strings.Replace(strings.Replace(hdr, "%s", id, 1), "%s", "proj", 1)}
		lines = append(lines, msgs...)
		p := filepath.Join(sub, id+".jsonl")
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func userMsg(text string) string {
	return `{"type":"message","message":{"role":"user","content":[{"type":"text","text":"` + text + `"}]}}`
}

func openTest(t *testing.T) *DB {
	t.Helper()
	if err := CheckFTS5(); err != nil {
		t.Fatalf("build lacks FTS5 (use -tags sqlite_fts5): %v", err)
	}
	db, err := Open(filepath.Join(t.TempDir(), "i.db"), DriverName)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestBuildAndStats(t *testing.T) {
	dir := writeCorpus(t, map[string][]string{
		"s1": {userMsg("how do I center a div"), userMsg("use flexbox")},
		"s2": {userMsg("the database deadlocked during deploy")},
	})
	db := openTest(t)

	p, err := Build(context.Background(), db, BuildOptions{Dirs: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Indexed != 2 || p.Failed != 0 {
		t.Errorf("indexed=%d failed=%d, want 2/0", p.Indexed, p.Failed)
	}
	if !p.Finished {
		t.Error("progress not marked finished")
	}

	st, err := db.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if st.Sessions != 2 {
		t.Errorf("sessions = %d, want 2", st.Sessions)
	}
	if st.Chunks != 3 {
		t.Errorf("chunks = %d, want 3", st.Chunks)
	}
}

// The second run must do no work when nothing changed. This is what keeps
// reindexing cheap enough to run on every launch.
func TestBuildIncremental(t *testing.T) {
	dir := writeCorpus(t, map[string][]string{"s1": {userMsg("hello world")}})
	db := openTest(t)
	ctx := context.Background()

	if _, err := Build(ctx, db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}
	p, err := Build(ctx, db, BuildOptions{Dirs: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Skipped != 1 || p.Indexed != 0 {
		t.Errorf("second run: skipped=%d indexed=%d, want 1/0", p.Skipped, p.Indexed)
	}

	// --full ignores mtime and reindexes anyway.
	p, err = Build(ctx, db, BuildOptions{Dirs: []string{dir}, Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.Indexed != 1 || p.Skipped != 0 {
		t.Errorf("full run: indexed=%d skipped=%d, want 1/0", p.Indexed, p.Skipped)
	}
}

// Appending to a session must update it, not duplicate it.
func TestBuildDetectsChange(t *testing.T) {
	dir := writeCorpus(t, map[string][]string{"s1": {userMsg("first message")}})
	db := openTest(t)
	ctx := context.Background()

	if _, err := Build(ctx, db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}

	p := filepath.Join(dir, "--Users-me-code-proj--", "s1.jsonl")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(userMsg("second message") + "\n")
	f.Close()
	// Ensure mtime differs even on coarse-grained filesystems.
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(p, future, future)

	if _, err := Build(ctx, db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}
	st, _ := db.Stats()
	if st.Sessions != 1 {
		t.Errorf("sessions = %d, want 1 (updated, not duplicated)", st.Sessions)
	}
	if st.Chunks != 2 {
		t.Errorf("chunks = %d, want 2", st.Chunks)
	}
}

// A deleted session file must leave no trace, including in the FTS index.
func TestBuildRemovesDeleted(t *testing.T) {
	dir := writeCorpus(t, map[string][]string{
		"s1": {userMsg("keep this one")},
		"s2": {userMsg("delete this unique_marker_xyz")},
	})
	db := openTest(t)
	ctx := context.Background()

	if _, err := Build(ctx, db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "--Users-me-code-proj--", "s2.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(ctx, db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}

	st, _ := db.Stats()
	if st.Sessions != 1 || st.Chunks != 1 {
		t.Errorf("sessions=%d chunks=%d, want 1/1", st.Sessions, st.Chunks)
	}

	// External-content FTS5 needs explicit deletion; a stale row here would
	// produce search hits pointing at chunks that no longer exist.
	var n int
	err := db.SQL().QueryRow(
		`SELECT COUNT(*) FROM chunks_fts WHERE chunks_fts MATCH '"unique_marker_xyz"'`).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("FTS index still has %d row(s) for the deleted session", n)
	}
}

// Reindexing must not leave duplicate FTS rows behind.
func TestFTSStaysConsistent(t *testing.T) {
	dir := writeCorpus(t, map[string][]string{"s1": {userMsg("consistency marker_abc")}})
	db := openTest(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := Build(ctx, db, BuildOptions{Dirs: []string{dir}, Full: true}); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := db.SQL().QueryRow(
		`SELECT COUNT(*) FROM chunks_fts WHERE chunks_fts MATCH '"marker_abc"'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("after 3 full rebuilds, FTS has %d rows for one chunk, want 1", n)
	}
}

func TestMeta(t *testing.T) {
	db := openTest(t)
	if v, err := db.Meta("nope"); err != nil || v != "" {
		t.Errorf("missing key: got %q, %v", v, err)
	}
	if err := db.SetMeta("k", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetMeta("k", "v2"); err != nil {
		t.Fatal(err)
	}
	if v, _ := db.Meta("k"); v != "v2" {
		t.Errorf("meta = %q, want v2 (upsert)", v)
	}
	if v, _ := db.Meta(MetaSchemaVersion); v != strconv.Itoa(SchemaVersion) {
		t.Errorf("schema_version = %q, want %d", v, SchemaVersion)
	}
}

// Mixing vectors from two models is silently wrong, so it must be refused.
func TestEmbedderMismatchRefused(t *testing.T) {
	db := openTest(t)
	if err := db.SetMeta(MetaEmbedModel, "model-a"); err != nil {
		t.Fatal(err)
	}
	if err := db.checkEmbedderName("model-b", false); err == nil {
		t.Error("expected refusal when switching models without --full")
	}
	if err := db.checkEmbedderName("model-b", true); err != nil {
		t.Errorf("--full should permit a model change: %v", err)
	}
	if err := db.checkEmbedderName("model-a", false); err != nil {
		t.Errorf("same model should be permitted: %v", err)
	}
}

// An index with no recorded model accepts any model.
func TestEmbedderFirstUse(t *testing.T) {
	db := openTest(t)
	if err := db.checkEmbedderName("anything", false); err != nil {
		t.Errorf("fresh index should accept any model: %v", err)
	}
}

func TestSchemaVersionMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "i.db")
	db, err := Open(path, DriverName)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetMeta(MetaSchemaVersion, "999"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	if _, err := Open(path, DriverName); err == nil {
		t.Error("expected Open to refuse an index from a different schema version")
	}
}

// Appending to a session must reuse chunks that did not change.
//
// Regression for two linked bugs. chunks.session_id declares ON DELETE CASCADE,
// so the original code's DELETE + INSERT of the session row destroyed the very
// chunks it had just marked for reuse. Chunk ids then climbed on every reindex,
// and because chunks_vec is a virtual table that does not participate in
// cascades, its rows were left orphaned -- 240 orphans after five rounds.
//
// The visible symptom was cost: reindexing a growing session scaled with total
// size (405ms -> 2065ms) instead of with the appended text.
func TestBuildReusesUnchangedChunks(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "--Users-me-code-proj--")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(sub, "s.jsonl")
	write := func(n int) {
		lines := []string{strings.Replace(strings.Replace(hdr, "%s", "grow", 1), "%s", "proj", 1)}
		for i := 0; i < n; i++ {
			lines = append(lines, userMsg(fmt.Sprintf("message number %d with distinct text", i)))
		}
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	db := openTest(t)
	ctx := context.Background()

	write(5)
	if _, err := Build(ctx, db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}
	ids1 := chunkIDs(t, db, "grow")
	if len(ids1) != 5 {
		t.Fatalf("expected 5 chunks, got %d", len(ids1))
	}

	// Append five messages; the first five chunks must keep their ids.
	write(10)
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(p, future, future)
	if _, err := Build(ctx, db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}
	ids2 := chunkIDs(t, db, "grow")
	if len(ids2) != 10 {
		t.Fatalf("expected 10 chunks, got %d", len(ids2))
	}
	for _, id := range ids1 {
		if !contains(ids2, id) {
			t.Errorf("chunk id %d was recreated instead of reused; ids now %v", id, ids2)
		}
	}
}

// Editing a session must drop the chunks that no longer exist.
func TestBuildDropsRemovedChunks(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "--Users-me-code-proj--")
	os.MkdirAll(sub, 0o755)
	p := filepath.Join(sub, "s.jsonl")

	writeMsgs := func(texts ...string) {
		lines := []string{strings.Replace(strings.Replace(hdr, "%s", "edit", 1), "%s", "proj", 1)}
		for _, t := range texts {
			lines = append(lines, userMsg(t))
		}
		os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	}

	db := openTest(t)
	ctx := context.Background()

	writeMsgs("keep this text", "remove_me_marker text")
	if _, err := Build(ctx, db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}

	writeMsgs("keep this text")
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(p, future, future)
	if _, err := Build(ctx, db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}

	st, _ := db.Stats()
	if st.Chunks != 1 {
		t.Errorf("chunks = %d, want 1", st.Chunks)
	}
	var n int
	if err := db.SQL().QueryRow(
		`SELECT COUNT(*) FROM chunks_fts WHERE chunks_fts MATCH '"remove_me_marker"'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("FTS still matches removed chunk (%d rows)", n)
	}
}

// A session id whose file moved must not leave two rows behind.
func TestBuildHandlesRenamedFile(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "--Users-me-code-proj--")
	os.MkdirAll(sub, 0o755)
	body := strings.Replace(strings.Replace(hdr, "%s", "moved", 1), "%s", "proj", 1) +
		"\n" + userMsg("hello there") + "\n"

	db := openTest(t)
	ctx := context.Background()

	os.WriteFile(filepath.Join(sub, "a.jsonl"), []byte(body), 0o644)
	if _, err := Build(ctx, db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(sub, "a.jsonl"))
	os.WriteFile(filepath.Join(sub, "b.jsonl"), []byte(body), 0o644)
	if _, err := Build(ctx, db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}

	st, _ := db.Stats()
	if st.Sessions != 1 {
		t.Errorf("sessions = %d, want 1 after rename", st.Sessions)
	}
}

func chunkIDs(t *testing.T, db *DB, sessionID string) []int64 {
	t.Helper()
	rows, err := db.SQL().Query(`SELECT id FROM chunks WHERE session_id = ? ORDER BY id`, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		out = append(out, id)
	}
	return out
}

func contains(hay []int64, needle int64) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// A stale index must be rebuilt automatically rather than reported as a fatal
// error. The index is derived from the session files, so discarding it is always
// safe, and telling the user to run a command is a worse experience than just
// doing the work.
func TestOpenOrResetRebuildsStaleIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "i.db")

	db, err := Open(path, DriverName)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetMeta(MetaSchemaVersion, "999"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Open must refuse.
	if _, err := Open(path, DriverName); err == nil {
		t.Fatal("Open accepted a stale index")
	} else {
		var stale *StaleIndexError
		if !errors.As(err, &stale) {
			t.Fatalf("expected StaleIndexError, got %T: %v", err, err)
		}
	}

	// OpenOrReset must recover, reporting that it did.
	db2, reset, err := OpenOrReset(path, DriverName)
	if err != nil {
		t.Fatalf("OpenOrReset failed: %v", err)
	}
	defer db2.Close()
	if !reset {
		t.Error("reset flag was false for a stale index")
	}
	if v, _ := db2.Meta(MetaSchemaVersion); v != strconv.Itoa(SchemaVersion) {
		t.Errorf("schema_version = %q after reset, want %d", v, SchemaVersion)
	}
	st, err := db2.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if st.Sessions != 0 || st.Chunks != 0 {
		t.Errorf("reset index is not empty: %+v", st)
	}
}

// A healthy index must be left alone.
func TestOpenOrResetKeepsGoodIndex(t *testing.T) {
	dir := writeCorpus(t, map[string][]string{"s1": {userMsg("keep me")}})
	path := filepath.Join(t.TempDir(), "i.db")

	db, _, err := OpenOrReset(path, DriverName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build(context.Background(), db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db2, reset, err := OpenOrReset(path, DriverName)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if reset {
		t.Error("a current index was needlessly reset")
	}
	st, _ := db2.Stats()
	if st.Sessions != 1 {
		t.Errorf("sessions = %d after reopen, want 1", st.Sessions)
	}
}

// Searching while a write transaction is open is what makes indexing
// non-blocking, so assert WAL actually allows it through a second handle.
func TestReaderQueriesDuringWrite(t *testing.T) {
	dir := writeCorpus(t, map[string][]string{"s1": {userMsg("findable marker_ready")}})
	path := filepath.Join(t.TempDir(), "i.db")

	db, _, err := OpenOrReset(path, DriverName)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := Build(context.Background(), db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}

	reader, err := OpenReader(path, DriverName)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	// Hold a write transaction open on the writer handle.
	tx, err := db.SQL().Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO chunks(session_id, msg_idx, role, body)
		VALUES('s1', 99, 'user', 'written inside the open transaction')`); err != nil {
		t.Fatal(err)
	}

	// The reader must still answer, seeing the pre-transaction snapshot.
	var n int
	if err := reader.SQL().QueryRow(
		`SELECT COUNT(*) FROM chunks_fts WHERE chunks_fts MATCH '"marker_ready"'`).Scan(&n); err != nil {
		t.Fatalf("reader blocked or failed during an open write tx: %v", err)
	}
	if n != 1 {
		t.Errorf("reader saw %d matches, want 1", n)
	}

	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
