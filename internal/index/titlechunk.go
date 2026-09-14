package index

import (
	"context"
	"database/sql"
)

// RoleTitle marks the synthetic chunk holding a session's generated title.
//
// Not a role any message has: it is this package's own, so that a title can
// travel through the same lexical and semantic ranking as prose without a
// second index, a second query path, and a second set of bugs.
const RoleTitle = "title"

// TitleMsgIdx is the message index a title chunk claims.
//
// Negative on purpose. A chunk's msg_idx is what locates a hit inside a
// session, and a title belongs to no message; -1 matches nothing the
// transcript rendered, so in-session marking skips it instead of scrolling to
// a message that does not exist.
const TitleMsgIdx = -1

// titleOf reads a session's stored title, which the build must not overwrite
// but does need to index.
//
// Titles are written by a pass that runs after the build, so the title being
// indexed here is always the one generated on some previous run. A session
// titled for the first time gains its chunk on the next pass, which
// needsTitleChunks arranges.
func titleOf(ctx context.Context, tx *sql.Tx, sessionID string) (string, error) {
	var title sql.NullString
	err := tx.QueryRowContext(ctx,
		`SELECT title FROM sessions WHERE id = ?`, sessionID).Scan(&title)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return title.String, nil
}

// needsTitleChunks reports whether any titled session's indexed title differs
// from the title it now has.
//
// The third backfill of this shape, after vectors and the archive, and for the
// third version of the same reason: the title pass runs after the build, so
// the run that writes a title cannot index it, and the next run would skip the
// file as unchanged. Promoting to a full pass is what closes the gap.
//
// Compares the text rather than merely testing for existence. A session that
// was retitled has a title chunk -- the old one -- and looking only for a
// missing chunk left it there, matching in search forever for words nothing
// displays. Titles are capped well under the chunk limit, so one title is
// always exactly one chunk and this comparison is sound.
//
// It settles: once every title matches its chunk, this is false again.
func (d *DB) needsTitleChunks() (bool, error) {
	var stale bool
	err := d.sql.QueryRow(`SELECT EXISTS(
		SELECT 1 FROM sessions s
		WHERE s.title IS NOT NULL AND s.title <> ''
		  AND NOT EXISTS (
		    SELECT 1 FROM chunks c
		    WHERE c.session_id = s.id AND c.role = ? AND c.body = s.title))`,
		RoleTitle).Scan(&stale)
	return stale, err
}
