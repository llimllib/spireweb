package index

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mattn/go-sqlite3"

	"github.com/llimllib/spireweb/internal/embed"
)

func TestReaderRefusesWrites(t *testing.T) {
	dir := writeCorpus(t, map[string][]string{"s1": {userMsg("hello")}})
	path := filepath.Join(t.TempDir(), "i.db")

	db, err := Open(path, DriverName)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := Build(context.Background(), db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}

	// query_only rather than mode=ro: a genuinely read-only file handle cannot
	// create the -shm and -wal files WAL needs, so the reader must be able to
	// write those while still refusing to write the database.
	r, err := OpenReader(path, DriverName)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	var n int
	if err := r.SQL().QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatalf("reader cannot read: %v", err)
	}
	if n != 1 {
		t.Fatalf("sessions = %d, want 1", n)
	}
	if _, err := r.SQL().Exec(`DELETE FROM sessions`); err == nil {
		t.Fatal("reader accepted a write")
	}
}

// A read-only pool must work when no writer has the database open, which is
// the case mode=ro fails: without an existing -shm file it cannot start a WAL
// read transaction.
func TestReaderOpensWithoutAWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "i.db")
	db, err := Open(path, DriverName)
	if err != nil {
		t.Fatal(err)
	}
	dir := writeCorpus(t, map[string][]string{"s1": {userMsg("hello")}})
	if _, err := Build(context.Background(), db, BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}
	if err := db.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	db.Close() // no writer holds the database open

	r, err := OpenReader(path, DriverName)
	if err != nil {
		t.Fatalf("open reader with no writer: %v", err)
	}
	defer r.Close()

	var n int
	if err := r.SQL().QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatalf("read with no writer: %v", err)
	}
}

// semanticPaths skips the test unless semantic search actually works here.
//
// Installed is not the same as working. llama.cpp needs a Metal device on
// macOS, and a headless CI runner or a sandbox without GPU access has none: the
// extension loads, lembed_version() answers, and model loading then fails,
// handing the vtab a null model. Pinging is what distinguishes the two, because
// the driver's ConnectHook registers the model on connect.
func semanticPaths(t *testing.T) embed.Paths {
	t.Helper()
	p := embed.DefaultPaths()
	if err := p.Check(); err != nil {
		t.Skipf("semantic search not installed: %v", err)
	}
	if err := RegisterSemanticDriver(p); err != nil {
		t.Skipf("semantic driver unavailable: %v", err)
	}
	probe, err := sql.Open(SemanticDriverName, filepath.Join(t.TempDir(), "probe.db"))
	if err == nil {
		err = probe.Ping()
		probe.Close()
	}
	if err != nil {
		t.Skipf("semantic backend not functional here (no GPU?): %v", err)
	}
	return p
}

// The failure this guards against is silent. sqlite-lembed keeps its model in
// temp.lembed_models, which is per-connection, so a connection database/sql
// opened after the pool was set up has no model. Semantic queries on it fail,
// the ranker gets skipped, and search degrades to lexical-only with no error
// anywhere.
//
// Forcing idle connections to zero makes every query take a fresh connection,
// which is the churn a long-running server sees spread over hours.
func TestReaderPoolSurvivesConnectionChurn(t *testing.T) {
	semanticPaths(t)

	path := filepath.Join(t.TempDir(), "i.db")
	db, err := Open(path, SemanticDriverName)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	r, err := OpenReader(path, SemanticDriverName)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	r.SQL().SetMaxIdleConns(0) // every query gets a connection it has never seen

	for i := range 10 {
		var blob []byte
		if err := r.SQL().QueryRow(`SELECT lembed('minilm', ?)`, "hello world").Scan(&blob); err != nil {
			t.Fatalf("query %d on a fresh connection: %v", i, err)
		}
		if len(blob) != embed.Dim*4 {
			t.Fatalf("query %d: got %d bytes, want %d", i, len(blob), embed.Dim*4)
		}
	}
}

// Demonstrates that the ConnectHook is what makes the test above pass, rather
// than something else registering the model. Without the hook, the extension
// loads and lembed() exists, but no model is registered and the call fails.
func TestWithoutConnectHookTheModelIsMissing(t *testing.T) {
	p := semanticPaths(t)

	const name = "sqlite3_semantic_nohook_test"
	if !slicesContains(sql.Drivers(), name) {
		sql.Register(name, &sqlite3.SQLiteDriver{Extensions: []string{p.Extension}})
	}

	db, err := sql.Open(name, filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var blob []byte
	err = db.QueryRow(`SELECT lembed('minilm', ?)`, "hello world").Scan(&blob)
	if err == nil {
		t.Fatal("lembed succeeded with no model registered; the ConnectHook test proves nothing")
	}
	if !strings.Contains(err.Error(), "minilm") && !strings.Contains(err.Error(), "model") {
		t.Logf("unexpected error text (still a failure, which is what matters): %v", err)
	}
}

func slicesContains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
