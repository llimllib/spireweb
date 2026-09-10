package index

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/llimllib/spireweb/internal/embed"
)

// Live indexing embeds new chunks on the writer while search requests embed
// queries on the reader pool, so embedding runs concurrently on several
// connections at once. That is a different question from the one the writer's
// single-connection rule answers, and it decides whether a process-wide lock
// around embedding is required.
//
// If this ever fails it will not fail as a test failure: a crash inside cgo
// takes the whole process down, so a panic-free run is the assertion.
func TestConcurrentEmbeddingAcrossConnections(t *testing.T) {
	p := semanticPaths(t)

	path := filepath.Join(t.TempDir(), "i.db")
	writer, err := Open(path, SemanticDriverName)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	reader, err := OpenReader(path, SemanticDriverName)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	writerEmbed, err := embed.New(writer.SQL(), p)
	if err != nil {
		t.Fatal(err)
	}
	readerEmbed, err := embed.New(reader.SQL(), p)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 64)

	// Several readers, as concurrent search requests would be, plus a writer
	// embedding continuously, as the indexer would be.
	for i := range 6 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for range 12 {
				if _, err := readerEmbed.EmbedText(ctx, "a query about databases"); err != nil {
					errs <- err
					return
				}
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 12 {
			if _, err := writerEmbed.EmbedText(ctx, "some indexed prose"); err != nil {
				errs <- err
				return
			}
		}
	}()

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent embedding: %v", err)
	}
}
