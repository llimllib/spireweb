package index

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// waitFor polls until cond holds or the deadline passes, so tests do not depend
// on filesystem event timing.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", d, what)
}

func TestWatcherIndexesNewSession(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "--Users-me-code-proj--")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	db := openTest(t)

	w, err := NewWatcher(db, BuildOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var events []WatchEvent
	go w.Run(ctx, func(ev WatchEvent) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	})

	body := strings.Replace(strings.Replace(hdr, "%s", "new-1", 1), "%s", "proj", 1) +
		"\n" + userMsg("a brand new session about deadlocks") + "\n"
	if err := os.WriteFile(filepath.Join(sub, "new.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 10*time.Second, "the new session to be indexed", func() bool {
		st, err := db.Stats()
		return err == nil && st.Sessions == 1 && st.Chunks == 1
	})

	mu.Lock()
	n := len(events)
	mu.Unlock()
	if n == 0 {
		t.Error("no watch event was reported")
	}
}

// Appending to a watched session must add only the new chunks.
func TestWatcherAppendReusesChunks(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "--Users-me-code-proj--")
	os.MkdirAll(sub, 0o755)
	p := filepath.Join(sub, "s.jsonl")

	head := strings.Replace(strings.Replace(hdr, "%s", "grow-w", 1), "%s", "proj", 1)
	if err := os.WriteFile(p, []byte(head+"\n"+userMsg("first")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	db := openTest(t)
	ctx := context.Background()
	if _, err := Build(ctx, db, BuildOptions{Dir: dir}); err != nil {
		t.Fatal(err)
	}
	before := chunkIDs(t, db, "grow-w")

	w, err := NewWatcher(db, BuildOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go w.Run(wctx, nil)

	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(userMsg("second message") + "\n")
	f.Close()

	waitFor(t, 10*time.Second, "the appended message to be indexed", func() bool {
		st, err := db.Stats()
		return err == nil && st.Chunks == 2
	})

	after := chunkIDs(t, db, "grow-w")
	for _, id := range before {
		if !contains(after, id) {
			t.Errorf("chunk %d was recreated; watcher did not reuse (before=%v after=%v)",
				id, before, after)
		}
	}
}

func TestWatcherRemovesDeletedSession(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "--Users-me-code-proj--")
	os.MkdirAll(sub, 0o755)
	p := filepath.Join(sub, "gone.jsonl")
	body := strings.Replace(strings.Replace(hdr, "%s", "gone-1", 1), "%s", "proj", 1) +
		"\n" + userMsg("temporary session") + "\n"
	os.WriteFile(p, []byte(body), 0o644)

	db := openTest(t)
	ctx := context.Background()
	if _, err := Build(ctx, db, BuildOptions{Dir: dir}); err != nil {
		t.Fatal(err)
	}

	w, err := NewWatcher(db, BuildOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go w.Run(wctx, nil)

	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "the deleted session to be dropped", func() bool {
		st, err := db.Stats()
		return err == nil && st.Sessions == 0 && st.Chunks == 0
	})
}

// A project directory created after startup must be watched: starting pi in a
// new project creates one, and fsnotify does not watch recursively on macOS.
func TestWatcherPicksUpNewProjectDir(t *testing.T) {
	dir := t.TempDir()
	db := openTest(t)

	w, err := NewWatcher(db, BuildOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx, nil)

	sub := filepath.Join(dir, "--Users-me-code-brand-new--")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// Give the watcher a moment to register the directory before writing into it.
	time.Sleep(300 * time.Millisecond)

	body := strings.Replace(strings.Replace(hdr, "%s", "fresh-1", 1), "%s", "brand-new", 1) +
		"\n" + userMsg("session in a new project") + "\n"
	if err := os.WriteFile(filepath.Join(sub, "s.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 10*time.Second, "a session in a new project directory to be indexed", func() bool {
		st, err := db.Stats()
		return err == nil && st.Sessions == 1
	})
}

// A burst of writes must settle into one reindex rather than one per event.
func TestWatcherCoalescesBurst(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "--Users-me-code-proj--")
	os.MkdirAll(sub, 0o755)
	p := filepath.Join(sub, "burst.jsonl")
	head := strings.Replace(strings.Replace(hdr, "%s", "burst-1", 1), "%s", "proj", 1)
	os.WriteFile(p, []byte(head+"\n"), 0o644)

	db := openTest(t)
	w, err := NewWatcher(db, BuildOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	reindexes := 0
	go w.Run(ctx, func(ev WatchEvent) {
		mu.Lock()
		reindexes++
		mu.Unlock()
	})

	// Ten appends in quick succession, as pi would during a turn.
	for i := 0; i < 10; i++ {
		f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(userMsg("burst message") + "\n")
		f.Close()
		time.Sleep(30 * time.Millisecond)
	}

	waitFor(t, 10*time.Second, "the burst to be indexed", func() bool {
		st, err := db.Stats()
		return err == nil && st.Chunks > 0
	})
	// Allow any further timers to fire.
	time.Sleep(WatchSettle)

	mu.Lock()
	n := reindexes
	mu.Unlock()
	if n > 3 {
		t.Errorf("burst of 10 writes caused %d reindexes; expected coalescing", n)
	}
}
