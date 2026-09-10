// Command spireweb indexes and browses pi agent sessions.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/llimllib/spireweb/internal/embed"
	"github.com/llimllib/spireweb/internal/index"
	"github.com/llimllib/spireweb/internal/session"
)

// Version is stamped in at build time by `mise run build`.
var Version = "dev"

const usage = `spireweb - search and read pi agent sessions

usage:
  spireweb index [flags]   build or update the search index
  spireweb stats [flags]   report what is in the index
  spireweb info            show paths and configuration
  spireweb version

flags:
  --db PATH      index location (default %s)
  --dir PATH     session directory (default %s)
  --full         reindex everything rather than what changed
  --lexical      skip semantic indexing, even if the model is installed
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
	fs.Usage = func() { fmt.Fprintf(os.Stderr, usage, index.DefaultPath(), session.DefaultDir()) }

	var err error
	switch cmd {
	case "index":
		_ = fs.Parse(os.Args[2:])
		err = runIndex(*dbPath, *dir, *full, *lexical)
	case "stats":
		_ = fs.Parse(os.Args[2:])
		err = runStats(*dbPath)
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

func runIndex(dbPath, dir string, full, lexical bool) error {
	db, embedder, err := openIndex(dbPath, lexical)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	start := time.Now()
	var lastReport time.Time
	p, err := index.Build(ctx, db, index.BuildOptions{
		Dir:      dir,
		Full:     full,
		Embedder: embedder,
		OnProgress: func(p index.Progress) {
			// Rate-limited because a skipped file takes microseconds and
			// printing every one of them costs more than the indexing does.
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

	fmt.Printf("indexed %d sessions (%d chunks) in %s\n",
		p.Indexed, p.Chunks, time.Since(start).Round(time.Millisecond))
	if embedder == nil {
		note("built without embeddings; search will be keyword-only")
	}
	return nil
}

func runStats(dbPath string) error {
	db, err := index.OpenReader(dbPath, index.DriverName)
	if err != nil {
		return err
	}
	defer db.Close()

	st, err := db.Stats()
	if err != nil {
		return err
	}
	fmt.Printf("sessions  %d\nchunks    %d\nprojects  %d\n", st.Sessions, st.Chunks, st.Projects)
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
