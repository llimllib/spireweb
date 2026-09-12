package index

import (
	"context"
	"database/sql"
	"time"

	"github.com/llimllib/spireweb/internal/session"
)

// archiveMessages stores a session's messages verbatim.
//
// Incremental in the only way that matters: pi appends, so a session that
// gained one message inserts one row rather than rewriting four hundred. The
// stored count is the starting point, which is correct exactly as long as
// sessions are append-only -- the same assumption chunk reuse and the title
// cache already rest on.
//
// A file that was rewritten *shorter* is handled, because that is a real state
// during a partial write: the extra rows are dropped rather than left behind to
// claim messages the session no longer has. A file rewritten to a different
// conversation of the same length keeps the old messages, which merge reports
// as divergence rather than silently resolving.
func archiveMessages(ctx context.Context, tx *sql.Tx, s *session.Session) (int, error) {
	var have int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE session_id = ?`, s.ID).Scan(&have); err != nil {
		return 0, err
	}

	if have > len(s.Messages) {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM messages WHERE session_id = ? AND idx >= ?`,
			s.ID, len(s.Messages)); err != nil {
			return 0, err
		}
		have = len(s.Messages)
	}
	if have == len(s.Messages) {
		return 0, nil
	}

	stmt, err := tx.PrepareContext(ctx,
		`INSERT OR REPLACE INTO messages(session_id, idx, role, at, content)
		 VALUES(?,?,?,?,?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	n := 0
	for i := have; i < len(s.Messages); i++ {
		m := s.Messages[i]
		if len(m.Raw) == 0 {
			// Parsed without raw bytes: archiving a re-marshalled struct would
			// store a lossy copy, which is worse than storing nothing.
			continue
		}
		var at string
		if !m.At.IsZero() {
			at = m.At.UTC().Format(time.RFC3339Nano)
		}
		if _, err := stmt.ExecContext(ctx, s.ID, i, m.Role, at, string(m.Raw)); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// NeedsArchive reports whether the next build will backfill the archive, so a
// caller can say so before it happens.
//
// Exported because the growth is large and automatic: the server indexes in
// the background, so an index built before this table existed gains ~400MB
// within seconds of starting the server, and a progress bar reading "indexing"
// does not convey that.
func (d *DB) NeedsArchive() (bool, error) { return d.needsArchive() }

// needsArchive reports whether any indexed session has no archived messages.
//
// The same shape as needsVectors, and for the same reason: the skip test
// compares mtime and size, so an index built before this table existed would
// pass over every unchanged file forever and the archive would stay empty
// however many times indexing ran.
func (d *DB) needsArchive() (bool, error) {
	var missing bool
	err := d.sql.QueryRow(`SELECT EXISTS(
		SELECT 1 FROM sessions s
		WHERE s.n_msgs > 0
		  AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.session_id = s.id))`).Scan(&missing)
	return missing, err
}

// CountMessages reports how many messages are archived.
func (d *DB) CountMessages(ctx context.Context) (int, error) {
	var n int
	err := d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages`).Scan(&n)
	return n, err
}
