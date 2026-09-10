package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// Lexical ranks chunks by FTS5 BM25.
//
// This is the half of hybrid search that vectors are bad at: exact identifiers,
// error strings, filenames, flags. Searching for NLContextualEmbedding or
// --pooling should find the literal occurrence, and an embedding will happily
// return something merely topically related instead.
type Lexical struct {
	DB *sql.DB
}

// Name identifies this ranker in FusionConfig.Weights and in debug output.
func (l *Lexical) Name() string { return "lexical" }

// Rank returns chunk IDs ordered by BM25 relevance, best first.
func (l *Lexical) Rank(ctx context.Context, q Query, limit int) ([]ChunkID, error) {
	match := ftsQuery(q.Text)
	if match == "" {
		return nil, nil
	}

	// bm25() returns a negative number where more negative is more relevant, so
	// ascending order puts the best match first.
	sb := strings.Builder{}
	sb.WriteString(`
		SELECT f.rowid
		FROM chunks_fts f
		JOIN chunks c ON c.id = f.rowid
		JOIN sessions s ON s.id = c.session_id
		WHERE chunks_fts MATCH ?`)
	args := []any{match}

	if q.Project != "" {
		sb.WriteString(` AND s.project = ?`)
		args = append(args, q.Project)
	}
	if q.Since != "" {
		sb.WriteString(` AND s.started_at >= ?`)
		args = append(args, q.Since)
	}
	sb.WriteString(` ORDER BY bm25(chunks_fts) LIMIT ?`)
	args = append(args, limit)

	rows, err := l.DB.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("fts5 query: %w", err)
	}
	defer rows.Close()

	var out []ChunkID
	for rows.Next() {
		var id ChunkID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ftsQuery converts free text into an FTS5 MATCH expression.
//
// User input cannot be passed through: FTS5 has its own syntax, and characters
// common in developer queries (-, *, ", :, () ) are operators there. An
// unbalanced quote or a leading hyphen is a syntax error, which would surface as
// a failed search while typing. Each word is therefore extracted and quoted as
// a literal, and the terms are OR-ed so partial matches still rank.
//
// OR rather than AND because fusion handles precision: a chunk matching more
// terms scores better under BM25 and rises anyway, while AND would return
// nothing for a query where one word is absent.
func ftsQuery(text string) string {
	var terms []string
	for _, w := range strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	}) {
		if len(w) < 2 {
			continue // single characters match too much to be useful
		}
		terms = append(terms, `"`+w+`"`)
	}
	if len(terms) == 0 {
		return ""
	}
	return strings.Join(terms, " OR ")
}

// Excerpt returns a snippet of a chunk with matching terms wrapped in open
// and close, for display alongside a result.
//
// Showing why a result appeared is most of what makes search feel
// trustworthy, and it matters more here than in a keyword-only tool:
// semantic hits routinely share no words with the query, so without an
// excerpt a result looks arbitrary.
//
// The markers are caller-supplied and deliberately not HTML. FTS5 pastes them
// into text that came from a session, so emitting tags here would produce a
// string that is part markup and part untrusted content, with no way to
// escape one without the other. Callers pass sentinels, escape the result,
// then substitute tags.
func Excerpt(ctx context.Context, db *sql.DB, id ChunkID, queryText string, maxTokens int, open, close string) (string, error) {
	match := ftsQuery(queryText)
	if match == "" {
		return chunkBody(ctx, db, id)
	}
	var snip string
	err := db.QueryRowContext(ctx, `
		SELECT snippet(chunks_fts, 0, ?, ?, '…', ?)
		FROM chunks_fts WHERE rowid = ? AND chunks_fts MATCH ?`,
		open, close, maxTokens, int64(id), match).Scan(&snip)
	if errors.Is(err, sql.ErrNoRows) {
		// Chunk did not match lexically (a pure semantic hit): fall back to
		// the head of the body, unmarked, because nothing in it matched.
		return chunkBody(ctx, db, id)
	}
	return snip, err
}

func chunkBody(ctx context.Context, db *sql.DB, id ChunkID) (string, error) {
	var body string
	err := db.QueryRowContext(ctx, `SELECT body FROM chunks WHERE id = ?`, id).Scan(&body)
	return body, err
}
