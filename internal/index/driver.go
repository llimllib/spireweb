package index

// Driver registration.
//
// Two build requirements are enforced here rather than left to a developer's
// shell history:
//
//   - -tags sqlite_fts5: mattn/go-sqlite3 omits FTS5 by default, and without it
//     the schema fails at runtime with "no such module: fts5". mise sets this in
//     GOFLAGS.
//   - sqlite-vec is linked in via the Go bindings; sqlite-lembed is loaded at
//     runtime from a dylib, because it is distributed as a native library.

import (
	"database/sql"
	"fmt"
	"sync"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	"github.com/mattn/go-sqlite3"

	"github.com/llimllib/spireweb/internal/embed"
)

// DriverName is the driver for lexical-only use.
const DriverName = "sqlite3"

// SemanticDriverName is the driver with sqlite-vec and sqlite-lembed attached.
const SemanticDriverName = "sqlite3_semantic"

var (
	regOnce sync.Once
	regErr  error
)

// RegisterSemanticDriver registers a driver that loads sqlite-lembed, with
// sqlite-vec linked in, and registers the embedding model on every connection
// it opens.
//
// Per-connection model registration is the important part, and it is easy to
// get wrong. sqlite-lembed keeps its model registry in temp.lembed_models,
// which is scoped to a connection. database/sql opens and closes pooled
// connections whenever it likes, so registering the model once after opening a
// pool leaves later connections without one -- and the symptom is not an error.
// Semantic queries on an unregistered connection simply fail, the ranker is
// skipped, and search silently degrades to lexical-only. A ConnectHook is the
// only place that can see every connection.
//
// Registration is process-global and can only happen once, hence sync.Once:
// database/sql panics on a duplicate driver name. Callers may invoke this more
// than once with the same paths; the first call wins.
func RegisterSemanticDriver(p embed.Paths) error {
	regOnce.Do(func() {
		if err := p.Check(); err != nil {
			regErr = err
			return
		}

		// sqlite-vec registers itself as an auto-extension on every new
		// connection. macOS deprecates process-global auto extensions, but the
		// mechanism still works and is what the bindings provide.
		sqlite_vec.Auto()

		sql.Register(SemanticDriverName, &sqlite3.SQLiteDriver{
			Extensions:  []string{p.Extension},
			ConnectHook: embed.ConnectHook(p.Model),
		})
	})
	return regErr
}

// CheckFTS5 verifies the binary was built with FTS5 support, so that a missing
// build tag is reported clearly at startup instead of as a schema error.
func CheckFTS5() error {
	db, err := sql.Open(DriverName, ":memory:")
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE VIRTUAL TABLE t USING fts5(x)`); err != nil {
		return fmt.Errorf("this binary lacks FTS5 support (build with -tags sqlite_fts5): %w", err)
	}
	return nil
}
