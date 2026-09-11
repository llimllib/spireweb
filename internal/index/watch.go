package index

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/llimllib/spireweb/internal/session"
)

// WatchSettle is how long to wait after the last filesystem event before
// reindexing.
//
// pi appends to a session once per message, so events arrive in bursts with long
// gaps rather than continuously (a live session was observed changing zero times
// in a 20-second window while idle). Waiting for the burst to end means one
// reindex per exchange instead of one per write, and keeps a partially written
// final line from being parsed repeatedly.
const WatchSettle = 2 * time.Second

// WatchEvent reports the result of a reindex triggered by file changes.
type WatchEvent struct {
	Files   []string // session files that changed
	Chunks  int      // chunks added
	Elapsed time.Duration
	Err     error
}

// Watcher reindexes sessions as their files change.
//
// The session directory is a two-level tree: one directory per project, each
// containing session files. fsnotify does not watch recursively on macOS, so
// every project directory is watched individually and new ones are added as they
// appear -- starting a pi session in a new project creates a directory.
type Watcher struct {
	db   *DB
	opts BuildOptions
	root string

	fs      *fsnotify.Watcher
	mu      sync.Mutex
	pending map[string]bool
}

// NewWatcher prepares a watcher over opts.Dir.
func NewWatcher(db *DB, opts BuildOptions) (*Watcher, error) {
	if opts.Dir == "" {
		opts.Dir = session.DefaultDir()
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	w := &Watcher{
		db:      db,
		opts:    opts,
		root:    opts.Dir,
		fs:      fsw,
		pending: map[string]bool{},
	}
	if err := w.addTree(); err != nil {
		fsw.Close()
		return nil, err
	}
	return w, nil
}

// addTree watches the root and every project directory beneath it.
func (w *Watcher) addTree() error {
	if err := w.fs.Add(w.root); err != nil {
		return err
	}
	entries, err := os.ReadDir(w.root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			// Ignore errors: a directory may vanish between listing and adding,
			// and one unwatchable project should not prevent watching the rest.
			_ = w.fs.Add(filepath.Join(w.root, e.Name()))
		}
	}
	return nil
}

// Close releases the watcher.
func (w *Watcher) Close() error { return w.fs.Close() }

// Run reindexes changed sessions until ctx is cancelled, calling onEvent after
// each batch. It returns nil on cancellation.
//
// Only the files that changed are reindexed, not the whole corpus: a full
// Discover pass over ~950 files costs about 40ms, but reindexing one changed
// session costs under 100ms thanks to chunk reuse, so scoping the work keeps a
// burst of edits cheap.
func (w *Watcher) Run(ctx context.Context, onEvent func(WatchEvent)) error {
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	armed := false

	for {
		select {
		case <-ctx.Done():
			return nil

		case err, ok := <-w.fs.Errors:
			if !ok {
				return nil
			}
			if onEvent != nil && err != nil {
				onEvent(WatchEvent{Err: err})
			}

		case ev, ok := <-w.fs.Events:
			if !ok {
				return nil
			}
			if !w.note(ev) {
				continue
			}
			// Restart the settle timer on every relevant event, so a burst of
			// writes produces one reindex.
			if armed && !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(WatchSettle)
			armed = true

		case <-timer.C:
			armed = false
			files := w.drain()
			if len(files) == 0 {
				continue
			}
			start := time.Now()
			n, err := w.reindex(ctx, files)
			if onEvent != nil {
				onEvent(WatchEvent{
					Files: files, Chunks: n,
					Elapsed: time.Since(start), Err: err,
				})
			}
		}
	}
}

// note records a relevant event and reports whether it should trigger a
// reindex.
func (w *Watcher) note(ev fsnotify.Event) bool {
	// A new project directory means a new pi session; start watching it.
	if ev.Op&(fsnotify.Create|fsnotify.Rename) != 0 {
		if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
			_ = w.fs.Add(ev.Name)
			return false
		}
	}
	if !strings.HasSuffix(ev.Name, ".jsonl") {
		return false
	}
	// Chmod alone carries no content change; ignore it to avoid pointless work.
	if ev.Op == fsnotify.Chmod {
		return false
	}

	w.mu.Lock()
	w.pending[ev.Name] = true
	w.mu.Unlock()
	return true
}

func (w *Watcher) drain() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) == 0 {
		return nil
	}
	out := make([]string, 0, len(w.pending))
	for p := range w.pending {
		out = append(out, p)
	}
	w.pending = map[string]bool{}
	return out
}

// reindex updates just the given session files.
func (w *Watcher) reindex(ctx context.Context, paths []string) (int, error) {
	chunks := 0
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return chunks, err
		}
		fi, err := os.Stat(p)
		if err != nil {
			// Deleted: drop it from the index.
			if os.IsNotExist(err) {
				if derr := w.db.deleteByPath(p); derr != nil {
					return chunks, derr
				}
				continue
			}
			return chunks, err
		}
		if fi.IsDir() {
			continue
		}

		// WithRaw so that live indexing archives too; a session written while
		// the server runs must not be the one session missing from the archive.
		s, err := session.ParseWithRaw(p)
		if err != nil {
			// A session may be mid-write, or not yet have a header. Skipping is
			// correct: the next event will pick it up.
			continue
		}
		n, _, err := w.db.upsertSession(ctx, s, w.opts)
		if err != nil {
			return chunks, err
		}
		chunks += n
	}
	return chunks, nil
}
