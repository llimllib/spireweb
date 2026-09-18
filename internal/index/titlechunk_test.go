package index

import (
	"context"
	"testing"
)

func chunkBodies(t *testing.T, db *DB, sessionID, role string) []string {
	t.Helper()
	rows, err := db.SQL().Query(
		`SELECT body FROM chunks WHERE session_id = ? AND role = ? ORDER BY id`,
		sessionID, role)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

// The title is the line the list shows in bold, and it was the one sentence in
// the database that search could not see.
func TestTitleIsIndexedAsAChunk(t *testing.T) {
	dir := writeCorpus(t, map[string][]string{
		"s1": {userMsg("the thing we talked about for ages")},
	})
	db := openTest(t)
	ctx := context.Background()
	if _, err := Build(ctx, db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}

	// No title yet, so no title chunk.
	if got := chunkBodies(t, db, "s1", RoleTitle); len(got) != 0 {
		t.Fatalf("title chunks before titling = %v", got)
	}

	// The pass runs after the build and writes one.
	if err := db.SetTitle(ctx, "s1", "Grafana dashboard for the v1 API", "key1", 1); err != nil {
		t.Fatal(err)
	}

	// The next build picks it up, even though the file has not changed: that
	// is what the backfill promotion is for.
	if _, err := Build(ctx, db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}
	got := chunkBodies(t, db, "s1", RoleTitle)
	if len(got) != 1 || got[0] != "Grafana dashboard for the v1 API" {
		t.Fatalf("title chunks = %v", got)
	}

	// And it is searchable, which the whole thing is for.
	var n int
	if err := db.SQL().QueryRow(`
		SELECT COUNT(*) FROM chunks_fts f JOIN chunks c ON c.id = f.rowid
		WHERE chunks_fts MATCH ? AND c.session_id = 's1'`, `"grafana"`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Error("the title is not in the FTS index")
	}
}

// A title chunk belongs to no message, so it must not be able to send the
// reading pane scrolling to one.
func TestTitleChunkClaimsNoMessage(t *testing.T) {
	dir := writeCorpus(t, map[string][]string{"s1": {userMsg("hello")}})
	db := openTest(t)
	ctx := context.Background()
	if _, err := Build(ctx, db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTitle(ctx, "s1", "A generated title", "key1", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(ctx, db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}

	var idx int
	if err := db.SQL().QueryRow(
		`SELECT msg_idx FROM chunks WHERE session_id='s1' AND role=?`, RoleTitle).Scan(&idx); err != nil {
		t.Fatal(err)
	}
	if idx >= 0 {
		t.Errorf("title chunk msg_idx = %d, want a negative one that matches no message", idx)
	}
}

// Reindexing is constant work per unchanged session; a title that has not
// changed must not churn its chunk, or every build would re-embed 1162 of them.
func TestTitleChunkIsReused(t *testing.T) {
	dir := writeCorpus(t, map[string][]string{"s1": {userMsg("hello")}})
	db := openTest(t)
	ctx := context.Background()
	if _, err := Build(ctx, db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTitle(ctx, "s1", "A generated title", "key1", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(ctx, db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}

	var before int64
	if err := db.SQL().QueryRow(
		`SELECT id FROM chunks WHERE session_id='s1' AND role=?`, RoleTitle).Scan(&before); err != nil {
		t.Fatal(err)
	}

	// A full pass, which is what the backfill promotion does.
	if _, err := Build(ctx, db, BuildOptions{Dirs: []string{dir}, Full: true}); err != nil {
		t.Fatal(err)
	}
	var after int64
	if err := db.SQL().QueryRow(
		`SELECT id FROM chunks WHERE session_id='s1' AND role=?`, RoleTitle).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Errorf("title chunk id %d -> %d: it was rewritten rather than reused", before, after)
	}
}

// A retitled session must lose the old one, or search keeps matching a title
// nothing displays.
func TestRetitlingReplacesTheChunk(t *testing.T) {
	dir := writeCorpus(t, map[string][]string{"s1": {userMsg("hello")}})
	db := openTest(t)
	ctx := context.Background()
	if _, err := Build(ctx, db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTitle(ctx, "s1", "The first title", "key1", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(ctx, db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTitle(ctx, "s1", "A better second title", "key2", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(ctx, db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}

	got := chunkBodies(t, db, "s1", RoleTitle)
	if len(got) != 1 || got[0] != "A better second title" {
		t.Fatalf("title chunks = %v, want only the current one", got)
	}
	// The old title must be out of the FTS index too, which external-content
	// FTS5 does not do by itself.
	var n int
	if err := db.SQL().QueryRow(
		`SELECT COUNT(*) FROM chunks_fts WHERE chunks_fts MATCH ?`, `"first"`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("the previous title still matches in FTS (%d rows)", n)
	}
}

// The backfill settles: once every title has a chunk, a build is incremental
// again rather than promoting itself to a full pass forever.
func TestTitleChunkBackfillSettles(t *testing.T) {
	dir := writeCorpus(t, map[string][]string{
		"s1": {userMsg("one")},
		"s2": {userMsg("two")},
	})
	db := openTest(t)
	ctx := context.Background()
	if _, err := Build(ctx, db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTitle(ctx, "s1", "A title", "key1", 1); err != nil {
		t.Fatal(err)
	}

	p, err := Build(ctx, db, BuildOptions{Dirs: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Indexed != 2 {
		t.Errorf("indexed = %d, want a full pass to pick the title up", p.Indexed)
	}

	p, err = Build(ctx, db, BuildOptions{Dirs: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Indexed != 0 || p.Skipped != 2 {
		t.Errorf("progress = %+v, want an incremental run once titles are indexed", p)
	}
}
