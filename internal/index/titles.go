package index

import (
	"context"
	"fmt"
)

// addedColumns are columns added to an existing table after the fact.
//
// A schema-version bump is the other way to do this, and it is the wrong one
// here: bumping discards the database and rebuilds it, which means re-embedding
// 46k chunks -- minutes of GPU time -- to gain two nullable columns that start
// out NULL anyway. The version exists for changes that make an old index
// *unusable*; an index without these columns is merely an index with no titles
// yet, which is the state every index starts in.
//
// ALTER TABLE ADD COLUMN is O(1) in SQLite and the added column reads as NULL
// for existing rows, so this is safe to run on every open.
var addedColumns = []struct{ table, column, decl string }{
	// The hash of the prose the title was generated from. What makes a reindex
	// free: a session whose opening has not changed is not summarized again.
	{"sessions", "title_key", "TEXT"},

	// n_msgs as of the last time the title pass looked at this session. Only
	// an optimization, and a large one: without it the pass would have to parse
	// all 1128 session files on every run just to discover that nothing moved.
	{"sessions", "title_msgs", "INTEGER"},
}

// migrate applies additive column changes to an existing index.
func (d *DB) migrate() error {
	for _, c := range addedColumns {
		has, err := d.hasColumn(c.table, c.column)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		if _, err := d.sql.Exec(fmt.Sprintf(
			`ALTER TABLE %s ADD COLUMN %s %s`, c.table, c.column, c.decl)); err != nil {
			return fmt.Errorf("add %s.%s: %w", c.table, c.column, err)
		}
	}
	return nil
}

func (d *DB) hasColumn(table, column string) (bool, error) {
	var n int
	err := d.sql.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n)
	return n > 0, err
}

// TitleCandidate is a session the title pass may need to do something about.
type TitleCandidate struct {
	ID   string
	Path string

	// Key is the hash of the prose the current title was generated from, empty
	// when there is no title yet.
	Key string

	// NumMsgs is the session's current message count, written back once the
	// pass has looked at it.
	NumMsgs int
}

// TitleCandidates returns sessions that might need a title, newest first.
//
// "Might" is the point: this is a cheap SQL filter, and the expensive check --
// parsing the file and hashing its opening prose -- happens per candidate. A
// session that has grown is a candidate even though its title is almost
// certainly still accurate, because growth is the only signal available
// without reading the file.
//
// Newest first because that is the order the list pane shows, so the titles a
// user is about to look at appear before the ones they are not.
//
// The filter is on title_msgs alone rather than on title: a session that has
// been examined is settled whether or not it ended up with a title, and one
// whose summary failed is left unexamined precisely so that it comes back
// here next time.
func (d *DB) TitleCandidates(ctx context.Context) ([]TitleCandidate, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT id, path, COALESCE(title_key, ''), n_msgs FROM sessions
		WHERE title_msgs IS NULL OR title_msgs <> n_msgs
		ORDER BY started_at DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TitleCandidate
	for rows.Next() {
		var c TitleCandidate
		if err := rows.Scan(&c.ID, &c.Path, &c.Key, &c.NumMsgs); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetTitle stores a generated title with the key it was generated from.
func (d *DB) SetTitle(ctx context.Context, id, title, key string, nMsgs int) error {
	_, err := d.sql.ExecContext(ctx,
		`UPDATE sessions SET title = ?, title_key = ?, title_msgs = ? WHERE id = ?`,
		title, key, nMsgs, id)
	return err
}

// MarkTitleChecked records that the pass examined a session and found nothing
// to do, so the next run does not parse the file again.
//
// Deliberately does not touch title or title_key: a session whose prose hash
// is unchanged keeps the title it has, and one whose summarization failed
// keeps no title and is retried, because a failure never gets this far.
func (d *DB) MarkTitleChecked(ctx context.Context, id string, nMsgs int) error {
	_, err := d.sql.ExecContext(ctx,
		`UPDATE sessions SET title_msgs = ? WHERE id = ?`, nMsgs, id)
	return err
}

// CountTitledSessions reports how many sessions have a generated title.
func (d *DB) CountTitledSessions(ctx context.Context) (int, error) {
	var n int
	err := d.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE title IS NOT NULL AND title <> ''`).Scan(&n)
	return n, err
}
