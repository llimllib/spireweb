package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/llimllib/spireweb/internal/config"
	"github.com/llimllib/spireweb/internal/embed"
	"github.com/llimllib/spireweb/internal/index"
	"github.com/llimllib/spireweb/internal/indexer"
	"github.com/llimllib/spireweb/internal/search"
	"github.com/llimllib/spireweb/internal/titles"
	"github.com/llimllib/spireweb/internal/web"
)

func runServe(dbPath, addr string, dirs []string, dev, launchBrowser, noWatch bool, titlesVia string) error {
	if err := bootstrapIndex(dbPath); err != nil {
		return err
	}

	// The server only reads. Semantic support is preferred because chunks_vec
	// is unreadable without the extension, but browsing works without it, so
	// a machine that cannot load the model still gets a usable interface.
	driver := index.DriverName
	if err := index.RegisterSemanticDriver(embed.DefaultPaths()); err == nil {
		driver = index.SemanticDriverName
	}
	stop := noteSlowModelLoad()
	db, err := index.OpenReader(dbPath, driver)
	stop()
	if err != nil && driver != index.DriverName {
		note("semantic search unavailable: %v", err)
		driver = index.DriverName
		db, err = index.OpenReader(dbPath, driver)
	}
	if err != nil {
		return err
	}
	defer db.Close()

	n, err := db.CountSessions(context.Background())
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A writer, separate from the read pool above. WAL lets handlers read a
	// consistent snapshot while this one indexes, so a reindex triggered by a
	// conversation in another terminal never blocks a request.
	live, closeLive := startIndexer(ctx, dbPath, driver, dirs, noWatch, titlesVia)
	if closeLive != nil {
		defer closeLive()
	}

	srv, err := web.New(db, buildEngine(db, driver), web.Options{Dev: dev, Indexer: live})
	if err != nil {
		return err
	}

	// Listen before announcing, so the printed URL is always one that works
	// and --open cannot race the socket.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	url := "http://" + ln.Addr().String()
	// The subcommands are not visible on a bare `spireweb`, which is now the
	// usual way to run it. One line restores them without anyone reading usage
	// they did not ask for.
	fmt.Printf("spireweb %s serving %d sessions on %s\n", Version, n, url)
	fmt.Println("'spireweb help' lists the other commands")
	if dev {
		fmt.Println("dev mode: templates and static files reload from disk")
	}
	if launchBrowser {
		go open(url)
	}

	httpSrv := srv.Server(addr)
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.Serve(ln) }()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		fmt.Println("\nshutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	}
}

// bootstrapIndex creates an empty index when there is none.
//
// The read pool below is _query_only, which cannot create a schema, so
// something has to have made one first. That used to be an error telling the
// reader to go and run 'spireweb index' -- accurate, and backwards: serve opens
// a writer a few lines further down and runs a catch-up build through it, so it
// already does the thing it was refusing to start without. The only reason it
// could not bootstrap itself was the order the two handles were opened in.
//
// Creating it here rather than reordering so that startIndexer runs first:
// --no-watch makes that function return immediately, and a scripted run against
// a fresh index is exactly where that path is used.
//
// The lexical driver, whatever the server will use: this needs the schema and
// nothing else, and opening a semantic connection would load a second copy of
// the model -- 30MB, and fifteen seconds on a cold shader cache -- to run a few
// CREATE TABLEs.
func bootstrapIndex(dbPath string) error {
	if _, err := os.Stat(dbPath); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	db, err := index.Open(dbPath, index.DriverName)
	if err != nil {
		return err
	}
	// Said rather than done silently: the first run comes up with an empty list
	// and fills in behind the reader, and "no sessions" is alarming without a
	// reason for it. The progress itself reaches the page through /status.
	note("no index at %s; creating one and indexing in the background", dbPath)
	return db.Close()
}

// startIndexer opens a writer and keeps the index current in the background.
//
// Failure here is not fatal. The server's job is to show what is already
// indexed, and a second spireweb holding the write lock, or a read-only
// filesystem, should cost live updates rather than the whole interface.
func startIndexer(ctx context.Context, dbPath, driver string, dirs []string, noWatch bool, titlesVia string) (web.StatusSource, func()) {
	if noWatch {
		return nil, nil
	}

	writer, err := index.Open(dbPath, driver)
	if err != nil {
		note("live indexing disabled: %v", err)
		return nil, nil
	}

	// Said before the build rather than after, because the catch-up starts
	// immediately and nothing else reports the cost. On this corpus it is the
	// difference between a 107MB index and a 502MB one, arriving in about
	// fifteen seconds of starting the server.
	if need, err := writer.NeedsArchive(); err == nil && need {
		note("this index predates the message archive; the catch-up build will store " +
			"every message, which grows the database several times over")
	}

	opts := index.BuildOptions{Dirs: dirs}
	if driver == index.SemanticDriverName {
		if e, err := embed.New(writer.SQL(), embed.DefaultPaths()); err != nil {
			note("new sessions will be indexed without embeddings: %v", err)
		} else if err := writer.EnsureVectorTable(e.Dim()); err != nil {
			// An index built by 'spireweb index' already has this table, which
			// is why nothing missed it -- but serve can now be the first thing
			// to touch a new index, and then there is nowhere to put a vector.
			note("new sessions will be indexed without embeddings: %v", err)
		} else {
			opts.Embedder = e
		}
	}

	// Titles fill in behind the list while it is being browsed, which is the
	// point of them being a separate pass: nothing waits on a network call.
	var titleOpts titles.Options
	if titlesVia != config.TitlesOff {
		if s, err := summarizer(titlesVia); err != nil {
			note("titles disabled: %v", err)
		} else {
			titleOpts.Summarizer = s
			titleOpts.Concurrency = titles.ConcurrencyFor(s)
		}
	}

	ix := indexer.New(writer, opts, titleOpts)
	go func() {
		if err := ix.Run(ctx, true); err != nil && ctx.Err() == nil {
			note("indexer stopped: %v", err)
		}
	}()

	return ix, func() { writer.Close() }
}

// buildEngine assembles the rankers available against this index.
//
// Lexical always works; semantic needs the extension, a model that loads, and
// an index that actually has vectors. An engine with only the lexical ranker
// is a normal outcome rather than a degraded one worth hiding: FTS5 alone
// still finds identifiers, error strings, and filenames, which is a large
// part of what searching your own sessions is for.
func buildEngine(db *index.DB, driver string) *search.Engine {
	rankers := []search.Ranker{&search.Lexical{DB: db.SQL()}}

	if driver == index.SemanticDriverName {
		if e, err := embed.New(db.SQL(), embed.DefaultPaths()); err != nil {
			note("semantic search unavailable: %v", err)
		} else {
			// Attached whether or not the index currently has vectors. The
			// background indexer adds them while the server runs, and gating
			// on the count here would leave a process that started against an
			// empty index searching by keyword until it was restarted. With no
			// vectors the KNN query simply returns nothing and fusion falls
			// back to lexical on its own.
			rankers = append(rankers, &search.Semantic{DB: db.SQL(), Embedder: e})

			if has, err := db.HasVectors(); err == nil && !has {
				note("index has no embeddings yet; results will be keyword-only " +
					"until indexing adds them")
			}
		}
	}

	return &search.Engine{Rankers: rankers, Fusion: search.DefaultFusion()}
}

func open(url string) {
	var cmd string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "windows":
		cmd = "explorer"
	default:
		cmd = "xdg-open"
	}
	_ = exec.Command(cmd, url).Start()
}
