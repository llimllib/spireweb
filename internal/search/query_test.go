package search

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// Quoting is the only syntax the box has, and the parser is where it survives
// or is thrown away.
func TestParseQuerySplitsPhrasesFromWords(t *testing.T) {
	for _, tc := range []struct {
		in      string
		phrases []string // joined with " " for legibility
		words   []string
	}{
		{"", nil, nil},
		{"deploy pipeline", nil, []string{"deploy", "pipeline"}},
		{`"deploy pipeline"`, []string{"deploy pipeline"}, nil},
		{`"deploy pipeline" timeout`, []string{"deploy pipeline"}, []string{"timeout"}},
		{`timeout "deploy pipeline"`, []string{"deploy pipeline"}, []string{"timeout"}},
		{`"one two" and "three four"`, []string{"one two", "three four"}, []string{"and"}},
		// Punctuation inside a phrase splits the way the tokenizer splits it,
		// which is what makes "go1.26" match the text that contains it.
		{`"go1.26"`, []string{"go1 26"}, nil},
		// Single characters are dropped from bare words, where they match
		// everything, and kept inside a phrase, where adjacency is the point.
		{`a little slow`, nil, []string{"little", "slow"}},
		{`"a little slow"`, []string{"a little slow"}, nil},
		// Half-typed, which happens on literally every keystroke.
		{`"deploy pip`, []string{"deploy pip"}, nil},
		{`"`, nil, nil},
		{`""`, nil, nil},
	} {
		got := parseQuery(tc.in)
		var phrases []string
		for _, p := range got.Phrases {
			phrases = append(phrases, strings.Join(p, " "))
		}
		if strings.Join(phrases, "|") != strings.Join(tc.phrases, "|") {
			t.Errorf("parseQuery(%q) phrases = %v, want %v", tc.in, phrases, tc.phrases)
		}
		if strings.Join(got.Words, "|") != strings.Join(tc.words, "|") {
			t.Errorf("parseQuery(%q) words = %v, want %v", tc.in, got.Words, tc.words)
		}
	}
}

func TestStrictQueryRequiresEveryPhrase(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"no quotes here", ""},
		{`"deploy pipeline"`, `"deploy pipeline"`},
		{`"deploy pipeline" timeout`, `"deploy pipeline"`},
		{`"one two" x "three four"`, `"one two" AND "three four"`},
	} {
		if got := strictQuery(tc.in); got != tc.want {
			t.Errorf("strictQuery(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The whole point: a phrase excludes, and does not merely demote.
func TestQuotedPhraseExcludesNonMatches(t *testing.T) {
	db := indexFixture(t, map[string][]string{
		"exact":    {"the deploy pipeline broke this morning"},
		"reversed": {"the pipeline deploy broke this morning"},
		"split":    {"we deploy the whole pipeline every morning"},
		"neither":  {"an unrelated conversation about morning coffee"},
	})
	engine := &Engine{Rankers: []Ranker{&Lexical{DB: db}}, Fusion: DefaultFusion()}

	got := sessionIDs(t, engine, db, `"deploy pipeline"`)
	if strings.Join(got, ",") != "exact" {
		t.Errorf("results = %v, want only the session containing the phrase", got)
	}

	// Unquoted, the same words are a suggestion and the near misses come back.
	if got := sessionIDs(t, engine, db, "deploy pipeline"); len(got) != 3 {
		t.Errorf("unquoted results = %v, want the three sessions mentioning either word", got)
	}
}

// A mixed query: the phrase decides who is in, the bare word decides the order.
func TestBareWordsRankWithinPhraseMatches(t *testing.T) {
	db := indexFixture(t, map[string][]string{
		"plain":   {"the deploy pipeline broke and nothing else happened here"},
		"timeout": {"the deploy pipeline broke because of a timeout in the runner"},
		"other":   {"a timeout in the runner, with no pipeline involved at all"},
	})
	engine := &Engine{Rankers: []Ranker{&Lexical{DB: db}}, Fusion: DefaultFusion()}

	got := sessionIDs(t, engine, db, `"deploy pipeline" timeout`)
	if strings.Join(got, ",") != "timeout,plain" {
		t.Errorf("results = %v, want both phrase matches with the timeout one first", got)
	}
}

// A ranker that knows nothing about phrases must not be able to smuggle in a
// session that lacks one. This is what the semantic half does in production.
func TestPhraseFilterSurvivesAnIgnorantRanker(t *testing.T) {
	db := indexFixture(t, map[string][]string{
		"exact":   {"the deploy pipeline broke this morning"},
		"neither": {"an unrelated conversation about morning coffee"},
	})
	// Returns every chunk in the corpus whatever the query, like a nearest
	// neighbour search with no filter.
	all := &everything{db: db}
	engine := &Engine{Rankers: []Ranker{&Lexical{DB: db}, all}, Fusion: DefaultFusion()}

	got := sessionIDs(t, engine, db, `"deploy pipeline"`)
	if strings.Join(got, ",") != "exact" {
		t.Errorf("results = %v, want the phrase filter to hold", got)
	}
	if !all.ran {
		t.Error("the test did not exercise the second ranker at all")
	}
}

type everything struct {
	db  *sql.DB
	ran bool
}

func (e *everything) Name() string { return "everything" }

func (e *everything) Rank(ctx context.Context, _ Query, _ int) ([]ChunkID, error) {
	e.ran = true
	rows, err := e.db.QueryContext(ctx, `SELECT id FROM chunks ORDER BY id`)
	if err != nil {
		return nil, err
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

func sessionIDs(t *testing.T, e *Engine, db *sql.DB, q string) []string {
	t.Helper()
	res, err := e.SearchSessions(context.Background(), db, Query{Text: q}, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(res))
	for i, r := range res {
		out[i] = r.ID
	}
	return out
}
