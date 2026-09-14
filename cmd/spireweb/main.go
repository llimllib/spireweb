// Command spireweb indexes and browses pi agent sessions.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/llimllib/spireweb/internal/embed"
	"github.com/llimllib/spireweb/internal/index"
	"github.com/llimllib/spireweb/internal/session"
	"github.com/llimllib/spireweb/internal/titles"
)

// Version is stamped in at build time by `mise run build`.
var Version = "dev"

const usage = `spireweb - search and read pi agent sessions

usage:
  spireweb serve [flags]   browse sessions in a web interface
  spireweb index [flags]   build or update the search index
  spireweb stats [flags]   report what is in the index
  spireweb doctor [flags]  check the index for inconsistencies
  spireweb info            show paths and configuration
  spireweb version

flags:
  --db PATH      index location (default %s)
  --dir PATH     session directory (default %s)
  --full         reindex everything rather than what changed
  --lexical      skip semantic indexing, even if the model is installed
  --addr ADDR    serve on this address (default 127.0.0.1:8080)
  --dev          reload templates and static files from disk per request
  --open         open a browser once the server is listening
  --no-watch     do not index in the background while serving
  --no-titles    do not generate session titles with an LLM
  --titles N     stop after generating N titles (0 for no limit)
  --titles-via   api (ANTHROPIC_API_KEY) or claude (the Claude Code CLI,
                 which bills a Pro/Max subscription rather than the API)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, usage, index.DefaultPath(), session.DefaultDir())
		os.Exit(2)
	}

	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	dbPath := fs.String("db", index.DefaultPath(), "index location")
	dir := fs.String("dir", session.DefaultDir(), "session directory")
	full := fs.Bool("full", false, "reindex everything")
	lexical := fs.Bool("lexical", false, "skip semantic indexing")
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	dev := fs.Bool("dev", false, "reload templates and static files from disk")
	openBrowser := fs.Bool("open", false, "open a browser once listening")
	noWatch := fs.Bool("no-watch", false, "do not index in the background")
	noTitles := fs.Bool("no-titles", false, "do not generate session titles")
	// Titling the whole corpus costs real money. A limit makes a trial run
	// possible -- newest sessions first, so it titles what is worth looking at
	// -- before committing to all eleven hundred.
	titleLimit := fs.Int("titles", 0, "stop after generating N titles")
	titleVia := fs.String("titles-via", titles.BackendAPI, "api or claude")
	fs.Usage = func() { fmt.Fprintf(os.Stderr, usage, index.DefaultPath(), session.DefaultDir()) }

	var err error
	switch cmd {
	case "serve":
		_ = fs.Parse(os.Args[2:])
		err = runServe(*dbPath, *addr, *dir, *dev, *openBrowser, *noWatch, *noTitles, *titleVia)
	case "index":
		_ = fs.Parse(os.Args[2:])
		err = runIndex(*dbPath, *dir, *full, *lexical, *noTitles, *titleLimit, *titleVia)
	case "stats":
		_ = fs.Parse(os.Args[2:])
		err = runStats(*dbPath)
	case "doctor":
		_ = fs.Parse(os.Args[2:])
		err = runDoctor(*dbPath)
	case "info":
		_ = fs.Parse(os.Args[2:])
		err = runInfo(*dbPath, *dir)
	case "version":
		fmt.Println("spireweb", Version)
	default:
		fs.Usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// openIndex opens the index with semantic support when it is available, and
// falls back to lexical-only when it is not.
//
// Falling back rather than failing is deliberate: keyword search over an index
// with no vectors is still useful, and the reasons semantic search is
// unavailable are mostly environmental -- the model is not downloaded, or
// llama.cpp cannot reach a GPU. The fallback is reported, never silent, because
// an index quietly built without vectors looks identical to one with them until
// someone notices the results got worse.
func openIndex(path string, lexical bool) (*index.DB, index.Embedder, error) {
	if err := index.CheckFTS5(); err != nil {
		return nil, nil, err
	}

	if lexical {
		db, _, err := index.OpenOrReset(path, index.DriverName)
		return db, nil, err
	}

	paths := embed.DefaultPaths()
	if err := index.RegisterSemanticDriver(paths); err != nil {
		note("semantic search unavailable: %v", err)
		db, _, err := index.OpenOrReset(path, index.DriverName)
		return db, nil, err
	}

	// A failure here is usually the driver's ConnectHook reporting that the
	// model would not load. Never continue past it on the semantic driver: a
	// connection with no registered model does not return errors from lembed(),
	// it segfaults the process.
	db, reset, err := index.OpenOrReset(path, index.SemanticDriverName)
	if err != nil {
		note("semantic search unavailable: %v", err)
		db, _, err := index.OpenOrReset(path, index.DriverName)
		return db, nil, err
	}
	if reset {
		note("index was written by a different schema version; rebuilding")
	}

	e, err := embed.New(db.SQL(), paths)
	if err != nil {
		db.Close()
		note("semantic search unavailable: %v", err)
		db, _, err := index.OpenOrReset(path, index.DriverName)
		return db, nil, err
	}
	if !e.IsPatchedFork() {
		db.Close()
		return nil, nil, fmt.Errorf(
			"sqlite-lembed %s is the unpatched upstream build, which crashes the process "+
				"on long or malformed input; run 'mise run setup'", e.Version())
	}
	if err := db.EnsureVectorTable(e.Dim()); err != nil {
		db.Close()
		return nil, nil, err
	}
	return db, e, nil
}

func runIndex(dbPath, dir string, full, lexical, noTitles bool, titleLimit int, titleVia string) error {
	db, embedder, err := openIndex(dbPath, lexical)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	start := time.Now()
	var lastReport time.Time
	var announcedBackfill, announcedArchive bool
	p, err := index.Build(ctx, db, index.BuildOptions{
		Dir:      dir,
		Full:     full,
		Embedder: embedder,
		OnProgress: func(p index.Progress) {
			// Rate-limited because a skipped file takes microseconds and
			// printing every one of them costs more than the indexing does.
			if p.Backfill && !announcedBackfill {
				announcedBackfill = true
				note("index has chunks without embeddings; reindexing in full to add them")
			}
			if p.ArchiveBackfill && !announcedArchive {
				announcedArchive = true
				note("index predates the message archive; reindexing in full to store them " +
					"(this corpus is ~265MB of messages, so expect the database to grow)")
			}
			if !p.Finished && time.Since(lastReport) < 100*time.Millisecond {
				return
			}
			lastReport = time.Now()
			fmt.Printf("\r\033[K%d/%d  indexed %d  skipped %d  failed %d",
				p.Done, p.Total, p.Indexed, p.Skipped, p.Failed)
		},
	})
	fmt.Println()
	if err != nil {
		return err
	}

	if full {
		// A full rebuild leaves a large write-ahead log behind.
		if err := db.Checkpoint(); err != nil {
			return err
		}
	}
	if err := db.Optimize(); err != nil {
		return err
	}

	fmt.Printf("indexed %d sessions (%d chunks, %d messages archived) in %s\n",
		p.Indexed, p.Chunks, p.Messages, time.Since(start).Round(time.Millisecond))
	if embedder == nil {
		note("built without embeddings; search will be keyword-only")
	}

	if noTitles {
		return nil
	}
	return runTitles(ctx, db, titleLimit, titleVia)
}

// summarizer builds the title generator for the chosen backend.
func summarizer(via string) (titles.Summarizer, error) {
	return titles.NewSummarizer(via)
}

// runTitles generates titles for sessions that do not have one.
//
// Separate from the build and after it, so a search index exists whatever
// happens here. Missing configuration is a note rather than an error: titles
// are an improvement to the list, and an index without them is the index this
// tool had for its first five milestones.
func runTitles(ctx context.Context, db *index.DB, limit int, via string) error {
	s, err := summarizer(via)
	if err != nil {
		note("skipping titles: %v", err)
		return nil
	}

	start := time.Now()
	var lastReport time.Time
	p, err := titles.Run(ctx, db, titles.Options{
		Summarizer:  s,
		Limit:       limit,
		Concurrency: titles.ConcurrencyFor(s),
		OnProgress: func(p titles.Progress) {
			if p.Total == 0 {
				return
			}
			if !p.Finished && time.Since(lastReport) < 100*time.Millisecond {
				return
			}
			lastReport = time.Now()
			fmt.Printf("\r\033[K%d/%d  titled %d  unchanged %d  failed %d",
				p.Done, p.Total, p.Titled, p.Cached, p.Failed)
		},
	})
	if p.Total > 0 {
		fmt.Println()
	}
	if err != nil {
		return err
	}
	if p.Titled > 0 {
		fmt.Printf("titled %d sessions with %s in %s\n",
			p.Titled, s.Name(), time.Since(start).Round(time.Millisecond))
	}
	if p.Failed > 0 {
		// One reason, not p.Failed of them: when this goes wrong it is almost
		// always one thing wrong with the configuration.
		note("%d of %d sessions could not be summarized (%v); they keep their "+
			"opening message and will be retried", p.Failed, p.Total, p.FirstErr)
	}
	return nil
}

func runStats(dbPath string) error {
	// Prefer the semantic driver: chunks_vec is a vec0 virtual table, and
	// without the extension loaded it is not readable, so a plain reader would
	// report an index full of embeddings as having none.
	driver := index.DriverName
	if err := index.RegisterSemanticDriver(embed.DefaultPaths()); err == nil {
		driver = index.SemanticDriverName
	}
	db, err := index.OpenReader(dbPath, driver)
	if err != nil && driver != index.DriverName {
		db, err = index.OpenReader(dbPath, index.DriverName)
	}
	if err != nil {
		return err
	}
	defer db.Close()

	st, err := db.Stats()
	if err != nil {
		return err
	}
	fmt.Printf("sessions  %d\nchunks    %d\nprojects  %d\n", st.Sessions, st.Chunks, st.Projects)
	if msgs, err := db.CountMessages(context.Background()); err == nil {
		fmt.Printf("messages  %d\n", msgs)
	}
	if titled, err := db.CountTitledSessions(context.Background()); err == nil {
		fmt.Printf("titles    %d\n", titled)
	}
	if st.Vectors < 0 {
		fmt.Println("vectors   none (keyword search only)")
	} else {
		fmt.Printf("vectors   %d\n", st.Vectors)
	}
	if fi, err := os.Stat(dbPath); err == nil {
		fmt.Printf("size      %.1f MB\n", float64(fi.Size())/(1<<20))
	}
	return nil
}

// runDoctor checks the invariants that indexing can silently violate.
//
// chunks_fts is an external-content FTS5 table and chunks_vec is a virtual
// table, and neither participates in foreign-key cascades. Deleting a chunk
// without also deleting its index rows leaves search returning hits that point
// at rows which no longer exist. Nothing detects that at query time, so it has
// to be checked directly -- and it cannot be checked with the sqlite3 CLI,
// because chunks_vec is unreadable without the extension this binary links in.
func runDoctor(dbPath string) error {
	// The extension is needed to read chunks_vec at all, but an index built
	// without embeddings has no such table and is still worth checking.
	driver := index.DriverName
	if err := index.RegisterSemanticDriver(embed.DefaultPaths()); err == nil {
		driver = index.SemanticDriverName
	} else {
		note("extension unavailable, skipping vector checks: %v", err)
	}
	db, err := index.OpenReader(dbPath, driver)
	if err != nil {
		return err
	}
	defer db.Close()

	// Which tables exist at all. A lexical-only index has no vector table, and
	// an index that has not been rebuilt since the archive landed has no
	// messages table -- OpenReader creates nothing, by design.
	has := func(table string) bool {
		var n int
		_ = db.SQL().QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n)
		return n > 0
	}
	hasVec, hasMessages := has("chunks_vec"), has("messages")
	if !hasMessages {
		note("no message archive in this index; run 'spireweb index' to build one")
	}

	checks := []struct{ name, sql string }{
		{"vectors with no chunk", `SELECT COUNT(*) FROM chunks_vec v
			WHERE NOT EXISTS (SELECT 1 FROM chunks c WHERE c.id = v.rowid)`},
		{"chunks with no vector", `SELECT COUNT(*) FROM chunks c
			WHERE NOT EXISTS (SELECT 1 FROM chunks_vec v WHERE v.rowid = c.id)`},
		{"chunks with no session", `SELECT COUNT(*) FROM chunks c
			WHERE NOT EXISTS (SELECT 1 FROM sessions s WHERE s.id = c.session_id)`},
		{"fts rows minus chunks", `SELECT ABS((SELECT COUNT(*) FROM chunks_fts) -
			(SELECT COUNT(*) FROM chunks))`},
		// The archive is the part that is not derived from anything else, so a
		// session missing from it is the one inconsistency that cannot be
		// repaired by reindexing alone -- if the file is gone too, it is gone.
		{"sessions not archived", `SELECT COUNT(*) FROM sessions s
			WHERE s.n_msgs > 0
			  AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.session_id = s.id)`},
		{"messages minus n_msgs", `SELECT COUNT(*) FROM sessions s
			WHERE s.n_msgs <> (SELECT COUNT(*) FROM messages m WHERE m.session_id = s.id)`},
		{"messages with no session", `SELECT COUNT(*) FROM messages m
			WHERE NOT EXISTS (SELECT 1 FROM sessions s WHERE s.id = m.session_id)`},
		{"messages that are not json", `SELECT COUNT(*) FROM messages
			WHERE json_valid(content) = 0`},
	}

	bad := 0
	for _, c := range checks {
		switch {
		case !hasVec && strings.Contains(c.sql, "chunks_vec"):
			fmt.Printf("%-26s %7s  skipped (no embeddings)\n", c.name, "-")
			continue
		case !hasMessages && strings.Contains(c.sql, "messages"):
			fmt.Printf("%-26s %7s  skipped (no archive)\n", c.name, "-")
			continue
		}
		var n int
		if err := db.SQL().QueryRow(c.sql).Scan(&n); err != nil {
			return fmt.Errorf("%s: %w", c.name, err)
		}
		status := "ok"
		if n != 0 {
			status = "FAIL"
			bad++
		}
		fmt.Printf("%-26s %7d  %s\n", c.name, n, status)
	}
	if bad > 0 {
		return fmt.Errorf("%d checks failed; rebuild with 'spireweb index --full'", bad)
	}
	return nil
}

func runInfo(dbPath, dir string) error {
	paths := embed.DefaultPaths()
	fmt.Printf("spireweb %s (%s %s/%s)\n\n", Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	fmt.Printf("sessions   %s\nindex      %s\nextension  %s\nmodel      %s\n\n",
		dir, dbPath, paths.Extension, paths.Model)

	if err := index.CheckFTS5(); err != nil {
		fmt.Println("fts5       missing:", err)
	} else {
		fmt.Println("fts5       available")
	}
	if err := paths.Check(); err != nil {
		fmt.Println("semantic   unavailable:", err)
		return nil
	}
	if err := index.RegisterSemanticDriver(paths); err != nil {
		fmt.Println("semantic   unavailable:", err)
		return nil
	}
	// Opening is the only honest test: the files can be present and the backend
	// still unable to load the model.
	probe, err := index.Open(dbPath, index.SemanticDriverName)
	if err != nil {
		fmt.Println("semantic   unavailable:", err)
		return nil
	}
	probe.Close()
	fmt.Printf("semantic   available (%s, %d dims)\n", embed.ModelName, embed.Dim)
	return nil
}

func note(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "note: "+format+"\n", args...)
}
