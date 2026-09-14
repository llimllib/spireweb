package web

import (
	"context"
	"html/template"
	stdsort "sort"
	"strings"

	"github.com/llimllib/spireweb/internal/index"
	"github.com/llimllib/spireweb/internal/search"
)

// Sentinels FTS5 wraps matched terms in.
//
// Not HTML: snippet() interleaves these with text taken from a session, so
// emitting tags here would produce a string that is part markup and part
// untrusted content, and escaping it would then destroy the markup. Escaping
// happens first, and only then do these become tags -- see Subtitle.
const (
	markOpen  = "\x01"
	markClose = "\x02"
)

// excerptTokens is roughly how much of a matching chunk a row shows. FTS5
// counts tokens, and two clamped lines hold about this many.
const excerptTokens = 28

// row is one entry in the list pane.
//
// Browsing and searching produce the same type so the template does not have
// to know which it is looking at. Excerpt is set only for search results.
type row struct {
	index.Summary
	Excerpt string

	// BestChunk is the highest-scoring matching chunk, kept so that opening
	// this result can locate the hit inside the session without running the
	// ranking a second time. Zero when browsing.
	BestChunk search.ChunkID
}

// Subtitle overrides index.Summary.Subtitle for search results, replacing the
// opening exchange with the text that actually matched.
//
// Returns template.HTML, having escaped the session text itself and then
// substituted <mark> for the sentinels. Nothing from the session survives
// unescaped.
func (r row) Subtitle() template.HTML {
	if r.Excerpt == "" {
		return template.HTML(template.HTMLEscapeString(r.Summary.Subtitle())) //nolint:gosec // escaped
	}
	escaped := template.HTMLEscapeString(r.Excerpt)
	escaped = strings.ReplaceAll(escaped, markOpen, "<mark>")
	escaped = strings.ReplaceAll(escaped, markClose, "</mark>")
	return template.HTML(escaped) //nolint:gosec // escaped above, markers are ours
}

// Sort orders for the list pane.
const (
	SortRelevance = ""
	SortNewest    = "new"
)

// rowsFor returns the list pane's contents: search results when there is a
// query, the chronological list when there is not.
//
// sort reorders results without changing which ones there are. Sorting by date
// is not a worse ranking, it is a different question: "the grafana session I
// had on Friday" is a memory of when, and no amount of relevance tuning
// answers it as directly as putting them in order.
func (s *Server) rowsFor(ctx context.Context, q, sort string) ([]row, error) {
	q = strings.TrimSpace(q)
	if q == "" || s.engine == nil {
		summaries, err := s.db.ListSessions(ctx, listLimit, 0)
		if err != nil {
			return nil, err
		}
		rows := make([]row, len(summaries))
		for i, sum := range summaries {
			rows[i] = row{Summary: sum}
		}
		return rows, nil
	}

	results, err := s.engine.SearchSessions(ctx, s.db.SQL(), search.Query{Text: q}, searchLimit)
	if err != nil {
		return nil, err
	}

	rows := make([]row, 0, len(results))
	for _, res := range results {
		r := row{Summary: res.Summary, BestChunk: res.BestChunk}
		// One extra query per row. At the result limit that is a bounded
		// number of indexed lookups, tens of microseconds each, and it buys
		// the single most useful thing a result can show.
		if ex, err := search.Excerpt(ctx, s.db.SQL(), res.BestChunk, q,
			excerptTokens, markOpen, markClose); err == nil && ex != "" {
			r.Excerpt = ex
		} else {
			r.Excerpt = res.BestText
		}
		rows = append(rows, r)
	}

	if sort == SortNewest {
		// Stable so that two sessions started in the same second keep the
		// order relevance gave them.
		stdsort.SliceStable(rows, func(i, j int) bool {
			return rows[i].StartedAt.After(rows[j].StartedAt)
		})
	}
	return rows, nil
}
