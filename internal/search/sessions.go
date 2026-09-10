package search

import (
	"context"
	"database/sql"
	"sort"
	"strings"

	"github.com/llimllib/spireweb/internal/index"
)

// SessionResult is a search hit rolled up to the session that contains it.
//
// It embeds index.Summary so a result renders through exactly the same row
// template as a browsed session. The alternative -- a parallel struct with
// the same fields -- drifts the moment either side gains a column.
type SessionResult struct {
	index.Summary

	Score     float64
	NumChunks int            // matching chunks in this session
	BestChunk ChunkID        // highest-scoring chunk
	BestText  string         // its body, before excerpting
	Ranks     map[string]int // per-ranker position of BestChunk
}

// SearchSessions runs a query and returns results grouped by session.
//
// A session's score is the best score among its chunks rather than the sum: a
// session that discusses the topic once, well, should outrank one that
// mentions it three times in passing. Chunk count is reported separately and
// breaks ties.
func (e *Engine) SearchSessions(ctx context.Context, db *sql.DB, q Query, limit int) ([]SessionResult, error) {
	if db == nil {
		return nil, nil
	}
	hits, err := e.Search(ctx, q)
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 {
		return nil, nil
	}

	byChunk := make(map[ChunkID]Scored, len(hits))
	args := make([]any, 0, len(hits))
	for _, h := range hits {
		byChunk[h.ID] = h
		args = append(args, int64(h.ID))
	}

	rows, err := db.QueryContext(ctx, `
		SELECT c.id, c.body, `+index.SummaryColumns("s")+`
		FROM chunks c JOIN sessions s ON s.id = c.session_id
		WHERE c.id IN (`+placeholders(len(args))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	bySession := map[string]*SessionResult{}
	for rows.Next() {
		var chunkID ChunkID
		var body string
		// Scanning in two steps because the summary columns are produced by a
		// helper: read the two leading values, then hand the rest over.
		summary, err := scanWithPrefix(rows, &chunkID, &body)
		if err != nil {
			return nil, err
		}

		cur, ok := bySession[summary.ID]
		if !ok {
			cur = &SessionResult{Summary: summary}
			bySession[summary.ID] = cur
		}
		cur.NumChunks++
		if sc := byChunk[chunkID]; sc.Score > cur.Score {
			cur.Score = sc.Score
			cur.BestChunk = chunkID
			cur.BestText = body
			cur.Ranks = sc.Ranks
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]SessionResult, 0, len(bySession))
	for _, r := range bySession {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].NumChunks != out[j].NumChunks {
			return out[i].NumChunks > out[j].NumChunks
		}
		return out[i].ID < out[j].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// prefixScanner adapts a row so index.ScanSummary can read the trailing
// columns after some leading ones have been claimed.
type prefixScanner struct {
	row    interface{ Scan(...any) error }
	prefix []any
}

func (p prefixScanner) Scan(dest ...any) error {
	return p.row.Scan(append(append([]any{}, p.prefix...), dest...)...)
}

func scanWithPrefix(row interface{ Scan(...any) error }, prefix ...any) (index.Summary, error) {
	return index.ScanSummary(prefixScanner{row: row, prefix: prefix})
}

func placeholders(n int) string {
	if n == 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
