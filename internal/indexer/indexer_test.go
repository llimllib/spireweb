package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/llimllib/spireweb/internal/index"
)

func writeSession(t *testing.T, dir, id, text string) string {
	t.Helper()
	sub := filepath.Join(dir, "--Users-me-code-proj--")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"session","version":3,"id":"` + id +
		`","timestamp":"2026-03-31T12:26:01.076Z","cwd":"/Users/me/code/proj"}` + "\n" +
		`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"` +
		text + `"}]}}` + "\n"

	p := filepath.Join(sub, id+".jsonl")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func openWriter(t *testing.T) *index.DB {
	t.Helper()
	db, err := index.Open(filepath.Join(t.TempDir(), "i.db"), index.DriverName)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// waitFor polls until cond holds, so the test tracks the watcher's settle
// timer rather than guessing a sleep long enough to cover it.
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The catch-up pass exists for sessions written while the server was down.
func TestRunCatchesUpThenStops(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "aaa", "first")
	writeSession(t, dir, "bbb", "second")

	ix := New(openWriter(t), index.BuildOptions{Dir: dir})
	if err := ix.Run(context.Background(), false); err != nil {
		t.Fatal(err)
	}

	st := ix.Status()
	if st.Phase != PhaseStopped {
		t.Errorf("phase = %q, want %q", st.Phase, PhaseStopped)
	}
	if st.Sessions != 2 {
		t.Errorf("sessions = %d, want 2", st.Sessions)
	}
	if st.Busy() {
		t.Error("still reporting busy after finishing")
	}
	if st.LastRun.IsZero() {
		t.Error("LastRun not set")
	}
}

func TestWatchIndexesNewSessions(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "aaa", "the original session")

	db := openWriter(t)
	ix := New(db, index.BuildOptions{Dir: dir})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ix.Run(ctx, true) }()

	waitFor(t, "the catch-up build", 10*time.Second, func() bool {
		return ix.Status().Phase == PhaseWatching
	})
	if got := ix.Status().Sessions; got != 1 {
		t.Fatalf("sessions after catch-up = %d, want 1", got)
	}

	// A conversation started in another terminal.
	writeSession(t, dir, "bbb", "a session about ospreys")

	waitFor(t, "the new session to be indexed", 15*time.Second, func() bool {
		return ix.Status().Sessions == 2
	})

	// Indexed means searchable, not merely counted.
	var body string
	err := db.SQL().QueryRow(
		`SELECT body FROM chunks WHERE session_id = 'bbb'`).Scan(&body)
	if err != nil {
		t.Fatalf("new session has no chunks: %v", err)
	}
	if body != "a session about ospreys" {
		t.Errorf("chunk body = %q", body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Run did not return after cancellation")
	}
	if ix.Status().Phase != PhaseStopped {
		t.Errorf("phase = %q after cancel", ix.Status().Phase)
	}
}

func TestWatchNoticesDeletedSessions(t *testing.T) {
	dir := t.TempDir()
	p := writeSession(t, dir, "aaa", "a doomed session")

	ix := New(openWriter(t), index.BuildOptions{Dir: dir})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ix.Run(ctx, true) }()

	waitFor(t, "the catch-up build", 10*time.Second, func() bool {
		return ix.Status().Phase == PhaseWatching
	})

	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the deletion to be indexed", 15*time.Second, func() bool {
		return ix.Status().Sessions == 0
	})
}

// A build failure has to leave the status readable rather than wedged as
// busy, or the header claims it is indexing for as long as the tab is open.
func TestBuildFailureIsReported(t *testing.T) {
	ix := New(openWriter(t), index.BuildOptions{Dir: filepath.Join(t.TempDir(), "missing")})

	if err := ix.Run(context.Background(), false); err == nil {
		t.Fatal("expected an error for a nonexistent session directory")
	}
	st := ix.Status()
	if st.Phase != PhaseStopped {
		t.Errorf("phase = %q, want %q", st.Phase, PhaseStopped)
	}
	if st.LastErr == "" {
		t.Error("error not recorded in status")
	}
	if st.Busy() {
		t.Error("still busy after failing")
	}
}
