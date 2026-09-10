package search

import (
	"context"
	"errors"
	"math"
	"testing"
)

func ids(s []Scored) []ChunkID {
	out := make([]ChunkID, len(s))
	for i, x := range s {
		out[i] = x.ID
	}
	return out
}

func eq(a, b []ChunkID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The central property of RRF: a document both rankers agree on beats one that
// only tops a single ranker. Here 7 is rank 2 semantically and rank 1
// lexically, while 1 is rank 1 semantically and rank 3 lexically.
func TestFusePrefersAgreement(t *testing.T) {
	got := Fuse(DefaultFusion(), map[string][]ChunkID{
		"semantic": {1, 7, 3, 9},
		"lexical":  {7, 2, 1},
	})
	if got[0].ID != 7 {
		t.Errorf("expected chunk 7 first (found by both rankers), got %v", ids(got))
	}
	if got[0].Ranks["semantic"] != 2 || got[0].Ranks["lexical"] != 1 {
		t.Errorf("ranks not recorded: %v", got[0].Ranks)
	}
	// Every candidate from either ranker must appear exactly once.
	if len(got) != 5 {
		t.Errorf("expected 5 unique results, got %d: %v", len(got), ids(got))
	}
}

func TestFuseWeights(t *testing.T) {
	res := map[string][]ChunkID{
		"semantic": {1, 2},
		"lexical":  {3, 4},
	}
	// Weighting lexical heavily must pull its top hit to the front.
	cfg := DefaultFusion()
	cfg.Weights = map[string]float64{"lexical": 10}
	if got := Fuse(cfg, res); got[0].ID != 3 {
		t.Errorf("lexical weighted 10x should rank 3 first, got %v", ids(got))
	}
	// Equal weights: semantic's first hit wins on the ID tiebreak.
	if got := Fuse(DefaultFusion(), res); got[0].ID != 1 {
		t.Errorf("equal weights should rank 1 first, got %v", ids(got))
	}
}

// Weight 0 must fully disable a ranker's influence, giving semantic-only or
// lexical-only search without a separate code path.
func TestFuseZeroWeightDisables(t *testing.T) {
	cfg := DefaultFusion()
	cfg.Weights = map[string]float64{"lexical": 0}
	got := Fuse(cfg, map[string][]ChunkID{
		"semantic": {5, 6},
		"lexical":  {9, 8, 7},
	})
	for _, s := range got {
		if s.ID == 9 && s.Score != 0 {
			t.Errorf("disabled ranker contributed score: %+v", s)
		}
	}
	if got[0].ID != 5 {
		t.Errorf("expected semantic order to dominate, got %v", ids(got))
	}
}

func TestFuseK(t *testing.T) {
	res := map[string][]ChunkID{"a": {1, 2}}
	// Small K exaggerates the gap between rank 1 and rank 2.
	sharp := Fuse(FusionConfig{K: 1}, res)
	flat := Fuse(FusionConfig{K: 1000}, res)
	sharpGap := sharp[0].Score - sharp[1].Score
	flatGap := flat[0].Score - flat[1].Score
	if sharpGap <= flatGap {
		t.Errorf("K=1 gap (%.6f) should exceed K=1000 gap (%.6f)", sharpGap, flatGap)
	}
}

func TestFuseKDefaults(t *testing.T) {
	res := map[string][]ChunkID{"a": {1}}
	want := 1.0 / 61.0 // K=60, rank 1
	for _, cfg := range []FusionConfig{{}, {K: 0}, {K: -5}} {
		got := Fuse(cfg, res)[0].Score
		if math.Abs(got-want) > 1e-12 {
			t.Errorf("K=%v: score %.12f, want %.12f (K should default to 60)", cfg.K, got, want)
		}
	}
}

func TestFuseEmptyAndMissingRankers(t *testing.T) {
	if got := Fuse(DefaultFusion(), nil); len(got) != 0 {
		t.Errorf("nil results should fuse to nothing, got %v", ids(got))
	}
	if got := Fuse(DefaultFusion(), map[string][]ChunkID{}); len(got) != 0 {
		t.Errorf("empty results should fuse to nothing, got %v", ids(got))
	}
	// A ranker returning nothing must not affect the other's ordering.
	got := Fuse(DefaultFusion(), map[string][]ChunkID{
		"semantic": {4, 5},
		"lexical":  {},
	})
	if !eq(ids(got), []ChunkID{4, 5}) {
		t.Errorf("empty ranker changed results: %v", ids(got))
	}
}

// Map iteration order is random in Go; fused output must not be.
func TestFuseDeterministic(t *testing.T) {
	res := map[string][]ChunkID{
		"a": {1, 2, 3},
		"b": {3, 2, 1},
		"c": {2, 1, 3},
	}
	first := ids(Fuse(DefaultFusion(), res))
	for i := 0; i < 50; i++ {
		if got := ids(Fuse(DefaultFusion(), res)); !eq(first, got) {
			t.Fatalf("nondeterministic: %v then %v", first, got)
		}
	}
}

func TestFuseScoreFormula(t *testing.T) {
	// Two rankers, weights 1 and 2, chunk at rank 1 and rank 3, K=60.
	cfg := FusionConfig{K: 60, Weights: map[string]float64{"b": 2}}
	got := Fuse(cfg, map[string][]ChunkID{"a": {7}, "b": {1, 2, 7}})
	want := 1.0/61.0 + 2.0/63.0
	for _, s := range got {
		if s.ID == 7 {
			if math.Abs(s.Score-want) > 1e-12 {
				t.Errorf("score = %.12f, want %.12f", s.Score, want)
			}
			return
		}
	}
	t.Fatal("chunk 7 missing from results")
}

// --- Engine ---

type fakeRanker struct {
	name string
	out  []ChunkID
	err  error
	runs *int
}

func (f *fakeRanker) Name() string { return f.name }
func (f *fakeRanker) Rank(ctx context.Context, q Query, limit int) ([]ChunkID, error) {
	if f.runs != nil {
		*f.runs++
	}
	return f.out, f.err
}

// A failing semantic backend should degrade to lexical-only, not return nothing.
func TestEngineToleratesRankerFailure(t *testing.T) {
	e := &Engine{
		Rankers: []Ranker{
			&fakeRanker{name: "semantic", err: errors.New("model unavailable")},
			&fakeRanker{name: "lexical", out: []ChunkID{1, 2}},
		},
		Fusion: DefaultFusion(),
	}
	got, err := e.Search(context.Background(), Query{Text: "x"})
	if err != nil {
		t.Fatalf("one failing ranker should not fail the search: %v", err)
	}
	if !eq(ids(got), []ChunkID{1, 2}) {
		t.Errorf("got %v, want [1 2]", ids(got))
	}
}

// If every ranker fails, the error must surface rather than looking like "no
// results found".
func TestEngineAllRankersFail(t *testing.T) {
	e := &Engine{
		Rankers: []Ranker{
			&fakeRanker{name: "semantic", err: errors.New("boom")},
			&fakeRanker{name: "lexical", err: errors.New("bang")},
		},
		Fusion: DefaultFusion(),
	}
	if _, err := e.Search(context.Background(), Query{Text: "x"}); err == nil {
		t.Error("expected an error when all rankers fail")
	}
}

// A disabled ranker should not even be queried.
func TestEngineSkipsZeroWeightRanker(t *testing.T) {
	runs := 0
	cfg := DefaultFusion()
	cfg.Weights = map[string]float64{"semantic": 0}
	e := &Engine{
		Rankers: []Ranker{
			&fakeRanker{name: "semantic", out: []ChunkID{9}, runs: &runs},
			&fakeRanker{name: "lexical", out: []ChunkID{1}},
		},
		Fusion: cfg,
	}
	got, err := e.Search(context.Background(), Query{Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if runs != 0 {
		t.Errorf("disabled ranker was queried %d times", runs)
	}
	if !eq(ids(got), []ChunkID{1}) {
		t.Errorf("got %v, want [1]", ids(got))
	}
}

// --- FTS5 query construction ---

// Free text must never reach FTS5 unescaped: -, ", *, : and () are operators
// there, so a query typed while searching would be a syntax error.
func TestFTSQuery(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"a", ""}, // single chars dropped
		{"hello", `"hello"`},
		{"hello world", `"hello" OR "world"`},
		{"NLContextualEmbedding", `"NLContextualEmbedding"`},
		{"snake_case_name", `"snake_case_name"`},
		{"--pooling mean", `"pooling" OR "mean"`},
		{`unbalanced " quote`, `"unbalanced" OR "quote"`},
		{"a* OR b*", `"OR"`}, // operators stripped, short terms dropped
		{"c++ / go", `"go"`},
		{"error: NOT found", `"error" OR "NOT" OR "found"`},
		{"go1.26", `"go1" OR "26"`},
	} {
		if got := ftsQuery(tc.in); got != tc.want {
			t.Errorf("ftsQuery(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
