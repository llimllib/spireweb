// Package indexer keeps the search index current while the server runs.
//
// It owns the only writer connection. Search handlers read through a separate
// pool, and WAL lets them read a consistent snapshot while a reindex is in
// flight, so indexing never blocks a request.
package indexer

import (
	"context"
	"sync"
	"time"

	"github.com/llimllib/spireweb/internal/index"
	"github.com/llimllib/spireweb/internal/titles"
)

// Phase is what the indexer is doing.
type Phase string

const (
	// PhaseStarting covers the catch-up build that runs before watching:
	// sessions written while the server was down.
	PhaseStarting Phase = "starting"
	PhaseIndexing Phase = "indexing"

	// PhaseTitling covers the summarizing pass that follows a build. It is a
	// phase of its own because it is slow, it runs against a network service,
	// and "indexing" going quiet for a minute is not a useful thing to show.
	PhaseTitling Phase = "titling"

	PhaseWatching Phase = "watching"
	PhaseStopped  Phase = "stopped"
)

// Status is a snapshot for the UI. Copied by value under the lock, so a
// handler never reads a half-written update.
type Status struct {
	Phase Phase

	// Done and Total are meaningful during the catch-up build.
	Done  int
	Total int

	// Sessions is how many sessions are currently indexed. The list pane
	// compares it against the count it rendered with to notice new arrivals.
	Sessions int

	Chunks  int
	LastRun time.Time
	LastErr string

	// TitleErr is kept apart from LastErr: a summarizing failure means the
	// list shows opening messages instead of titles, not that the index is
	// broken, and conflating them would report a network problem as data loss.
	TitleErr string
}

// Busy reports whether work is in progress, which is what decides how often
// the UI polls.
func (s Status) Busy() bool {
	return s.Phase == PhaseStarting || s.Phase == PhaseIndexing || s.Phase == PhaseTitling
}

// Indexer runs a catch-up build and then watches for changes.
type Indexer struct {
	db     *index.DB
	opts   index.BuildOptions
	titles titles.Options

	mu     sync.Mutex
	status Status
}

// New returns an Indexer over a writer handle. The caller keeps ownership of
// db and must not write through it elsewhere: lembed's llama_context is not
// safe for concurrent use, which is why the writer is a single connection.
//
// A zero titles.Options -- no summarizer -- means titles are not generated,
// which is the normal state of a machine with no API key.
func New(db *index.DB, opts index.BuildOptions, t titles.Options) *Indexer {
	return &Indexer{db: db, opts: opts, titles: t, status: Status{Phase: PhaseStarting}}
}

// Status returns the current snapshot.
func (i *Indexer) Status() Status {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.status
}

func (i *Indexer) update(f func(*Status)) {
	i.mu.Lock()
	defer i.mu.Unlock()
	f(&i.status)
}

// countSessions refreshes the indexed session count. Failures are ignored:
// this drives a cosmetic affordance, not correctness.
func (i *Indexer) countSessions(ctx context.Context) {
	if n, err := i.db.CountSessions(ctx); err == nil {
		i.update(func(s *Status) { s.Sessions = n })
	}
}

// Run catches up, then watches until ctx is cancelled.
//
// The catch-up build finishes before the watcher starts, deliberately. Both
// write, and the writer is one connection; letting them overlap would mean two
// goroutines embedding through the same llama_context, which segfaults rather
// than returning an error.
func (i *Indexer) Run(ctx context.Context, watch bool) error {
	i.update(func(s *Status) { s.Phase = PhaseStarting })

	var lastReport time.Time
	_, err := index.Build(ctx, i.db, i.withProgress(&lastReport))
	if err != nil {
		i.update(func(s *Status) {
			s.Phase = PhaseStopped
			s.LastErr = err.Error()
		})
		return err
	}
	i.countSessions(ctx)
	i.titlePass(ctx)
	i.update(func(s *Status) {
		s.Phase = PhaseWatching
		s.LastRun = time.Now()
		s.Done, s.Total = 0, 0
	})

	if !watch {
		i.update(func(s *Status) { s.Phase = PhaseStopped })
		return nil
	}

	w, err := index.NewWatcher(i.db, i.opts)
	if err != nil {
		i.update(func(s *Status) { s.LastErr = err.Error() })
		return err
	}
	defer w.Close()

	err = w.Run(ctx, func(ev index.WatchEvent) {
		if ev.Err != nil {
			i.update(func(s *Status) { s.LastErr = ev.Err.Error() })
			return
		}
		i.update(func(s *Status) {
			s.Chunks += ev.Chunks
			s.LastRun = time.Now()
			s.LastErr = ""
		})
		i.countSessions(ctx)

		// After the build, never during: both write, and the writer is one
		// connection. Titling a session pi is still adding to is cheap, since
		// the key only moves when the conversation has grown substantially.
		i.titlePass(ctx)
		i.update(func(s *Status) {
			s.Phase = PhaseWatching
			s.Done, s.Total = 0, 0
		})
	})
	i.update(func(s *Status) { s.Phase = PhaseStopped })
	return err
}

// titlePass generates titles for sessions that need one.
//
// Failures are recorded and otherwise ignored. This depends on a network
// service and an API key; a server whose header said "indexing failed" because
// a summary request was rate limited would be reporting the wrong thing about
// the wrong subsystem. The list falls back to the opening message either way.
func (i *Indexer) titlePass(ctx context.Context) {
	if i.titles.Summarizer == nil || ctx.Err() != nil {
		return
	}
	// Announced before the first result rather than after it: progress only
	// arrives once a session has been summarized, and the wait for that is
	// exactly the part worth labelling.
	i.update(func(s *Status) {
		s.Phase = PhaseTitling
		s.Done, s.Total = 0, 0
	})

	opts := i.titles
	var last time.Time
	opts.OnProgress = func(p titles.Progress) {
		if !p.Finished && time.Since(last) < 200*time.Millisecond {
			return
		}
		last = time.Now()
		i.update(func(s *Status) {
			s.Phase = PhaseTitling
			s.Done, s.Total = p.Done, p.Total
		})
	}
	if _, err := titles.Run(ctx, i.db, opts); err != nil && ctx.Err() == nil {
		i.update(func(s *Status) { s.TitleErr = err.Error() })
	}
}

// withProgress returns build options that publish progress, rate-limited.
//
// A skipped file takes microseconds, and there are eleven hundred of them;
// taking the lock for every one would cost more than the skipping does.
func (i *Indexer) withProgress(last *time.Time) index.BuildOptions {
	opts := i.opts
	opts.OnProgress = func(p index.Progress) {
		if !p.Finished && time.Since(*last) < 200*time.Millisecond {
			return
		}
		*last = time.Now()
		i.update(func(s *Status) {
			s.Done, s.Total = p.Done, p.Total
			s.Chunks = p.Chunks
			if p.Indexed > 0 && !p.Finished {
				s.Phase = PhaseIndexing
			}
		})
	}
	return opts
}
