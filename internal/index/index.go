// Package index builds and queries the SQLite search index.
//
// One file holds everything: session metadata, chunk text, an FTS5 index for
// lexical search, and (added in a later milestone) a sqlite-vec table for
// semantic search. Keeping them in one database means a chunk's text, its
// keyword index, and its vector cannot drift apart.
package index

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/llimllib/spireweb/internal/session"
)

// SchemaVersion is bumped whenever the layout changes in a way that makes an
// existing index unusable. Open refuses to use a database written by a
// different version rather than failing in confusing ways later.
const SchemaVersion = 1

// Meta keys. The embedding model and chunk size are recorded because an index
// built with different values is silently wrong: vectors from two models are
// not comparable, and changing chunk size changes what a match means.
const (
	MetaSchemaVersion = "schema_version"
	MetaChunkChars    = "chunk_chars"
	MetaEmbedModel    = "embed_model"
	MetaEmbedDim      = "embed_dim"
)

// The sessions table is designed to survive being merged with another
// machine's index (see the Archive milestone), which is why identity lives in
// id -- a UUID minted by pi -- while path and host are machine-local metadata.
// Anything that treats a local path as identity has to be undone later, across
// every machine, so it is worth avoiding now.
const schema = `
CREATE TABLE IF NOT EXISTS sessions (
  id          TEXT PRIMARY KEY,
  path        TEXT NOT NULL UNIQUE,
  host        TEXT NOT NULL,
  cwd         TEXT NOT NULL,
  project     TEXT NOT NULL,
  started_at  TEXT NOT NULL,
  mtime       INTEGER NOT NULL,
  size        INTEGER NOT NULL,
  n_msgs      INTEGER NOT NULL,
  preview     TEXT NOT NULL,
  reply       TEXT NOT NULL DEFAULT '',

  -- Generated from the conversation by an LLM at index time. NULL until that
  -- pass runs (or when it fails), in which case the UI falls back to preview,
  -- so a missing title is never a blank row.
  title       TEXT
);
CREATE INDEX IF NOT EXISTS sessions_project ON sessions(project);
CREATE INDEX IF NOT EXISTS sessions_started ON sessions(started_at);

-- AUTOINCREMENT is required, not cosmetic. A plain INTEGER PRIMARY KEY reuses
-- rowids freed by DELETE, and chunks_vec is keyed by chunk id. Incremental
-- reindexing keeps unchanged chunks and inserts only new ones, so a recycled id
-- would collide with a surviving vector: "UNIQUE constraint failed on
-- chunks_vec primary key". AUTOINCREMENT guarantees ids are never reused.
CREATE TABLE IF NOT EXISTS chunks (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  msg_idx     INTEGER NOT NULL,
  role        TEXT NOT NULL,
  body        TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS chunks_session ON chunks(session_id);

-- External-content FTS5: indexes chunks.body without storing a second copy.
-- porter stemming matches search/searching/searches. It will never match
-- "query" to "search"; that is what the semantic ranker is for.
CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(
  body,
  content='chunks',
  content_rowid='id',
  tokenize='porter unicode61'
);

CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
`

// DB is an open search index.
type DB struct {
	sql  *sql.DB
	path string
}

// DefaultPath is where the index lives. It is a cache: everything in it is
// derived from the session files and can be rebuilt.
func DefaultPath() string {
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "spireweb", "index.db")
	}
	return "spireweb-index.db"
}

// pragmas are applied to every connection, writer and reader alike.
//
//   - WAL keeps reads working while indexing writes, so search works against a
//     partially built index.
//   - synchronous=NORMAL is safe under WAL and much faster. The worst case is
//     losing the last few transactions on power loss, and the index is derived
//     data that can always be rebuilt from the session files.
//   - busy_timeout avoids spurious SQLITE_BUSY between the writer and handlers.
//   - foreign_keys is load-bearing: chunks.session_id declares ON DELETE
//     CASCADE.
//   - journal_size_limit caps the WAL after a checkpoint, so a full rebuild
//     does not leave a permanently large file behind.
const pragmas = "_journal_mode=WAL" +
	"&_synchronous=NORMAL" +
	"&_busy_timeout=10000" +
	"&_foreign_keys=on" +
	"&_cache_size=-32000" +
	"&_mmap_size=268435456" +
	"&_temp_store=MEMORY" +
	"&_journal_size_limit=67108864"

// Open opens or creates the index at path for writing.
//
// The driver name is a parameter so that lexical-only use does not have to
// register the semantic driver, and so tests can run without the extension
// installed.
func Open(path, driver string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}

	// txlock=immediate takes the write lock when a transaction begins rather
	// than when it first writes. Without it, a transaction that reads before it
	// writes has to upgrade its lock, which fails outright with SQLITE_BUSY
	// rather than waiting -- and indexing reads reusable chunks before inserting.
	dsn := path + "?" + pragmas + "&_txlock=immediate"
	sdb, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, err
	}
	if err := sdb.Ping(); err != nil {
		sdb.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	// One connection, always.
	//
	// sqlite-lembed keeps its loaded model in temp.lembed_models, which is scoped
	// to a connection: a second pooled connection would have no model registered.
	// Worse, the underlying llama_context is not safe for concurrent use, and two
	// goroutines embedding at once (a background index build plus the file
	// watcher) segfaulted the process. Pinning to a single connection serializes
	// all access through SQLite's own locking.
	//
	// This costs little here: reads are microseconds and WAL still lets a reader
	// proceed while a writer holds a transaction on the *same* connection is not
	// possible -- so callers that must read during a long write use a separate
	// read-only DB handle (see OpenReader).
	sdb.SetMaxOpenConns(1)
	sdb.SetMaxIdleConns(1)
	sdb.SetConnMaxLifetime(0)

	db := &DB{sql: sdb, path: path}
	if err := db.init(); err != nil {
		sdb.Close()
		return nil, err
	}
	return db, nil
}

// ReaderConns is the size of the read-only pool. Handlers are short-lived and
// SQLite reads are microseconds; this is about not serializing concurrent
// requests behind one another, not about throughput.
const ReaderConns = 4

// OpenReader opens a pool of read-only handles to an existing index.
//
// A separate pool is needed because the writer is pinned to a single
// connection: while a build holds it, queries on that handle would queue behind
// the transaction. WAL lets these connections read a consistent snapshot while
// indexing runs, which is what makes searching during a build possible.
//
// query_only rather than mode=ro. They sound equivalent and are not: a
// genuinely read-only file handle cannot create the -shm and -wal files that
// WAL requires, so mode=ro fails against a database no writer currently has
// open. query_only refuses writes at the SQL level while leaving the connection
// able to participate in WAL.
//
// Connections never expire. The semantic driver's ConnectHook registers the
// embedding model on each new connection, so churn is survivable -- but a
// recycled connection also throws away SQLite's per-connection page cache, and
// there is nothing to gain here by allowing it.
func OpenReader(path, driver string) (*DB, error) {
	dsn := path + "?" + pragmas + "&_query_only=true"
	sdb, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, err
	}
	sdb.SetMaxOpenConns(ReaderConns)
	sdb.SetMaxIdleConns(ReaderConns)
	sdb.SetConnMaxLifetime(0)
	sdb.SetConnMaxIdleTime(0)
	if err := sdb.Ping(); err != nil {
		sdb.Close()
		return nil, fmt.Errorf("open reader %s: %w", path, err)
	}
	return &DB{sql: sdb, path: path}, nil
}

// Optimize runs SQLite's recommended maintenance for a long-lived process:
// refresh the query planner's statistics where they are stale. Cheap, and
// intended to be called periodically and at shutdown.
func (d *DB) Optimize() error {
	_, err := d.sql.Exec(`PRAGMA analysis_limit=400; PRAGMA optimize;`)
	return err
}

// Checkpoint truncates the WAL. Worth doing after a full rebuild, which can
// leave hundreds of megabytes of write-ahead log behind.
func (d *DB) Checkpoint() error {
	_, err := d.sql.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

func (d *DB) init() error {
	if _, err := d.sql.Exec(schema); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}

	got, err := d.Meta(MetaSchemaVersion)
	if err != nil {
		return err
	}
	switch got {
	case "":
		if err := d.SetMeta(MetaSchemaVersion, strconv.Itoa(SchemaVersion)); err != nil {
			return err
		}
		return d.SetMeta(MetaChunkChars, strconv.Itoa(session.MaxChunkChars))
	case strconv.Itoa(SchemaVersion):
		return nil
	default:
		return &StaleIndexError{Path: d.path, Found: got, Want: SchemaVersion}
	}
}

// StaleIndexError reports an index written by a different schema version.
//
// A distinct type rather than a plain error so callers can recover: the index is
// a derived cache that can always be rebuilt from the session files, so the right
// response is to discard and rebuild it, not to make the user run a command.
type StaleIndexError struct {
	Path  string
	Found string
	Want  int
}

func (e *StaleIndexError) Error() string {
	return fmt.Sprintf("index at %s has schema version %s, this build expects %d",
		e.Path, e.Found, e.Want)
}

// Reset deletes the index files and recreates an empty database.
//
// Everything in the index is derived from the session files, so discarding it
// loses nothing but the time to rebuild.
func Reset(path, driver string) (*DB, error) {
	// -wal and -shm must go too, or SQLite may recover the old content.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("removing stale index %s%s: %w", path, suffix, err)
		}
	}
	return Open(path, driver)
}

// OpenOrReset opens the index, rebuilding it from scratch if it was written by a
// different schema version. The bool reports whether a reset happened, so the
// caller can tell the user why indexing is about to run.
func OpenOrReset(path, driver string) (*DB, bool, error) {
	db, err := Open(path, driver)
	if err == nil {
		return db, false, nil
	}
	var stale *StaleIndexError
	if !errors.As(err, &stale) {
		return nil, false, err
	}
	db, err = Reset(path, driver)
	if err != nil {
		return nil, false, err
	}
	return db, true, nil
}

// Close closes the database.
func (d *DB) Close() error { return d.sql.Close() }

// SQL exposes the underlying handle for packages that need it.
func (d *DB) SQL() *sql.DB { return d.sql }

// Path returns the index file path.
func (d *DB) Path() string { return d.path }

// Meta reads a metadata value, returning "" when absent.
func (d *DB) Meta(key string) (string, error) {
	var v string
	err := d.sql.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// SetMeta writes a metadata value.
func (d *DB) SetMeta(key, value string) error {
	_, err := d.sql.Exec(
		`INSERT INTO meta(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// EnsureVectorTable creates the sqlite-vec table for embeddings.
//
// Separate from the base schema because vec0 is only available when the
// semantic driver is in use. A lexical-only build must not fail to open an
// index just because it cannot create this table.
func (d *DB) EnsureVectorTable(dim int) error {
	_, err := d.sql.Exec(fmt.Sprintf(
		`CREATE VIRTUAL TABLE IF NOT EXISTS chunks_vec USING vec0(embedding float[%d])`, dim))
	if err != nil {
		return fmt.Errorf("create vector table: %w", err)
	}
	return nil
}

// HasVectors reports whether the index contains embeddings, so callers can tell
// an un-embedded index from a missing model and give a useful message.
func (d *DB) HasVectors() (bool, error) {
	var n int
	err := d.sql.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='chunks_vec'`).Scan(&n)
	if err != nil || n == 0 {
		return false, err
	}
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM chunks_vec`).Scan(&n); err != nil {
		return false, nil // table exists but is unreadable without the extension
	}
	return n > 0, nil
}

// Stats summarizes index contents.
type Stats struct {
	Sessions int
	Chunks   int
	Projects int
	Vectors  int
}

// Stats reads current counts. Vectors is -1 when the vector table is not
// readable, which distinguishes "no semantic index" from "zero chunks".
func (d *DB) Stats() (Stats, error) {
	var s Stats
	err := d.sql.QueryRow(`
		SELECT (SELECT COUNT(*) FROM sessions),
		       (SELECT COUNT(*) FROM chunks),
		       (SELECT COUNT(DISTINCT project) FROM sessions)`).
		Scan(&s.Sessions, &s.Chunks, &s.Projects)
	if err != nil {
		return s, err
	}
	s.Vectors = -1
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM chunks_vec`).Scan(&s.Vectors); err != nil {
		s.Vectors = -1
	}
	return s, nil
}
