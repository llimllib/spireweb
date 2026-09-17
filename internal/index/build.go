package index

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/llimllib/spireweb/internal/session"
)

// Hostname identifies the machine that indexed a session, so that indexes from
// several machines can be told apart once they can be merged. Looked up once;
// a machine does not rename itself mid-run, and the fallback matters more than
// the precision.
var Hostname = sync.OnceValue(func() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
})

// Progress reports indexing state to a caller (the TUI or the CLI).
type Progress struct {
	Total    int    // session files discovered
	Done     int    // files examined
	Indexed  int    // files parsed and written
	Skipped  int    // unchanged since last run
	Excluded int    // not a session worth indexing; see session.SkipReason
	Failed   int    // unparseable
	Chunks   int    // chunks written this run
	Current  string // file being worked on
	Finished bool
	Err      error

	// Backfill reports that the run was promoted to a full reindex in order to
	// embed chunks that were indexed without vectors.
	Backfill bool

	// ArchiveBackfill reports the same promotion, to store messages for
	// sessions indexed before the archive existed.
	ArchiveBackfill bool

	// Messages counts rows added to the archive this run.
	Messages int
}

// BuildOptions configures a Build run.
type BuildOptions struct {
	Dir        string         // session directory
	ChunkChars int            // 0 uses session.MaxChunkChars
	Full       bool           // reindex everything, ignoring mtime/size
	OnProgress func(Progress) // optional, called as work proceeds
	Embedder   Embedder       // optional; nil means lexical-only
}

// Embedder turns chunk bodies into vectors. Defined here as an interface so
// that indexing does not depend on a particular embedding backend, and so a
// lexical-only build needs no embedding support at all.
type Embedder interface {
	// EmbedInto writes vectors for the given chunk rowids. Implementations may
	// batch. Called inside the indexing transaction.
	EmbedInto(ctx context.Context, tx *sql.Tx, chunkIDs []int64) error
	Name() string
	Dim() int
}

// Build indexes sessions incrementally.
//
// A session is reindexed when its (mtime, size) differ from what is stored.
// That is cheap and catches every real edit: pi appends to sessions, so any new
// message changes both. Files that vanished are removed from the index.
func Build(ctx context.Context, d *DB, opts BuildOptions) (Progress, error) {
	if opts.Dir == "" {
		opts.Dir = session.DefaultDir()
	}
	if opts.ChunkChars <= 0 {
		opts.ChunkChars = session.MaxChunkChars
	}

	report := func(p Progress) {
		if opts.OnProgress != nil {
			opts.OnProgress(p)
		}
	}

	// Refuse to mix vectors from different models in one index.
	if opts.Embedder != nil {
		if err := d.checkEmbedder(opts.Embedder, opts.Full); err != nil {
			return Progress{Err: err}, err
		}
	}

	// An index built lexically would otherwise never gain vectors: the skip
	// test compares mtime and size, so an unchanged file is passed over before
	// the embedder is ever consulted, and semantic search stays empty however
	// many times indexing runs. Promote the run to a full pass instead.
	// Unchanged chunks that already have vectors are still reused, so this
	// embeds what is missing rather than everything.
	backfill := false
	if opts.Embedder != nil && !opts.Full {
		var err error
		backfill, err = d.needsVectors()
		if err != nil {
			return Progress{Err: err}, err
		}
		opts.Full = backfill
	}

	// Likewise for the archive: an index built before the messages table
	// existed has sessions whose files will never look changed again.
	archiveBackfill := false
	if !opts.Full {
		var err error
		archiveBackfill, err = d.needsArchive()
		if err != nil {
			return Progress{Err: err}, err
		}
		opts.Full = archiveBackfill
	}

	// And once more for titles, which are written after a build finishes and
	// so are never indexed by the run that generated them.
	if !opts.Full {
		need, err := d.needsTitleChunks()
		if err != nil {
			return Progress{Err: err}, err
		}
		opts.Full = need
	}

	files, err := session.Discover(opts.Dir)
	if err != nil {
		return Progress{Err: err}, fmt.Errorf("discover %s: %w", opts.Dir, err)
	}

	known, err := d.knownFiles()
	if err != nil {
		return Progress{Err: err}, err
	}

	p := Progress{Total: len(files), Backfill: backfill, ArchiveBackfill: archiveBackfill}
	seen := make(map[string]bool, len(files))

	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return p, err
		}
		p.Done++
		p.Current = f.Path

		if !opts.Full {
			if prev, ok := known[f.Path]; ok &&
				prev.mtime == f.ModTime.UnixNano() && prev.size == f.Size {
				seen[f.Path] = true
				p.Skipped++
				report(p)
				continue
			}
		}

		// After the mtime test, not before: this reads the file, and an already
		// indexed session that has not changed should cost a stat and nothing
		// more. The consequence is that an index built before a file became
		// excluded keeps it until a full pass, which is the same bargain
		// needsVectors and needsArchive make.
		//
		// Deliberately not marked seen: the sweep below then removes it, so a
		// session indexed before this rule existed is dropped rather than left
		// behind with nothing to refresh it.
		if session.SkipReason(f.Path) != "" {
			p.Excluded++
			report(p)
			continue
		}

		seen[f.Path] = true

		// WithRaw because indexing is what fills the archive, and the archive
		// stores what pi wrote rather than what this code understood.
		s, err := session.ParseWithRaw(f.Path)
		if err != nil {
			p.Failed++
			report(p)
			continue
		}

		n, msgs, err := d.upsertSession(ctx, s, opts)
		if err != nil {
			return p, fmt.Errorf("index %s: %w", f.Path, err)
		}
		p.Indexed++
		p.Chunks += n
		p.Messages += msgs
		report(p)
	}

	// Drop sessions whose files are gone.
	for path := range known {
		if !seen[path] {
			if err := d.deleteByPath(path); err != nil {
				return p, err
			}
		}
	}

	if opts.Embedder != nil {
		if err := d.SetMeta(MetaEmbedModel, opts.Embedder.Name()); err != nil {
			return p, err
		}
		if err := d.SetMeta(MetaEmbedDim, fmt.Sprint(opts.Embedder.Dim())); err != nil {
			return p, err
		}
	}

	p.Finished = true
	p.Current = ""
	report(p)
	return p, nil
}

type fileState struct {
	mtime int64
	size  int64
}

func (d *DB) knownFiles() (map[string]fileState, error) {
	rows, err := d.sql.Query(`SELECT path, mtime, size FROM sessions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]fileState{}
	for rows.Next() {
		var p string
		var st fileState
		if err := rows.Scan(&p, &st.mtime, &st.size); err != nil {
			return nil, err
		}
		out[p] = st
	}
	return out, rows.Err()
}

// upsertSession writes a session and its chunks in one transaction, so a failure
// mid-write cannot leave a session with half its chunks.
//
// Chunks whose text is unchanged are kept rather than re-embedded. Sessions are
// append-only -- pi adds messages at the end -- so when a live session is
// reindexed almost every chunk is byte-identical to what is already stored.
// Re-embedding all of them made the cost proportional to total session size
// rather than to what changed: measured at 2.1s for a 200-chunk session where
// only 40 chunks were new, and growing linearly as the session grew. Reuse makes
// the cost proportional to the new text, which is what makes watch mode viable.
func (d *DB) upsertSession(ctx context.Context, s *session.Session, opts BuildOptions) (chunks, msgs int, err error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	blocks := s.Chunks(opts.ChunkChars)

	// The generated title, indexed as a chunk of its own so that the line the
	// list shows in bold is searchable. It is the densest sentence about a
	// session anywhere in the database -- written for exactly that purpose --
	// and it was the one sentence search could not see.
	//
	// Prepended rather than appended only so that it is obvious in a dump.
	if title, err := titleOf(ctx, tx, s.ID); err != nil {
		return 0, 0, err
	} else if title != "" {
		blocks = append([]session.TextBlock{{
			MsgIdx: TitleMsgIdx, Role: RoleTitle, Text: title,
		}}, blocks...)
	}

	// Enforce the model's token limit before anything reaches the embedder.
	// Character-based chunking cannot predict token count: 800 chars of prose is
	// 162 tokens, but 800 chars of dense punctuation is 802. Over-length input is
	// a clean error in the patched lembed fork and a segfault in upstream's build.
	//
	// Counting runs through tx, not the pooled *sql.DB: a query on a second
	// connection would block on the lock this transaction holds and deadlock.
	if tcf, ok := opts.Embedder.(interface {
		TokenCounterFor(*sql.Tx) session.TokenCounter
	}); ok {
		tc := tcf.TokenCounterFor(tx)
		var safe []session.TextBlock
		for _, c := range blocks {
			pieces, err := session.SplitToTokenLimit(tc, []string{c.Text}, session.MaxTokens)
			if err != nil {
				return 0, 0, fmt.Errorf("token check: %w", err)
			}
			for _, p := range pieces {
				safe = append(safe, session.TextBlock{MsgIdx: c.MsgIdx, Role: c.Role, Text: p})
			}
		}
		blocks = safe
	}

	// Chunks already stored for this session, keyed by content and position.
	// When embedding, only rows that already have a vector count as reusable;
	// otherwise a previous lexical-only pass would leave chunks un-embedded
	// forever.
	reusable, err := reusableChunks(tx, s.ID, opts.Embedder != nil)
	if err != nil {
		return 0, 0, err
	}

	keep := make(map[int64]bool, len(blocks))
	var toInsert []session.TextBlock
	for _, c := range blocks {
		key := chunkKey{msgIdx: c.MsgIdx, role: c.Role, text: c.Text}
		if id, ok := reusable[key]; ok && !keep[id] {
			keep[id] = true
			continue
		}
		toInsert = append(toInsert, c)
	}

	// Drop rows that are no longer part of the session (edited or removed text).
	if err := deleteChunksExceptTx(tx, s.ID, keep); err != nil {
		return 0, 0, err
	}

	// Upsert the session row rather than DELETE + INSERT.
	//
	// chunks.session_id declares ON DELETE CASCADE, so deleting the session row
	// would take every chunk with it -- including the ones just marked for reuse.
	// That silently defeated reuse (chunk ids climbed 1..360 across five rounds
	// of a 120-chunk session) and orphaned their vectors, since chunks_vec is a
	// virtual table and does not participate in cascades.
	//
	// A different file claiming the same session id is still handled: the path
	// conflict is cleared first, and ON CONFLICT(id) updates in place.
	if _, err := tx.Exec(
		`DELETE FROM sessions WHERE path = ? AND id <> ?`, s.Path, s.ID); err != nil {
		return 0, 0, err
	}
	// title is deliberately absent from the UPDATE list: it is written by a
	// separate pass and must survive a reindex, which happens every time the
	// session grows by one message.
	if _, err := tx.Exec(`
		INSERT INTO sessions(id, path, host, cwd, project, started_at, mtime, size, n_msgs, preview, reply)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			path = excluded.path, host = excluded.host, cwd = excluded.cwd,
			project = excluded.project, started_at = excluded.started_at,
			mtime = excluded.mtime, size = excluded.size,
			n_msgs = excluded.n_msgs, preview = excluded.preview,
			reply = excluded.reply`,
		s.ID, s.Path, Hostname(), s.CWD, s.Project(),
		s.StartedAt.UTC().Format(time.RFC3339),
		s.ModTime.UnixNano(), s.Size, len(s.Messages), s.Preview(200), s.Reply(200),
	); err != nil {
		return 0, 0, err
	}

	// After the session row, which the foreign key depends on, and inside the
	// same transaction: a session whose chunks were written but whose messages
	// were not would look archived and not be.
	msgs, err = archiveMessages(ctx, tx, s)
	if err != nil {
		return 0, 0, fmt.Errorf("archive: %w", err)
	}

	if len(toInsert) == 0 {
		return 0, msgs, tx.Commit()
	}

	insChunk, err := tx.PrepareContext(ctx,
		`INSERT INTO chunks(session_id, msg_idx, role, body) VALUES(?,?,?,?)`)
	if err != nil {
		return 0, 0, err
	}
	defer insChunk.Close()

	insFTS, err := tx.PrepareContext(ctx,
		`INSERT INTO chunks_fts(rowid, body) VALUES(?, ?)`)
	if err != nil {
		return 0, 0, err
	}
	defer insFTS.Close()

	ids := make([]int64, 0, len(toInsert))
	for _, c := range toInsert {
		res, err := insChunk.ExecContext(ctx, s.ID, c.MsgIdx, c.Role, c.Text)
		if err != nil {
			return 0, 0, err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return 0, 0, err
		}
		// External-content FTS5 requires explicit index maintenance.
		if _, err := insFTS.ExecContext(ctx, id, c.Text); err != nil {
			return 0, 0, err
		}
		ids = append(ids, id)
	}

	if opts.Embedder != nil {
		if err := opts.Embedder.EmbedInto(ctx, tx, ids); err != nil {
			return 0, 0, fmt.Errorf("embed: %w", err)
		}
	}

	return len(ids), msgs, tx.Commit()
}

// chunkKey identifies a chunk by content and position, so identical text in two
// different messages is not treated as the same row.
type chunkKey struct {
	msgIdx int
	role   string
	text   string
}

// reusableChunks maps stored chunk content to row id. With requireVector set,
// only rows that already have an embedding are returned.
func reusableChunks(tx *sql.Tx, sessionID string, requireVector bool) (map[chunkKey]int64, error) {
	q := `SELECT id, msg_idx, role, body FROM chunks WHERE session_id = ?`
	if requireVector && tableExistsTx(tx, "chunks_vec") {
		q = `SELECT c.id, c.msg_idx, c.role, c.body FROM chunks c
		     WHERE c.session_id = ?
		       AND EXISTS (SELECT 1 FROM chunks_vec v WHERE v.rowid = c.id)`
	}
	rows, err := tx.Query(q, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[chunkKey]int64{}
	for rows.Next() {
		var id int64
		var k chunkKey
		if err := rows.Scan(&id, &k.msgIdx, &k.role, &k.text); err != nil {
			return nil, err
		}
		if _, dup := out[k]; !dup {
			out[k] = id
		}
	}
	return out, rows.Err()
}

// deleteChunksTx removes all of a session's chunks with their FTS and vector rows.
func deleteChunksTx(tx *sql.Tx, sessionID string) error {
	return deleteChunksExceptTx(tx, sessionID, nil)
}

// deleteChunksExceptTx removes a session's chunks other than those in keep.
//
// Neither chunks_fts (external-content) nor chunks_vec cascades from the chunks
// table, so both must be maintained by hand. A stale row in either produces
// search hits pointing at chunks that no longer exist.
func deleteChunksExceptTx(tx *sql.Tx, sessionID string, keep map[int64]bool) error {
	rows, err := tx.Query(`SELECT id, body FROM chunks WHERE session_id = ?`, sessionID)
	if err != nil {
		return err
	}
	type row struct {
		id   int64
		body string
	}
	var doomed []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.body); err != nil {
			rows.Close()
			return err
		}
		if !keep[r.id] {
			doomed = append(doomed, r)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(doomed) == 0 {
		return nil
	}

	hasVec := tableExistsTx(tx, "chunks_vec")
	for _, r := range doomed {
		// External-content FTS5 needs the original text to remove its entry.
		if _, err := tx.Exec(`INSERT INTO chunks_fts(chunks_fts, rowid, body) VALUES('delete', ?, ?)`,
			r.id, r.body); err != nil {
			return err
		}
		if hasVec {
			if _, err := tx.Exec(`DELETE FROM chunks_vec WHERE rowid = ?`, r.id); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`DELETE FROM chunks WHERE id = ?`, r.id); err != nil {
			return err
		}
	}
	return nil
}

func tableExistsTx(tx *sql.Tx, name string) bool {
	var n int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

func (d *DB) deleteByPath(path string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var id string
	err = tx.QueryRow(`SELECT id FROM sessions WHERE path = ?`, path).Scan(&id)
	if err == sql.ErrNoRows {
		return tx.Rollback()
	}
	if err != nil {
		return err
	}
	if err := deleteChunksTx(tx, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// needsVectors reports whether any chunk lacks an embedding.
func (d *DB) needsVectors() (bool, error) {
	var n int
	err := d.sql.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='chunks_vec'`).Scan(&n)
	if err != nil || n == 0 {
		return false, err
	}
	var missing bool
	err = d.sql.QueryRow(`SELECT EXISTS(
		SELECT 1 FROM chunks c
		WHERE NOT EXISTS (SELECT 1 FROM chunks_vec v WHERE v.rowid = c.id))`).Scan(&missing)
	if err != nil {
		// The table exists but is unreadable without the extension loaded.
		return false, nil
	}
	return missing, nil
}

// checkEmbedder refuses to add vectors from one model to an index built with
// another. Cosine similarity between different models' vectors is meaningless,
// and the failure would be silent: plausible-looking but wrong rankings.
func (d *DB) checkEmbedder(e Embedder, full bool) error {
	return d.checkEmbedderName(e.Name(), full)
}

// checkEmbedderName holds the logic, separated so it can be tested without
// constructing a real embedding backend.
func (d *DB) checkEmbedderName(name string, full bool) error {
	prev, err := d.Meta(MetaEmbedModel)
	if err != nil {
		return err
	}
	if prev == "" || full || prev == name {
		return nil
	}
	return fmt.Errorf("index was built with model %q, now using %q: rebuild with --full", prev, name)
}
