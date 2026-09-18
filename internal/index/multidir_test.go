package index

import (
	"context"
	"testing"
	"time"

	"github.com/llimllib/spireweb/internal/session"
)

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
