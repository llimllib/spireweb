package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound reports a session id that is not in the index.
var ErrNotFound = errors.New("session not found")

// Summary is one row of the session list.
//
// Everything the list renders comes from the sessions table, so drawing the
// pane never touches a session file. Preview and Reply are stored at index
// time for exactly this reason: parsing 1100 files to render a list would cost
// more than every other part of a request combined.
type Summary struct {
	ID        string
	Project   string
	CWD       string
	Path      string
	Host      string
	StartedAt time.Time
	NumMsgs   int

	// Title is the generated summary, empty until that pass runs.
	Title   string
	Preview string
	Reply   string
}

// Heading is what the list shows in bold: the generated title when there is
// one, and the opening message when there is not, so a row is never blank.
func (s Summary) Heading() string {
	if s.Title != "" {
		return s.Title
	}
	if s.Preview != "" {
		return s.Preview
	}
	return "(empty session)"
}

// Subtitle is the third line. It avoids repeating whatever Heading used.
func (s Summary) Subtitle() string {
	if s.Title == "" {
		return s.Reply
	}
	return s.Preview
}

const summaryColumns = `id, project, cwd, path, host, started_at, n_msgs,
	COALESCE(title, ''), preview, reply`

func scanSummary(rows interface{ Scan(...any) error }) (Summary, error) {
	var s Summary
	var startedAt string
	err := rows.Scan(&s.ID, &s.Project, &s.CWD, &s.Path, &s.Host, &startedAt,
		&s.NumMsgs, &s.Title, &s.Preview, &s.Reply)
	if err != nil {
		return s, err
	}
	// Timestamps are stored as RFC3339 text. A session written by a client with
	// a different format should not take the whole list down with it.
	s.StartedAt, _ = time.Parse(time.RFC3339, startedAt)
	return s, nil
}

// ListSessions returns sessions newest first.
func (d *DB) ListSessions(ctx context.Context, limit, offset int) ([]Summary, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+summaryColumns+` FROM sessions
		 ORDER BY started_at DESC, id LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Summary
	for rows.Next() {
		s, err := scanSummary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Session returns one session's metadata.
func (d *DB) Session(ctx context.Context, id string) (Summary, error) {
	row := d.sql.QueryRowContext(ctx,
		`SELECT `+summaryColumns+` FROM sessions WHERE id = ?`, id)
	s, err := scanSummary(row)
	if errors.Is(err, sql.ErrNoRows) {
		return s, fmt.Errorf("%q: %w", id, ErrNotFound)
	}
	return s, err
}

// CountSessions reports how many sessions are indexed.
func (d *DB) CountSessions(ctx context.Context) (int, error) {
	var n int
	err := d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&n)
	return n, err
}
