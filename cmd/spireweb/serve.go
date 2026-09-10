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

	"github.com/llimllib/spireweb/internal/embed"
	"github.com/llimllib/spireweb/internal/index"
	"github.com/llimllib/spireweb/internal/search"
	"github.com/llimllib/spireweb/internal/web"
)

func runServe(dbPath, addr string, dev, launchBrowser bool) error {
	if _, err := os.Stat(dbPath); err != nil {
		return fmt.Errorf("no index at %s; run 'spireweb index' first", dbPath)
	}

	// The server only reads. Semantic support is preferred because chunks_vec
	// is unreadable without the extension, but browsing works without it, so
	// a machine that cannot load the model still gets a usable interface.
	driver := index.DriverName
	if err := index.RegisterSemanticDriver(embed.DefaultPaths()); err == nil {
		driver = index.SemanticDriverName
	}
	db, err := index.OpenReader(dbPath, driver)
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

	srv, err := web.New(db, buildEngine(db, driver), web.Options{Dev: dev})
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
	fmt.Printf("spireweb %s serving %d sessions on %s\n", Version, n, url)
	if dev {
		fmt.Println("dev mode: templates and static files reload from disk")
	}
	if launchBrowser {
		go open(url)
	}

	httpSrv := srv.Server(addr)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
		hasVectors, err := db.HasVectors()
		if err != nil || !hasVectors {
			note("index has no embeddings; searching by keyword only " +
				"(run 'spireweb index' to add them)")
		} else if e, err := embed.New(db.SQL(), embed.DefaultPaths()); err != nil {
			note("semantic search unavailable: %v", err)
		} else {
			rankers = append(rankers, &search.Semantic{DB: db.SQL(), Embedder: e})
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
