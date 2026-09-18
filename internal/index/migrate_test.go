package index

import (
	"context"
	"path/filepath"
	"testing"
)

// An index built before the title pass existed must gain its columns without
// being rebuilt. The alternative -- bumping SchemaVersion -- discards the
// database, and re-embedding 46k chunks to add two nullable columns is not a
// trade worth making.
func TestMigrateAddsTitleColumnsToAnExistingIndex(t *testing.T) {
	dir := writeCorpus(t, map[string][]string{"s1": {userMsg("how do I center a div")}})
	path := filepath.Join(t.TempDir(), "i.db")

	db, err := Open(path, DriverName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build(context.Background(), db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}
	// Roll the schema back to what it was before this milestone.
	for _, c := range addedColumns {
		if _, err := db.SQL().Exec(
			`ALTER TABLE ` + c.table + ` DROP COLUMN ` + c.column); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(path, DriverName)
	if err != nil {
		t.Fatalf("reopening an index without the title columns: %v", err)
	}
	defer db.Close()

	for _, c := range addedColumns {
		has, err := db.hasColumn(c.table, c.column)
		if err != nil {
			t.Fatal(err)
		}
		if !has {
			t.Errorf("%s.%s was not added", c.table, c.column)
		}
	}

	// The rows -- and therefore the chunks and vectors derived from them --
	// are still there. That is the whole point.
	s, err := db.Session(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if s.Preview != "how do I center a div" {
		t.Errorf("preview = %q, want the index left intact", s.Preview)
	}
}

func TestTitleCandidatesAndWrites(t *testing.T) {
	dir := writeCorpus(t, map[string][]string{
		"s1": {userMsg("alpha")},
		"s2": {userMsg("beta")},
	})
	db := openTest(t)
	ctx := context.Background()
	if _, err := Build(ctx, db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}

	cands, err := db.TitleCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 {
		t.Fatalf("candidates = %d, want both untitled sessions", len(cands))
	}
	if cands[0].Path == "" || cands[0].NumMsgs == 0 {
		t.Errorf("candidate = %+v, want the path and message count needed to summarize it", cands[0])
	}

	if err := db.SetTitle(ctx, "s1", "Centering a div", "abc123", 1); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkTitleChecked(ctx, "s2", 1); err != nil {
		t.Fatal(err)
	}

	// Both are settled now: one titled, one examined and found to have nothing
	// worth summarizing. Neither should be parsed again.
	cands, err = db.TitleCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Errorf("candidates = %d, want none", len(cands))
	}
	if n, err := db.CountTitledSessions(ctx); err != nil || n != 1 {
		t.Errorf("titled = %d (err %v), want 1", n, err)
	}

	s, err := db.Session(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if s.Heading() != "Centering a div" {
		t.Errorf("heading = %q, want the generated title", s.Heading())
	}
	// A session with no title still shows something, which is why a failed
	// summary is never a blank row.
	s2, err := db.Session(ctx, "s2")
	if err != nil {
		t.Fatal(err)
	}
	if s2.Heading() != "beta" {
		t.Errorf("heading = %q, want the opening message as a fallback", s2.Heading())
	}
}
