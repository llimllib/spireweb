package search

import (
	"strings"
	"unicode"
)

// parsedQuery is a search box's contents split into the parts that constrain a
// search and the parts that only rank it.
//
// Quoting is the one piece of syntax the box understands. It means "only
// sessions containing this", not "prefer sessions containing this": if a
// phrase merely sorted matches to the top, a page of plausible results would
// look identical whether or not the corpus contained the phrase at all, and
// the question a quoted search is usually asking -- did I ever discuss this,
// exactly this -- would have no observable answer. An empty result set is that
// answer.
type parsedQuery struct {
	// Phrases are the quoted runs, already reduced to their tokens. Each is
	// required.
	Phrases [][]string

	// Words are everything unquoted. They only influence ranking.
	Words []string
}

// parseQuery splits text into quoted phrases and bare words.
//
// An unterminated quote is a phrase in progress, not an error: search runs on
// every keystroke, so the moment after typing the opening quote of `"deploy
// pipeline"` is a state the parser has to hold an opinion about, and refusing
// the whole query would blank the list mid-word.
func parseQuery(text string) parsedQuery {
	var p parsedQuery
	for i := 0; i < len(text); {
		if text[i] != '"' {
			i++
			continue
		}
		// Everything before the quote is bare words.
		p.Words = append(p.Words, terms(text[:i], true)...)

		rest := text[i+1:]
		end := strings.IndexByte(rest, '"')
		if end < 0 {
			// Unterminated: the rest of the line is the phrase so far.
			if ts := terms(rest, false); len(ts) > 0 {
				p.Phrases = append(p.Phrases, ts)
			}
			return p
		}
		if ts := terms(rest[:end], false); len(ts) > 0 {
			p.Phrases = append(p.Phrases, ts)
		}
		text = rest[end+1:]
		i = 0
	}
	p.Words = append(p.Words, terms(text, true)...)
	return p
}

// terms extracts indexable tokens, splitting the way the tokenizer does.
//
// dropSingles applies to bare words, where a one-character term matches far
// too much to be worth a query. Inside a phrase every token is kept: adjacency
// is the whole point, and dropping the "a" from "a little slow" would match
// text that never said it.
func terms(text string, dropSingles bool) []string {
	var out []string
	for _, w := range strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	}) {
		if dropSingles && len(w) < 2 {
			continue
		}
		out = append(out, w)
	}
	return out
}

// Empty reports that there is nothing to search for.
func (p parsedQuery) Empty() bool { return len(p.Phrases) == 0 && len(p.Words) == 0 }

// Phrases returns the quoted parts of a query as readable text.
//
// Exported for the empty state. Excluding results is only safe if a reader can
// tell that is what happened: "nothing matched" and "nothing contains this
// exact phrase" call for different next moves, and only the second one
// suggests dropping the quotes.
func Phrases(text string) []string {
	p := parseQuery(text)
	if len(p.Phrases) == 0 {
		return nil
	}
	out := make([]string, 0, len(p.Phrases))
	for _, ph := range p.Phrases {
		out = append(out, strings.Join(ph, " "))
	}
	return out
}

// phrase renders one phrase as an FTS5 phrase literal.
func phrase(tokens []string) string {
	return `"` + strings.Join(tokens, " ") + `"`
}

// strict is the FTS5 expression that every result must satisfy: all phrases,
// AND-ed. Empty when the query has no quoted part, which is the case where
// nothing is required and everything is a suggestion.
func (p parsedQuery) strict() string {
	if len(p.Phrases) == 0 {
		return ""
	}
	parts := make([]string, 0, len(p.Phrases))
	for _, ph := range p.Phrases {
		parts = append(parts, phrase(ph))
	}
	return strings.Join(parts, " AND ")
}

// ranking is the expression handed to FTS5 for ordering.
//
// With no phrases this is the historical behaviour: OR the words and let BM25
// sort it out, since AND would return nothing whenever one word is absent.
//
// With phrases it is `P AND (P OR words)`. The AND-ed part is the constraint;
// the OR-ed group exists so the bare words still reach BM25, which scores every
// phrase in the expression it is given. Without the second group a mixed query
// would rank as though the unquoted words had not been typed.
func (p parsedQuery) ranking() string {
	var optional []string
	for _, ph := range p.Phrases {
		optional = append(optional, phrase(ph))
	}
	for _, w := range p.Words {
		optional = append(optional, `"`+w+`"`)
	}
	if len(optional) == 0 {
		return ""
	}
	or := strings.Join(optional, " OR ")

	strict := p.strict()
	if strict == "" {
		return or
	}
	if len(p.Words) == 0 {
		return strict // the OR group would only repeat the phrases
	}
	return strict + " AND (" + or + ")"
}

// ftsQuery converts free text into the FTS5 MATCH expression used for ranking
// and for snippets.
//
// User input cannot be passed through: FTS5 has its own syntax, and characters
// common in developer queries (-, *, ", :, () ) are operators there. An
// unbalanced quote or a leading hyphen is a syntax error, which would surface
// as a failed search while typing. Terms are therefore extracted and quoted as
// literals -- which is also what makes a deliberate quote meaningful, since by
// then it is the only punctuation that survives parsing.
func ftsQuery(text string) string { return parseQuery(text).ranking() }

// strictQuery is the expression that decides whether a session qualifies at
// all, empty when the query asks for nothing exactly.
func strictQuery(text string) string { return parseQuery(text).strict() }
