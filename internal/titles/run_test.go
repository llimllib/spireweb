package titles

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/llimllib/spireweb/internal/index"
)

const hdr = `{"type":"session","version":3,"id":"%s","timestamp":"2026-03-31T12:26:0%d.000Z","cwd":"/Users/me/code/proj"}`

func userMsg(text string) string {
	return `{"type":"message","message":{"role":"user","content":[{"type":"text","text":"` + text + `"}]}}`
}

// imageMsg is a message with no prose in it. A session of nothing but these is
// indexed -- it has messages -- and has nothing for a summarizer to read.
func imageMsg() string {
	return `{"type":"message","message":{"role":"user","content":[{"type":"image"}]}}`
}

// corpus writes session files and returns the directory holding them.
func corpus(t *testing.T, specs map[string][]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "sessions", "--Users-me-code-proj--")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	i := 0
	for id, msgs := range specs {
		lines := append([]string{fmt.Sprintf(hdr, id, i)}, msgs...)
		if err := os.WriteFile(filepath.Join(dir, id+".jsonl"),
			[]byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		i++
	}
	return filepath.Dir(dir)
}

// indexed builds an index over a corpus and returns both.
func indexed(t *testing.T, specs map[string][]string) (*index.DB, string) {
	t.Helper()
	dir := corpus(t, specs)
	db, err := index.Open(filepath.Join(t.TempDir(), "i.db"), index.DriverName)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := index.Build(context.Background(), db, index.BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}
	return db, dir
}

// fake counts calls and returns a title derived from the slice, so a test can
// tell which session a title came from.
type fake struct {
	calls atomic.Int32
	err   error
}

func (f *fake) Name() string { return "fake-model" }

func (f *fake) Summarize(ctx context.Context, slice string) (string, error) {
	f.calls.Add(1)
	if f.err != nil {
		return "", f.err
	}
	return "Title for " + strings.Fields(slice)[1], nil
}

func titleOf(t *testing.T, db *index.DB, id string) string {
	t.Helper()
	s, err := db.Session(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s.Title
}

func TestRunTitlesEverySession(t *testing.T) {
	db, _ := indexed(t, map[string][]string{
		"s1": {userMsg("alpha question")},
		"s2": {userMsg("beta question")},
	})
	f := &fake{}

	p, err := Run(context.Background(), db, Options{Summarizer: f})
	if err != nil {
		t.Fatal(err)
	}
	if p.Titled != 2 || p.Failed != 0 {
		t.Errorf("progress = %+v, want 2 titled", p)
	}
	if got := titleOf(t, db, "s1"); got != "Title for alpha" {
		t.Errorf("s1 title = %q", got)
	}
	if n, _ := db.CountTitledSessions(context.Background()); n != 2 {
		t.Errorf("titled sessions = %d, want 2", n)
	}
	if m, _ := db.Meta(index.MetaTitleModel); m != "fake-model" {
		t.Errorf("title_model = %q, want the model recorded", m)
	}
}

// The expensive half of the cache: a second pass must not call the model at
// all. 332 sessions at a fraction of a cent each is fine once and not fine on
// every reindex.
func TestRunDoesNotResummarizeUnchangedSessions(t *testing.T) {
	db, _ := indexed(t, map[string][]string{"s1": {userMsg("alpha question")}})
	f := &fake{}
	if _, err := Run(context.Background(), db, Options{Summarizer: f}); err != nil {
		t.Fatal(err)
	}

	p, err := Run(context.Background(), db, Options{Summarizer: f})
	if err != nil {
		t.Fatal(err)
	}
	if n := f.calls.Load(); n != 1 {
		t.Errorf("summarizer calls = %d, want 1: the second pass had nothing to do", n)
	}
	if p.Total != 0 {
		t.Errorf("total = %d, want 0 candidates once every session is checked", p.Total)
	}
	if got := titleOf(t, db, "s1"); got != "Title for alpha" {
		t.Errorf("title = %q, want it kept", got)
	}
}

// The cheap half: a long session that grew is re-examined, but its opening
// prose is past the slice bound and hashes the same, so it is not sent to the
// model again. This is the case that matters -- a session someone is working
// in is reindexed on every message -- and the reason the slice is bounded.
func TestRunRehashesGrownSessionWithoutCallingTheModel(t *testing.T) {
	long := "alpha " + strings.Repeat("question about the indexer ", 500)
	specs := map[string][]string{"s1": {userMsg(long)}}
	db, dir := indexed(t, specs)
	f := &fake{}
	if _, err := Run(context.Background(), db, Options{Summarizer: f}); err != nil {
		t.Fatal(err)
	}

	// Append a message, the way pi does, and reindex.
	p := filepath.Join(dir, "--Users-me-code-proj--", "s1.jsonl")
	file, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(userMsg("a follow-up") + "\n"); err != nil {
		t.Fatal(err)
	}
	file.Close()
	if err := os.Chtimes(p, time.Now(), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Build(context.Background(), db, index.BuildOptions{Dirs: []string{dir}}); err != nil {
		t.Fatal(err)
	}

	pr, err := Run(context.Background(), db, Options{Summarizer: f})
	if err != nil {
		t.Fatal(err)
	}
	if pr.Total != 1 {
		t.Errorf("total = %d, want the grown session re-examined", pr.Total)
	}
	if pr.Cached != 1 {
		t.Errorf("progress = %+v, want the grown session served from the key", pr)
	}
	if n := f.calls.Load(); n != 1 {
		t.Errorf("summarizer calls = %d, want 1: growth alone is not a new subject", n)
	}
	// And the session is settled again, so it is not even parsed next time.
	if _, err := Run(context.Background(), db, Options{Summarizer: f}); err != nil {
		t.Fatal(err)
	}
	if n := f.calls.Load(); n != 1 {
		t.Errorf("summarizer calls = %d after a third pass, want 1", n)
	}
}

// A failure must leave the row alone in every respect, so the next run retries
// it and the list keeps showing the opening message meanwhile.
func TestRunLeavesFallbackOnFailure(t *testing.T) {
	db, _ := indexed(t, map[string][]string{"s1": {userMsg("alpha question")}})

	p, err := Run(context.Background(), db, Options{Summarizer: &fake{err: errNope}})
	if err != nil {
		t.Fatalf("a failed summary must not fail the pass: %v", err)
	}
	if p.Failed != 1 || p.Titled != 0 {
		t.Errorf("progress = %+v, want 1 failed", p)
	}
	if got := titleOf(t, db, "s1"); got != "" {
		t.Errorf("title = %q, want none", got)
	}

	// Retried, not written off.
	f := &fake{}
	if _, err := Run(context.Background(), db, Options{Summarizer: f}); err != nil {
		t.Fatal(err)
	}
	if n := f.calls.Load(); n != 1 {
		t.Errorf("summarizer calls = %d, want the failed session retried", n)
	}
}

var errNope = fmt.Errorf("anthropic: 401 invalid x-api-key")

// Messages, but not a word of text between them: the pass has to settle the
// row without paying for a call. A session with no messages at all cannot
// stand in for this -- index.Build excludes those before they reach the
// titles pass, so it would test an empty database instead.
func TestRunSkipsSessionsWithNoProse(t *testing.T) {
	db, _ := indexed(t, map[string][]string{"s1": {imageMsg()}})
	f := &fake{}
	p, err := Run(context.Background(), db, Options{Summarizer: f})
	if err != nil {
		t.Fatal(err)
	}
	if p.Skipped != 1 {
		t.Errorf("progress = %+v, want the empty session skipped", p)
	}
	if n := f.calls.Load(); n != 0 {
		t.Errorf("summarizer calls = %d, want 0", n)
	}
}

func TestRunStopsAtLimit(t *testing.T) {
	specs := map[string][]string{}
	for i := range 20 {
		specs[fmt.Sprintf("s%02d", i)] = []string{userMsg(fmt.Sprintf("question%02d", i))}
	}
	db, _ := indexed(t, specs)

	f := &fake{}
	p, err := Run(context.Background(), db, Options{Summarizer: f, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if p.Titled != 5 {
		t.Errorf("titled = %d, want 5", p.Titled)
	}
	// Workers run ahead of the writer, so a few more calls than the limit are
	// expected; the whole corpus is not.
	if n := f.calls.Load(); int(n) > 5+DefaultConcurrency {
		t.Errorf("summarizer calls = %d, want the pass to stop near the limit", n)
	}
}

func TestRunReturnsPromptlyOnCancellation(t *testing.T) {
	specs := map[string][]string{}
	for i := range 50 {
		specs[fmt.Sprintf("s%02d", i)] = []string{userMsg(fmt.Sprintf("question%02d", i))}
	}
	db, _ := indexed(t, specs)

	ctx, cancel := context.WithCancel(context.Background())
	blocked := &blocking{release: make(chan struct{}), started: make(chan struct{}, 100)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := Run(ctx, db, Options{Summarizer: blocked}); err != nil {
			t.Errorf("cancellation is not an error: %v", err)
		}
	}()

	<-blocked.started
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// blocking hangs until its context is cancelled, standing in for an in-flight
// API call at shutdown.
type blocking struct {
	release chan struct{}
	started chan struct{}
}

func (b *blocking) Name() string { return "blocking" }

func (b *blocking) Summarize(ctx context.Context, slice string) (string, error) {
	select {
	case b.started <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-b.release:
		return "title", nil
	}
}
