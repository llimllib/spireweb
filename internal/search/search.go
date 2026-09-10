// Package search ranks chunks against a query.
//
// Ranking is split into independent Rankers whose outputs are combined by a
// fusion function. Rankers return ordered candidate IDs; fusion decides the
// final order. That separation exists because scores from different rankers are
// not comparable: sqlite-vec returns an unbounded L2 distance where lower is
// better, FTS5 returns a negative BM25 score whose magnitude depends on corpus
// and query length. Normalizing them against each other requires constants that
// are wrong on the next corpus.
package search

import (
	"context"
	"sort"
)

// ChunkID identifies a chunk row.
type ChunkID int64

// Query is a search request.
type Query struct {
	Text    string
	Project string // optional exact-match filter on sessions.project
	Since   string // optional ISO date lower bound on sessions.started_at
}

// Ranker produces candidate chunks in descending relevance.
//
// Rankers do not return scores. Only their ordering is used, so a ranker is
// free to use whatever internal scale it likes. A ranker that finds nothing
// returns an empty slice, not an error.
type Ranker interface {
	Name() string
	Rank(ctx context.Context, q Query, limit int) ([]ChunkID, error)
}

// FusionConfig tunes Reciprocal Rank Fusion.
type FusionConfig struct {
	// K damps the advantage of top ranks. The conventional value is 60. Smaller
	// K sharpens the difference between rank 1 and rank 2; larger K flattens the
	// curve so agreement across rankers matters more than any single position.
	K float64

	// Weights scales each ranker's contribution by name. Missing entries default
	// to 1. A weight of 0 disables a ranker, which makes semantic-only or
	// lexical-only search a configuration change rather than a code path.
	Weights map[string]float64

	// Limit is how many candidates to request from each ranker. Larger values
	// give fusion more to work with at the cost of more work per ranker.
	Limit int
}

// DefaultFusion returns the standard configuration.
func DefaultFusion() FusionConfig {
	return FusionConfig{K: 60, Weights: map[string]float64{}, Limit: 100}
}

func (c FusionConfig) k() float64 {
	if c.K <= 0 {
		return 60
	}
	return c.K
}

func (c FusionConfig) limit() int {
	if c.Limit <= 0 {
		return 100
	}
	return c.Limit
}

func (c FusionConfig) weight(name string) float64 {
	if c.Weights == nil {
		return 1
	}
	if w, ok := c.Weights[name]; ok {
		return w
	}
	return 1
}

// Scored is a fused result. Ranks records each ranker's 1-based position for
// this chunk, which is what makes fusion behavior inspectable in the UI rather
// than a black box.
type Scored struct {
	ID    ChunkID
	Score float64
	Ranks map[string]int
}

// Fuse combines ranked candidate lists using Reciprocal Rank Fusion:
//
//	score(d) = sum over rankers r of  weight(r) / (K + rank_r(d))
//
// Only rank position enters the formula, so no score calibration is needed and
// rankers can be added or removed without retuning anything.
//
// Fuse is pure: no database, no context, no I/O. It is the piece most worth
// testing directly, and swapping in a different fusion strategy means replacing
// this one function.
func Fuse(cfg FusionConfig, results map[string][]ChunkID) []Scored {
	k := cfg.k()
	acc := make(map[ChunkID]*Scored)

	for name, ids := range results {
		w := cfg.weight(name)
		for i, id := range ids {
			s, ok := acc[id]
			if !ok {
				s = &Scored{ID: id, Ranks: make(map[string]int, len(results))}
				acc[id] = s
			}
			rank := i + 1
			s.Ranks[name] = rank
			s.Score += w / (k + float64(rank))
		}
	}

	out := make([]Scored, 0, len(acc))
	for _, s := range acc {
		out = append(out, *s)
	}
	// Ties break on ID so results are deterministic across runs.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Engine runs a set of rankers and fuses their output.
type Engine struct {
	Rankers []Ranker
	Fusion  FusionConfig
}

// Search ranks chunks for a query. Rankers are run in sequence; a ranker that
// fails is skipped rather than failing the whole search, so a broken semantic
// backend degrades to lexical-only instead of returning nothing.
func (e *Engine) Search(ctx context.Context, q Query) ([]Scored, error) {
	// A nil engine or an engine with no rankers has nothing to contribute. Return
	// empty rather than panicking: search runs from UI event handlers, where a
	// panic takes down the whole program.
	if e == nil || len(e.Rankers) == 0 {
		return nil, nil
	}
	results := make(map[string][]ChunkID, len(e.Rankers))
	var firstErr error

	for _, r := range e.Rankers {
		if e.Fusion.weight(r.Name()) == 0 {
			continue // disabled by configuration; do not spend the query
		}
		ids, err := r.Rank(ctx, q, e.Fusion.limit())
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		results[r.Name()] = ids
	}

	fused := Fuse(e.Fusion, results)
	if len(fused) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return fused, nil
}
