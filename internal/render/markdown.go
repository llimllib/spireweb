// Package render turns session content into HTML.
//
// Markdown becomes HTML on the server rather than in the browser. The HTML
// HTMX swaps in is then final: no flash of unhighlighted code, no client-side
// pass over a large transcript, and no JavaScript dependency to do work Go can
// do while it is already assembling the page.
package render

import (
	"bytes"
	"html/template"
	"strings"
	"sync"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/util"
)

// Chroma styles, chosen to sit close to the light and dark palettes in app.css.
const (
	lightStyle = "github"
	darkStyle  = "github-dark"
)

// md is the shared converter.
//
// Raw HTML is never trusted, which is the single most important property in
// this package. Session content is arbitrary text: it routinely contains HTML
// and JavaScript, because pasting both into an agent is an ordinary thing to
// do.
//
// The load-bearing piece is escapeRawHTML below, not the absence of
// WithUnsafe: it replaces the renderer for HTML nodes outright, so it holds
// even if someone later adds WithUnsafe. There is a test that fails when it
// is removed.
var md = sync.OnceValue(func() goldmark.Markdown {
	return goldmark.New(
		goldmark.WithExtensions(
			extension.GFM,
			highlighting.NewHighlighting(
				highlighting.WithStyle(lightStyle),
				// Classes rather than inline styles, so one stylesheet can
				// restyle every code block and dark mode is a media query
				// rather than a re-render.
				highlighting.WithFormatOptions(chromahtml.WithClasses(true)),
			),
		),
		goldmark.WithRendererOptions(
			html.WithHardWraps(),
			// Priority below goldmark's own renderer (1000), so these win.
			renderer.WithNodeRenderers(util.Prioritized(escapeRawHTML{}, 1)),
		),
	)
})

// escapeRawHTML renders HTML found in the source as visible text.
//
// goldmark's safe default does not escape raw HTML, it discards it, leaving
// "<!-- raw HTML omitted -->" in its place. That is safe but lossy, and the
// loss lands exactly where it is least acceptable: sessions are full of HTML
// quoted as subject matter, and a transcript that silently drops the markup
// under discussion is worse than useless. Escaping keeps the content and is
// equally safe.
type escapeRawHTML struct{}

func (escapeRawHTML) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindRawHTML, renderRawHTML)
	reg.Register(ast.KindHTMLBlock, renderHTMLBlock)
}

func renderRawHTML(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkSkipChildren, nil
	}
	n := node.(*ast.RawHTML)
	for i := range n.Segments.Len() {
		seg := n.Segments.At(i)
		_, _ = w.Write(util.EscapeHTML(seg.Value(source)))
	}
	return ast.WalkSkipChildren, nil
}

func renderHTMLBlock(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	n := node.(*ast.HTMLBlock)
	if entering {
		for i := range n.Lines().Len() {
			seg := n.Lines().At(i)
			_, _ = w.Write(util.EscapeHTML(seg.Value(source)))
		}
		return ast.WalkContinue, nil
	}
	if n.HasClosure() {
		_, _ = w.Write(util.EscapeHTML(n.ClosureLine.Value(source)))
	}
	return ast.WalkContinue, nil
}

// Markdown converts src to HTML.
//
// The result is marked safe because goldmark produced it with raw HTML
// disabled, so every angle bracket that came from the session has already been
// escaped. Nothing else in this package may return template.HTML built from
// session text without going through here.
func Markdown(src string) template.HTML {
	var buf bytes.Buffer
	if err := md().Convert([]byte(src), &buf); err != nil {
		// Convert fails only on writer errors, which a bytes.Buffer does not
		// produce. Fall back to escaped plain text rather than dropping content.
		return template.HTML(template.HTMLEscapeString(src)) //nolint:gosec // escaped above
	}
	return template.HTML(buf.String()) //nolint:gosec // goldmark ran with raw HTML disabled
}

// ChromaCSS returns the stylesheet for highlighted code, with each theme
// behind its own prefers-color-scheme media query.
//
// Generated at runtime rather than committed, so the themes named above are
// the only place the choice is recorded.
//
// Both blocks are scoped, rather than letting the dark one override an
// unscoped light one, because a theme emits a rule only for the token types
// it colours. github-dark leaves punctuation and plain identifiers (.p, .nx,
// .na, .nb, .bp) to inherit from .chroma, so with the light rules always in
// force those tokens kept github's near-black on a near-black background and
// most of a code block was invisible in dark mode. A light query matches when
// the user has no preference as well, so nothing is left unstyled.
var ChromaCSS = sync.OnceValue(func() string {
	var b strings.Builder
	formatter := chromahtml.New(chromahtml.WithClasses(true))

	writeStyle := func(name string) {
		style := styles.Get(name)
		if style == nil {
			style = styles.Fallback
		}
		_ = formatter.WriteCSS(&b, style)
	}

	b.WriteString("@media (prefers-color-scheme: light) {\n")
	writeStyle(lightStyle)
	b.WriteString("}\n@media (prefers-color-scheme: dark) {\n")
	writeStyle(darkStyle)
	b.WriteString("}\n")
	return b.String()
})
