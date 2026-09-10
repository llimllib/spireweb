package search

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// QueryEmbedder produces a query vector in the serialized form sqlite-vec
// expects. Kept as an interface so the ranker does not depend on a particular
// embedding backend, and so tests can supply a stub.
type QueryEmbedder interface {
	EmbedText(ctx context.Context, text string) ([]byte, error)
}

// Semantic ranks chunks by embedding similarity.
//
// This is the half that finds paraphrases: a search for "make the query faster"
// matches "add a database index to speed up the lookup" despite sharing no
// content words. It is correspondingly weak at exact identifiers, which is why
// the lexical ranker exists alongside it.
type Semantic struct {
	DB       *sql.DB
	Embedder QueryEmbedder
}

// Name identifies this ranker in FusionConfig.Weights and in debug output.
func (s *Semantic) Name() string { return "semantic" }

// Rank returns chunk IDs ordered by vector distance, nearest first.
func (s *Semantic) Rank(ctx context.Context, q Query, limit int) ([]ChunkID, error) {
	if strings.TrimSpace(q.Text) == "" {
		return nil, nil
	}
	vec, err := s.Embedder.EmbedText(ctx, q.Text)
	if err != nil {
		return nil, err
	}

	// vec0 KNN requires its own LIMIT inside the MATCH query; filters are
	// applied afterwards. Over-fetch so that filtering does not empty the
	// result set, but cap the multiplier to bound the work.
	knnLimit := limit
	if q.Project != "" || q.Since != "" {
		knnLimit = limit * 8
		if knnLimit > 2000 {
			knnLimit = 2000
		}
	}

	var sb strings.Builder
	sb.WriteString(`
		WITH knn AS (
		  SELECT rowid AS id, distance
		  FROM chunks_vec
		  WHERE embedding MATCH ? AND k = ?
		)
		SELECT knn.id
		FROM knn
		JOIN chunks c ON c.id = knn.id
		JOIN sessions s ON s.id = c.session_id
		WHERE 1=1`)
	args := []any{vec, knnLimit}

	if q.Project != "" {
		sb.WriteString(` AND s.project = ?`)
		args = append(args, q.Project)
	}
	if q.Since != "" {
		sb.WriteString(` AND s.started_at >= ?`)
		args = append(args, q.Since)
	}
	sb.WriteString(` ORDER BY knn.distance LIMIT ?`)
	args = append(args, limit)

	rows, err := s.DB.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("vector query: %w", err)
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
