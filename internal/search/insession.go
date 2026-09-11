package search

import (
	"context"
	"database/sql"
	"fmt"
)

// MaxMessageMatches caps how many matching chunks are considered within one
// session. A session has a few hundred chunks at most, so this is a guard
// rather than a real limit.
const MaxMessageMatches = 500

// MessagesMatching returns the indexes of messages in one session whose text
// matches the query, best match first and without duplicates.
//
// Lexical only, deliberately. Semantic ranking answers "which session is worth
// opening", and it answers it well: a query can match a session that shares no
// words with it. But "where in this session" is a question about where the
// words are, and a vector has no position to offer. When nothing matches
// lexically the caller falls back to the message behind the fused best chunk,
// which locates the hit without claiming to have found terms in it.
//
// Ordering comes from BM25 and deduplication happens here rather than in SQL:
// FTS5 will not evaluate bm25() inside an aggregate ("unable to use function
// bm25 in the requested context"), so grouping by message in the query means
// giving up the ranking that makes the first result the right one to scroll to.
func MessagesMatching(ctx context.Context, db *sql.DB, sessionID, queryText string, limit int) ([]int, error) {
	if db == nil || sessionID == "" {
		return nil, nil
	}
	match := ftsQuery(queryText)
	if match == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = MaxMessageMatches
	}

	rows, err := db.QueryContext(ctx, `
		SELECT c.msg_idx
		FROM chunks_fts f
		JOIN chunks c ON c.id = f.rowid
		WHERE chunks_fts MATCH ? AND c.session_id = ?
		ORDER BY bm25(chunks_fts)
		LIMIT ?`, match, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("in-session match: %w", err)
	}
	defer rows.Close()

	var out []int
	seen := map[int]bool{}
	for rows.Next() {
		var idx int
		if err := rows.Scan(&idx); err != nil {
			return nil, err
		}
		if seen[idx] {
			continue // a message can hold several matching chunks
		}
		seen[idx] = true
		out = append(out, idx)
	}
	return out, rows.Err()
}

// ChunkMessage returns the message index a chunk came from, for locating a hit
// that matched semantically and so has no terms to find.
func ChunkMessage(ctx context.Context, db *sql.DB, id ChunkID) (int, error) {
	var idx int
	err := db.QueryRowContext(ctx,
		`SELECT msg_idx FROM chunks WHERE id = ?`, int64(id)).Scan(&idx)
	return idx, err
}
