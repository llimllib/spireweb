package index

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/llimllib/spireweb/internal/session"
)

// installSession writes a one-message pi session into dir/<id>.jsonl.
func installSession(t *testing.T, dir, id, text string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	head := strings.Replace(strings.Replace(hdr, "%s", id, 1), "%s", "proj", 1)
	body := head + "\n" + userMsg(text) + "\n"
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The reason Dirs is a slice rather than Build being called once per directory:
// Build removes every indexed session it did not see, so a build per root would
// have each one delete the other's.
func TestBuildIndexesSeveralDirectoriesAsOneCorpus(t *testing.T) {
	piDir := writeCorpus(t, map[string][]string{"pi-1": {userMsg("a pi session")}})
	ccDir := t.TempDir()
	writeClaudeSession(t, ccDir, "cc-1", "cli", "a claude code session")

	db := openTest(t)
	p, err := Build(context.Background(), db, BuildOptions{Dirs: []string{piDir, ccDir}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Indexed != 2 {
		t.Errorf("Indexed = %d, want 2", p.Indexed)
	}

	got := indexedIDs(t, db)
	for _, id := range []string{"pi-1", "cc-1"} {
		if !got[id] {
			t.Errorf("%s is not indexed", id)
		}
	}

	// And a second build keeps both, rather than each root's sweep claiming
	// the other's sessions are gone.
	if _, err := Build(context.Background(), db, BuildOptions{Dirs: []string{piDir, ccDir}}); err != nil {
		t.Fatal(err)
	}
	if n := len(indexedIDs(t, db)); n != 2 {
		t.Errorf("after a second build, %d sessions indexed, want 2", n)
	}
}

// Sessions from a directory that is no longer listed are dropped, which is the
// same sweep and has to keep working.
func TestBuildDropsSessionsFromARemovedDirectory(t *testing.T) {
	piDir := writeCorpus(t, map[string][]string{"pi-1": {userMsg("a pi session")}})
	ccDir := t.TempDir()
	writeClaudeSession(t, ccDir, "cc-1", "cli", "a claude code session")

	db := openTest(t)
	ctx := context.Background()
	if _, err := Build(ctx, db, BuildOptions{Dirs: []string{piDir, ccDir}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(ctx, db, BuildOptions{Dirs: []string{piDir}}); err != nil {
		t.Fatal(err)
	}

	got := indexedIDs(t, db)
	if !got["pi-1"] {
		t.Error("the listed directory's session was dropped")
	}
	if got["cc-1"] {
		t.Error("a session from an unlisted directory stayed in the index")
	}
}

// Naming a directory and its parent is a plausible configuration mistake.
// Indexing those files twice would be a confusing one.
func TestDiscoverDeduplicatesOverlappingRoots(t *testing.T) {
	dir := writeCorpus(t, map[string][]string{"s1": {userMsg("hello")}})

	one, err := session.Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	both, err := session.Discover(dir, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(both) != len(one) {
		t.Errorf("got %d files from overlapping roots, want %d", len(both), len(one))
	}
}

// A path that does not exist is still an error: silently indexing nothing
// turns a typo into an empty interface with no explanation.
func TestDiscoverStillRejectsAMissingRoot(t *testing.T) {
	good := writeCorpus(t, map[string][]string{"s1": {userMsg("hello")}})
	if _, err := session.Discover(good, t.TempDir()+"/absent"); err == nil {
		t.Error("Discover() = nil error for a missing root")
	}
	if _, err := session.Discover(); err == nil {
		t.Error("Discover() with no roots = nil error")
	}
}

// The watcher takes the same list, and a session appearing in the second root
// has to be noticed. Watching only the first would leave one agent's sessions
// updating live and the other's stale until a restart.
func TestWatcherWatchesEveryRoot(t *testing.T) {
	piDir := writeCorpus(t, map[string][]string{"pi-1": {userMsg("a pi session")}})
	ccDir := t.TempDir()
	// The project directory exists before the watcher starts. Creating it
	// afterwards races: fsnotify reports the new directory and the file inside
	// it as two events, and the file can be written before the watch on its
	// parent is established. That window is not what this test is about.
	writeClaudeSession(t, ccDir, "cc-early", "cli", "already here")

	db := openTest(t)
	ctx := context.Background()
	dirs := []string{piDir, ccDir}
	if _, err := Build(ctx, db, BuildOptions{Dirs: dirs}); err != nil {
		t.Fatal(err)
	}

	w, err := NewWatcher(db, BuildOptions{Dirs: dirs})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go w.Run(wctx, nil)

	// Written after the watcher started, into the root that had nothing.
	writeClaudeSession(t, ccDir, "cc-late", "cli", "arrived after the watcher did")

	waitFor(t, 10*time.Second, "a session in the second root to be indexed", func() bool {
		return indexedIDs(t, db)["cc-late"]
	})
}

// The race #58 describes: creating a project directory and writing the first
// session into it are two operations, and the write can land before the watch
// on the new directory exists. Without the sweep in noteExisting, the session
// that created the directory is the one that goes missing.
func TestWatcherCatchesASessionWrittenWithItsDirectory(t *testing.T) {
	root := t.TempDir()
	installSession(t, filepath.Join(root, "seed"), "seed-1", "already here")

	db := openTest(t)
	ctx := context.Background()
	if _, err := Build(ctx, db, BuildOptions{Dirs: []string{root}}); err != nil {
		t.Fatal(err)
	}

	w, err := NewWatcher(db, BuildOptions{Dirs: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go w.Run(wctx, nil)

	// Directory and file together, with nothing in between, which is what an
	// agent starting a session in a new project does.
	installSession(t, filepath.Join(root, "brand-new"), "new-1", "the first session here")

	waitFor(t, 10*time.Second, "a session written with its directory to be indexed", func() bool {
		return indexedIDs(t, db)["new-1"]
	})
}

// Build's filter is not enough on its own: the watcher reindexes changed files
// directly, and that is the path a title prompt arrives by. The titles pass
// shells out to `claude -p` while the server runs, writing a session into a
// watched directory -- so a server generating titles produces exactly the files
// this excludes, and indexing one makes it a candidate for titling.
func TestWatcherExcludesSDKSessions(t *testing.T) {
	root := t.TempDir()
	installSession(t, filepath.Join(root, "seed"), "seed-1", "already here")

	db := openTest(t)
	ctx := context.Background()
	if _, err := Build(ctx, db, BuildOptions{Dirs: []string{root}}); err != nil {
		t.Fatal(err)
	}

	w, err := NewWatcher(db, BuildOptions{Dirs: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go w.Run(wctx, nil)

	// What `claude -p` leaves behind, and a real session alongside it.
	writeClaudeSession(t, root, "titler", "sdk-cli", "You write short titles")
	writeClaudeSession(t, root, "real", "cli", "an actual conversation")

	waitFor(t, 10*time.Second, "the interactive session to be indexed", func() bool {
		return indexedIDs(t, db)["real"]
	})
	// A further settle before asserting the absence. The two files may not
	// land in the same batch, and "not indexed yet" would otherwise pass for
	// the same reason "never indexed" does.
	time.Sleep(2 * WatchSettle)
	if indexedIDs(t, db)["titler"] {
		t.Error("the watcher indexed a title prompt; Build's filter does not cover this path")
	}
}

// A file indexed before the rule reached it is dropped the next time it
// changes, rather than surviving until --full.
func TestWatcherDropsASessionThatBecomesExcluded(t *testing.T) {
	root := t.TempDir()
	p := writeClaudeSession(t, root, "leaked", "cli", "indexed before the rule")

	db := openTest(t)
	ctx := context.Background()
	if _, err := Build(ctx, db, BuildOptions{Dirs: []string{root}}); err != nil {
		t.Fatal(err)
	}
	if !indexedIDs(t, db)["leaked"] {
		t.Fatal("setup: not indexed")
	}

	w, err := NewWatcher(db, BuildOptions{Dirs: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go w.Run(wctx, nil)

	line := `{"type":"user","sessionId":"leaked","timestamp":"2026-09-11T13:35:05Z",` +
		`"cwd":"/Users/me/code/proj","entrypoint":"sdk-cli",` +
		`"message":{"role":"user","content":[{"type":"text","text":"now an sdk session"}]}}`
	if err := os.WriteFile(p, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 10*time.Second, "the now-excluded session to be dropped", func() bool {
		return !indexedIDs(t, db)["leaked"]
	})
}
