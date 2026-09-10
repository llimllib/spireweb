// Package embed generates text embeddings locally.
//
// Embeddings come from all-MiniLM-L6-v2 (384 dimensions) run through
// sqlite-lembed, a SQLite extension wrapping llama.cpp. Everything happens
// in-process: no API key, no network, no subprocess.
//
// Why this backend, from measurements taken during design:
//
//   - Apple's NLContextualEmbedding ships with macOS and needs no download, but
//     it is a general-purpose feature extractor rather than a retrieval model.
//     On a paraphrase suite it ranked the intended document first 2 times in 4,
//     with an average top1-top2 margin of 0.008 -- indistinguishable from noise.
//     MiniLM scored 3/4 with a 0.224 margin, 27x wider.
//
//   - sqlite-lembed produces vectors matching the llama.cpp CLI at cosine
//     0.999854, so an index built with either is valid for the other. The CLI
//     remains a viable fallback if this extension ever stops loading.
//
// This expects the landrix fork of sqlite-lembed, built from source (see
// 'mise run setup'). The upstream v0.0.1-alpha.8 prebuilt binary has two crash bugs
// that kill the host process rather than returning errors:
//
//   - Input over 512 tokens overflows a fixed-size batch (SIGSEGV). The fork
//     bound-checks and returns "Input too large: N tokens exceeds model context
//     size of 512" instead.
//   - Invalid UTF-8 raised an uncaught C++ std::invalid_argument (SIGABRT). The
//     fork's newer llama.cpp handles malformed input without throwing.
//
// The fork also enables Metal, which upstream's prebuilt binary lacked.
package embed

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/mattn/go-sqlite3"

	"github.com/llimllib/spireweb/internal/session"
)

// ModelName identifies the embedding model in index metadata. Changing the
// model must change this string, so that an index built with the old one is
// detected rather than silently producing meaningless comparisons.
const ModelName = "all-MiniLM-L6-v2.Q8_0"

// Dim is the embedding dimensionality of all-MiniLM-L6-v2.
const Dim = 384

// MinLembedVersion is the version prefix expected from the landrix fork.
//
// Upstream v0.0.1-alpha.8 reports "v0.0.1-alpha.8" and crashes the process on
// over-length or malformed input. The fork reports "v0.0.1-alpha.8-landrix.N".
// Checking at startup turns "why did my index build die with no error" into a
// clear message.
const forkVersionMarker = "landrix"

// registryName is how the model is referred to inside SQL: lembed('minilm', ...).
const registryName = "minilm"

// registerSQL loads a GGUF model into lembed's per-connection registry.
const registerSQL = `INSERT INTO temp.lembed_models(name, model)
	 SELECT ?, lembed_model_from_file(?)`

// ConnectHook returns a go-sqlite3 connect hook that registers the model on
// every new connection.
//
// lembed's model registry lives in temp.lembed_models, which is per-connection,
// so a connection that database/sql opened after the pool was set up would
// otherwise have no model. That failure is silent: semantic queries error, the
// ranker is skipped, and search quietly degrades to lexical-only.
func ConnectHook(modelPath string) func(*sqlite3.SQLiteConn) error {
	return func(conn *sqlite3.SQLiteConn) error {
		// Registering a model writes a row, and read-only connections set
		// query_only, which forbids writes to *every* attached database --
		// including temp, where the registry lives. So the reader pool would
		// fail here with "attempt to write a readonly database" and lose
		// semantic search entirely. Lift the restriction for the registration
		// and put it straight back.
		restricted, err := queryOnly(conn)
		if err != nil {
			return err
		}
		if restricted {
			if err := exec(conn, `PRAGMA query_only=OFF`); err != nil {
				return err
			}
			defer func() { _ = exec(conn, `PRAGMA query_only=ON`) }()
		}

		_, err = conn.Exec(registerSQL, []driver.Value{registryName, modelPath})
		if err != nil && !strings.Contains(err.Error(), "already exists") {
			return registerError(modelPath, err)
		}
		return nil
	}
}

func exec(conn *sqlite3.SQLiteConn, q string) error {
	_, err := conn.Exec(q, nil)
	return err
}

// queryOnly reports whether this connection has query_only set.
func queryOnly(conn *sqlite3.SQLiteConn) (bool, error) {
	rows, err := conn.Query(`PRAGMA query_only`, nil)
	if err != nil {
		return false, err
	}
	defer rows.Close()

	vals := make([]driver.Value, 1)
	if err := rows.Next(vals); err != nil {
		return false, err
	}
	n, _ := vals[0].(int64)
	return n != 0, nil
}

// registerError explains a failed model registration.
//
// lembed_model_from_file returns a null pointer when llama.cpp cannot load the
// model, and the virtual table rejects it with a bare "SQL logic error" and no
// message. The overwhelmingly common cause is not a bad file: it is llama.cpp
// failing to initialize because no GPU is reachable, which is what happens on a
// headless CI runner or inside a sandbox. The extension loads and answers
// lembed_version() either way, so this is the first point where the difference
// is visible.
//
// Callers must treat this as fatal for the connection and fall back to lexical
// search. Continuing is not an option: lembed() against an unregistered model
// does not return an error, it dereferences the null model and segfaults the
// process, which cannot be recovered from inside cgo.
func registerError(modelPath string, err error) error {
	if strings.Contains(err.Error(), "SQL logic error") {
		return fmt.Errorf("%w: llama.cpp could not load %s"+
			" (on macOS this usually means no Metal device is available;"+
			" otherwise the model file may be corrupt -- delete it and run 'mise run setup')",
			ErrUnavailable, modelPath)
	}
	return fmt.Errorf("%w: registering model %s: %v", ErrUnavailable, modelPath, err)
}

// Paths locates the native extension and model file.
type Paths struct {
	Extension string // lembed0.dylib (or .so)
	Model     string // *.gguf
}

// DefaultPaths returns the standard install location.
func DefaultPaths() Paths {
	dir := defaultDir()
	return Paths{
		Extension: filepath.Join(dir, extensionFile()),
		Model:     filepath.Join(dir, "all-MiniLM-L6-v2.Q8_0.gguf"),
	}
}

func defaultDir() string {
	if d := os.Getenv("SPIREWEB_DATA_DIR"); d != "" {
		return d
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "share", "spireweb")
	}
	return "."
}

// ErrUnavailable indicates the extension or model is not installed. Callers
// should fall back to lexical-only search rather than failing outright: a
// keyword search is still useful, and telling the user what to install is more
// helpful than refusing to start.
var ErrUnavailable = errors.New("semantic search unavailable")

// Check reports whether the extension and model are present and readable.
func (p Paths) Check() error {
	for _, f := range []struct{ kind, path string }{
		{"extension", p.Extension},
		{"model", p.Model},
	} {
		if f.path == "" {
			return fmt.Errorf("%w: no %s path configured", ErrUnavailable, f.kind)
		}
		if _, err := os.Stat(f.path); err != nil {
			return fmt.Errorf("%w: %s not found at %s (run 'mise run setup')", ErrUnavailable, f.kind, f.path)
		}
	}
	return nil
}

// Embedder embeds text using a lembed-enabled database connection.
//
// The connection must have been opened with a driver that loads the lembed
// extension; see index.RegisterDriver. Embedder registers the model on first
// use, since lembed keeps its model registry in a temp table scoped to the
// connection.
type Embedder struct {
	db         *sql.DB
	modelPath  string
	registered bool
	version    string
}

// New prepares an Embedder against an already-open database.
func New(db *sql.DB, p Paths) (*Embedder, error) {
	if err := p.Check(); err != nil {
		return nil, err
	}
	e := &Embedder{db: db, modelPath: p.Model}
	if err := e.register(context.Background()); err != nil {
		return nil, err
	}
	return e, nil
}

// register loads the GGUF model into lembed's per-connection registry.
func (e *Embedder) register(ctx context.Context) error {
	if e.registered {
		return nil
	}
	var version string
	if err := e.db.QueryRowContext(ctx, `SELECT lembed_version()`).Scan(&version); err != nil {
		return fmt.Errorf("%w: lembed extension did not load: %v", ErrUnavailable, err)
	}
	e.version = version

	// The driver's ConnectHook registers the model on every connection, so
	// normally there is nothing to do here. Checking first matters because the
	// reader pool sets query_only, which refuses the INSERT below outright --
	// registering is a write, even into temp.
	//
	// Reading the registry is per-connection and the pool may answer from a
	// different connection than a later query uses. That is fine precisely
	// because the hook registers on all of them: the question being asked is
	// "did the hook run", not "is this particular connection ready".
	var n int
	if err := e.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM temp.lembed_models WHERE name = ?`, registryName).Scan(&n); err == nil && n > 0 {
		e.registered = true
		return nil
	}

	// No hook: a connection opened with a plain driver, as tests do.
	_, err := e.db.ExecContext(ctx, registerSQL, registryName, e.modelPath)
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			e.registered = true
			return nil
		}
		return fmt.Errorf("%w: registering model %s: %v", ErrUnavailable, e.modelPath, err)
	}
	e.registered = true
	return nil
}

// Version reports the loaded extension version.
func (e *Embedder) Version() string { return e.version }

// IsPatchedFork reports whether the loaded extension is the landrix fork, which
// returns errors where upstream crashed the process.
func (e *Embedder) IsPatchedFork() bool {
	return strings.Contains(e.version, forkVersionMarker)
}

// Name identifies the model for index metadata.
func (e *Embedder) Name() string { return ModelName }

// Dim returns the embedding dimensionality.
func (e *Embedder) Dim() int { return Dim }

// sanitize makes text safe to hand to sqlite-lembed.
//
// The landrix fork tolerates malformed UTF-8, but this stays as defense in
// depth: upstream's build aborted the process on it, a crash inside cgo cannot
// be recovered, and the cost is one validity check. Chunk already splits only on
// rune boundaries, so this should never fire.
func sanitize(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "\uFFFD")
}

// querier abstracts *sql.DB and *sql.Tx.
type querier interface {
	QueryRow(query string, args ...any) *sql.Row
}

// CountTokensOn reports how many tokens text becomes under the model's
// tokenizer, using the given querier.
//
// Counting before embedding keeps over-length input out of the model. The
// landrix fork returns a clean SQL error for such input, so this is no longer
// load-bearing for crash safety -- but it is still what lets indexing split a
// long chunk and keep both halves, rather than failing the row and losing the
// text entirely.
//
// lembed_tokenize_json returns a JSON array of token ids. It was safe on
// over-length input even in upstream's crashing build.
//
// The querier matters. Running this on the pooled *sql.DB while a write
// transaction is open on the same database deadlocks: the new connection waits
// for a lock the transaction holds and neither side yields. Code inside a
// transaction must pass the *sql.Tx.
func (e *Embedder) CountTokensOn(q querier, text string) (int, error) {
	if text == "" {
		return 0, nil
	}
	var js string
	err := q.QueryRow(`SELECT lembed_tokenize_json('`+registryName+`', ?)`, sanitize(text)).Scan(&js)
	if err != nil {
		return 0, fmt.Errorf("tokenize: %w", err)
	}
	var toks []int
	if err := json.Unmarshal([]byte(js), &toks); err != nil {
		return 0, fmt.Errorf("tokenize: parsing %q: %w", truncErr(js), err)
	}
	return len(toks), nil
}

// CountTokens counts tokens on the pooled connection. Safe only outside a write
// transaction; during indexing use TokenCounterFor(tx).
func (e *Embedder) CountTokens(text string) (int, error) {
	return e.CountTokensOn(e.db, text)
}

// txCounter adapts a transaction to session.TokenCounter.
type txCounter struct {
	e  *Embedder
	tx *sql.Tx
}

func (t txCounter) CountTokens(text string) (int, error) {
	return t.e.CountTokensOn(t.tx, text)
}

// TokenCounterFor returns a session.TokenCounter bound to tx, so token counting
// runs on the same connection as the indexing transaction and cannot deadlock
// against it.
func (e *Embedder) TokenCounterFor(tx *sql.Tx) session.TokenCounter {
	return txCounter{e: e, tx: tx}
}

// MaxTokens is the model's context limit.
func (e *Embedder) MaxTokens() int { return session.MaxTokens }

func truncErr(s string) string {
	if len(s) > 60 {
		return s[:60] + "..."
	}
	return s
}

// EmbedInto computes and stores vectors for the given chunk rowids.
//
// Embedding happens entirely inside SQLite: the vector goes straight from
// lembed into the sqlite-vec table without crossing into Go. This is both
// faster and less error-prone than marshalling float slices back and forth.
//
// Called within the indexing transaction, so a failure rolls back the chunks
// alongside their vectors and cannot leave a session half-embedded.
func (e *Embedder) EmbedInto(ctx context.Context, tx *sql.Tx, chunkIDs []int64) error {
	if len(chunkIDs) == 0 {
		return nil
	}
	// The body is read back from the chunks table rather than passed in, so the
	// vector is always derived from exactly the text that was stored.
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO chunks_vec(rowid, embedding)
		SELECT id, lembed('`+registryName+`', body) FROM chunks WHERE id = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, id := range chunkIDs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := stmt.ExecContext(ctx, id); err != nil {
			return fmt.Errorf("embedding chunk %d: %w", id, err)
		}
	}
	return nil
}

// EmbedText returns the vector for a single string, as the serialized blob
// sqlite-vec expects. Used for queries, where the vector is compared against
// the index rather than stored in it.
//
// Over-length queries are truncated rather than rejected: a query long enough
// to exceed 512 tokens is unusual, and silently using its first 512 tokens is
// friendlier than an error, where the alternative is a segfault.
func (e *Embedder) EmbedText(ctx context.Context, text string) ([]byte, error) {
	if err := e.register(ctx); err != nil {
		return nil, err
	}

	n, err := e.CountTokens(text)
	if err != nil {
		return nil, err
	}
	if n > session.MaxTokens {
		if pieces := session.Chunk(text, session.SafeChunkChars); len(pieces) > 0 {
			text = pieces[0]
		}
	}

	var blob []byte
	err = e.db.QueryRowContext(ctx,
		`SELECT lembed('`+registryName+`', ?)`, sanitize(text)).Scan(&blob)
	if err != nil {
		return nil, fmt.Errorf("embedding query text: %w", err)
	}
	if len(blob) != Dim*4 {
		return nil, fmt.Errorf("expected %d bytes (%d float32), got %d", Dim*4, Dim, len(blob))
	}
	return blob, nil
}
