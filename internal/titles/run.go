package titles

import (
	"context"
	"errors"
	"sync"

	"github.com/llimllib/spireweb/internal/index"
	"github.com/llimllib/spireweb/internal/session"
)

// DefaultConcurrency is how many summaries are in flight at once.
//
// The work is entirely latency: a request takes a second or two and the
// process does nothing while it waits. Eight keeps a thousand sessions to a
// few minutes without being the reason an account hits its rate limit -- and
// when it does, the client backs off rather than the pass failing.
const DefaultConcurrency = 8

// Progress reports how a pass is going, on the same terms as index.Progress.
type Progress struct {
	Total   int // sessions that might need a title
	Done    int // candidates resolved, whatever the outcome
	Titled  int // summaries written
	Cached  int // opening prose unchanged, so no call was made
	Skipped int // nothing to summarize
	Failed  int

	// FirstErr is why the first failure failed. Individual failures are not
	// fatal, so without this a run reports a count and no reason, and the
	// reason is usually one thing wrong with the configuration rather than a
	// hundred unrelated problems.
	FirstErr error

	Finished bool
}

// Options configures a pass.
type Options struct {
	Summarizer  Summarizer
	Concurrency int            // 0 uses DefaultConcurrency
	OnProgress  func(Progress) // optional

	// Limit caps how many titles are generated in one pass, 0 for no limit.
	// Candidates come newest first, so a limited run titles what a browser is
	// most likely to look at.
	Limit int
}

// Run generates titles for sessions that need one.
//
// Reads and summarization happen on worker goroutines; every write goes
// through the caller's goroutine. That is not for safety -- database/sql would
// serialize them anyway -- but because the writer is a single connection, and
// funnelling the updates here keeps the progress count and the database in
// step.
//
// A cancelled context stops the pass promptly and is not an error: whatever
// was written is committed and correct, and the next run picks up the rest.
func Run(ctx context.Context, db *index.DB, opts Options) (Progress, error) {
	if opts.Summarizer == nil {
		return Progress{}, errors.New("titles: no summarizer configured")
	}
	workers := opts.Concurrency
	if workers <= 0 {
		workers = DefaultConcurrency
	}

	candidates, err := db.TitleCandidates(ctx)
	if err != nil {
		return Progress{}, err
	}
	if len(candidates) == 0 {
		p := Progress{Finished: true}
		report(opts, p)
		return p, nil
	}

	if err := db.SetMeta(index.MetaTitleModel, opts.Summarizer.Name()); err != nil {
		return Progress{}, err
	}

	// Cancelled when the pass stops early, so that hitting Limit stops the
	// workers rather than leaving them to summarize -- and pay for -- the rest
	// of the corpus into a channel nobody is reading.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	p := Progress{Total: len(candidates)}
	jobs := make(chan index.TitleCandidate)
	results := make(chan result)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range jobs {
				select {
				case results <- summarize(ctx, opts.Summarizer, c):
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, c := range candidates {
			select {
			case jobs <- c:
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	generated := 0
	for r := range results {
		p.Done++
		var werr error
		switch {
		case r.err != nil:
			// Leave title and title_key alone: the row keeps its fallback and
			// the next run tries again. Nothing is recorded that would make
			// this session look up to date.
			p.Failed++
			if p.FirstErr == nil {
				p.FirstErr = r.err
			}
		case r.slice == "":
			// A session with no prose at all -- a file pi opened and nothing
			// came of. Mark it checked so it is not parsed again every run.
			p.Skipped++
			werr = db.MarkTitleChecked(ctx, r.id, r.nMsgs)
		case r.title == "":
			p.Cached++
			werr = db.MarkTitleChecked(ctx, r.id, r.nMsgs)
		default:
			p.Titled++
			generated++
			werr = db.SetTitle(ctx, r.id, r.title, r.key, r.nMsgs)
		}
		if werr != nil {
			// A write that failed because the process is shutting down is not
			// a failure worth reporting: the pass is resumable by design.
			if ctx.Err() != nil {
				break
			}
			return p, werr
		}
		report(opts, p)

		if opts.Limit > 0 && generated >= opts.Limit {
			break
		}
	}

	p.Finished = true
	report(opts, p)
	return p, nil
}

// result is one candidate's outcome. An empty title with no error means the
// stored key still matches, so nothing was sent to the model.
type result struct {
	id    string
	nMsgs int
	slice string
	key   string
	title string
	err   error
}

// summarize parses a session, decides whether it needs a new title, and asks
// for one if it does.
//
// Parsing here rather than in the caller is the reason the pass is concurrent
// at all for a cached run: hashing 1128 files is the work, and it parallelizes.
func summarize(ctx context.Context, s Summarizer, c index.TitleCandidate) result {
	r := result{id: c.ID, nMsgs: c.NumMsgs}

	sess, err := session.Parse(c.Path)
	if err != nil {
		// The file is gone or unreadable. Indexing will notice and remove the
		// row; there is nothing useful to do here.
		r.err = err
		return r
	}

	r.slice = Slice(sess)
	if r.slice == "" {
		return r
	}
	r.key = Key(r.slice)
	if r.key == c.Key {
		return r // opening prose unchanged: the stored title still describes it
	}

	title, err := s.Summarize(ctx, r.slice)
	if err != nil {
		r.err = err
		return r
	}
	if title == "" {
		r.err = errors.New("empty title")
		return r
	}
	r.title = title
	return r
}

func report(opts Options, p Progress) {
	if opts.OnProgress != nil {
		opts.OnProgress(p)
	}
}
