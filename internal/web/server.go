// Package web serves the browsing interface.
//
// Navigation is ordinary page loads: clicking a session is a link, and which
// session is selected is derived entirely from the URL, so there is no client
// state that can disagree with what is on screen. HTMX is used only where the
// page genuinely should not reload -- lazily expanding a tool call, and
// search-as-you-type.
package web

import (
	"embed"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/llimllib/spireweb/internal/index"
	"github.com/llimllib/spireweb/internal/render"
	"github.com/llimllib/spireweb/internal/search"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Options configures a Server.
type Options struct {
	// Dev reparses templates and reads static files from disk on every
	// request, so editing a template or the stylesheet does not need a
	// rebuild.
	Dev bool

	// SourceDir is where Dev mode reads templates and static files from.
	// Defaults to this package's directory relative to the working directory.
	SourceDir string
}

// Server holds everything a request needs.
type Server struct {
	db     *index.DB
	engine *search.Engine
	cache  *sessionCache
	opts   Options

	tmpl     *template.Template
	tmplOnce sync.Once
}

// New builds a Server over a read-only index handle.
//
// A nil engine disables search rather than failing: browsing an index that
// has no vectors, or was built by a binary that could not load the embedding
// model, is still worth doing.
func New(db *index.DB, engine *search.Engine, opts Options) (*Server, error) {
	if opts.SourceDir == "" {
		opts.SourceDir = devSourceDir()
	}
	s := &Server{db: db, engine: engine, cache: newSessionCache(defaultCacheSize), opts: opts}
	if _, err := s.templates(); err != nil {
		return nil, err
	}
	return s, nil
}

// funcs are the helpers templates may call.
var funcs = template.FuncMap{
	// date renders a list-row date the way a mail client does: a time for
	// today, a weekday within the last week, a date beyond that.
	"date": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		t = t.Local()
		now := time.Now()
		switch {
		case t.YearDay() == now.YearDay() && t.Year() == now.Year():
			return t.Format("15:04")
		case now.Sub(t) < 6*24*time.Hour:
			return t.Format("Mon")
		case t.Year() == now.Year():
			return t.Format("Jan 2")
		default:
			return t.Format("Jan 2 2006")
		}
	},
	"datetime": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.Local().Format("Mon 2 Jan 2006, 15:04")
	},
	"rfc3339": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format(time.RFC3339)
	},
}

// templates parses the template set, once in production and per call in dev.
func (s *Server) templates() (*template.Template, error) {
	if s.opts.Dev {
		return template.New("").Funcs(funcs).ParseGlob(
			filepath.Join(s.opts.SourceDir, "templates", "*.html"))
	}
	var err error
	s.tmplOnce.Do(func() {
		s.tmpl, err = template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html")
	})
	if err != nil {
		return nil, err
	}
	return s.tmpl, nil
}

// Handler returns the router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /search", s.handleSearch)
	mux.HandleFunc("GET /sessions/{id}", s.handleSession)
	mux.HandleFunc("GET /sessions/{id}/tool/{msg}/{blk}", s.handleTool)
	mux.HandleFunc("GET /static/chroma.css", s.handleChromaCSS)
	mux.Handle("GET /static/", s.staticHandler())
	return mux
}

func (s *Server) staticHandler() http.Handler {
	if s.opts.Dev {
		return http.StripPrefix("/static/",
			http.FileServer(http.Dir(filepath.Join(s.opts.SourceDir, "static"))))
	}
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// Only possible if the embed directive and this path disagree, which
		// is a build-time mistake rather than a runtime condition.
		panic(err)
	}
	// The sub-FS is already rooted at static/, so the prefix has to come off
	// the request path or every lookup asks for static/static/....
	return http.StripPrefix("/static/", http.FileServerFS(sub))
}

// handleChromaCSS serves the syntax highlighting stylesheet, generated from
// the chroma themes rather than committed, so the theme names in the render
// package are the only place the choice is recorded.
func (s *Server) handleChromaCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	if !s.opts.Dev {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	_, _ = io.WriteString(w, render.ChromaCSS())
}

// Serve runs an HTTP server until ctx is cancelled by the caller closing it.
func (s *Server) Server(addr string) *http.Server {
	return &http.Server{
		Addr:    addr,
		Handler: s.Handler(),
		// Generous: rendering a 7MB session is fast, but a cold page cache on
		// a spinning disk is not, and nothing here is exposed to the network.
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          nil,
	}
}

// devSourceDir locates this package's assets when running from source, so
// --dev works whether the binary was started from the repository root or from
// the package directory itself.
func devSourceDir() string {
	candidates := []string{filepath.Join("internal", "web"), "."}
	for _, c := range candidates {
		if _, err := os.Stat(filepath.Join(c, "templates")); err == nil {
			return c
		}
	}
	return candidates[0]
}
