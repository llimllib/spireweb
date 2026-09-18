package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/llimllib/spireweb/internal/index"
)

// serve used to refuse to start without an index, so the check that matters is
// not that a file appears but that the read pool can open what was made: it is
// _query_only, and a connection that cannot create a schema is the whole reason
// the guard existed.
func TestBootstrapIndexMakesOneAReaderCanOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "fresh.db")

	if err := bootstrapIndex(path); err != nil {
		t.Fatal(err)
	}

	db, err := index.OpenReader(path, index.DriverName)
	if err != nil {
		t.Fatalf("OpenReader after bootstrap: %v", err)
	}
	defer db.Close()

	if n, err := db.CountSessions(t.Context()); err != nil || n != 0 {
		t.Errorf("CountSessions() = %d, %v; want 0 and no error", n, err)
	}
}

// An existing index must be left alone: bootstrapping is for the empty case,
// and anything it did to a real one would be done to somebody's corpus.
func TestBootstrapIndexLeavesAnExistingIndexAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.db")

	db, err := index.Open(path, index.DriverName)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetMeta("bootstrap-test", "kept"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := bootstrapIndex(path); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("bootstrapIndex wrote to an index that already existed")
	}

	db, err = index.OpenReader(path, index.DriverName)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if v, err := db.Meta("bootstrap-test"); err != nil || v != "kept" {
		t.Errorf("Meta() = %q, %v; want the value written before", v, err)
	}
}
