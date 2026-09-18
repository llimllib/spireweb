// Command spireweb indexes and browses pi agent sessions.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/llimllib/spireweb/internal/config"
	"github.com/llimllib/spireweb/internal/embed"
	"github.com/llimllib/spireweb/internal/index"
	"github.com/llimllib/spireweb/internal/session"
	"github.com/llimllib/spireweb/internal/titles"
)

// Version is stamped in at build time by `mise run build`.
var Version = "dev"

const usage = `spireweb - search and read pi agent sessions

usage:
  spireweb serve [flags]   browse sessions in a web interface
  spireweb index [flags]   build or update the search index
  spireweb stats [flags]   report what is in the index
  spireweb doctor [flags]  check the index for inconsistencies
  spireweb info            show paths and configuration
  spireweb version

flags:
  --db PATH      index location (default %s)
  --dir PATH     session directory, repeatable (default: from the
                 settings file, or detected)
  --full         reindex everything rather than what changed
  --lexical      skip semantic indexing, even if the model is installed
  --addr ADDR    serve on this address (default 127.0.0.1:8080)
  --dev          reload templates and static files from disk per request
  --open         open a browser once the server is listening
  --no-watch     do not index in the background while serving
  --no-titles    do not generate session titles (same as titles = "off")
  --titles N     stop after generating N titles (0 for no limit)
  --titles-via   api (ANTHROPIC_API_KEY) or claude (the Claude Code CLI,
                 which bills a Pro/Max subscription rather than the API).
                 Defaults to the settings file's titles value.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, usage, index.DefaultPath())
		os.Exit(2)
	}

	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	dbPath := fs.String("db", index.DefaultPath(), "index location")
	var dirs dirList
	fs.Var(&dirs, "dir", "session directory (repeatable)")
	full := fs.Bool("full", false, "reindex everything")
	lexical := fs.Bool("lexical", false, "skip semantic indexing")
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	dev := fs.Bool("dev", false, "reload templates and static files from disk")
	openBrowser := fs.Bool("open", false, "open a browser once listening")
	noWatch := fs.Bool("no-watch", false, "do not index in the background")
	_ = fs.Bool("no-titles", false, "do not generate session titles") // read via givenFlags
	// Titling the whole corpus costs real money. A limit makes a trial run
	// possible -- newest sessions first, so it titles what is worth looking at
	// -- before committing to all eleven hundred.
	titleLimit := fs.Int("titles", 0, "stop after generating N titles")
	titleVia := fs.String("titles-via", titles.BackendAPI, "api or claude")
	fs.Usage = func() { fmt.Fprintf(os.Stderr, usage, index.DefaultPath()) }

	var err error
	switch cmd {
	case "serve":
		_ = fs.Parse(os.Args[2:])
		if s, serr := resolve(givenFlags(fs), dirs, *dbPath, *addr, *titleVia); serr != nil {
			err = serr
		} else {
			err = runServe(s.dbPath, s.addr, s.dirs, *dev, *openBrowser, *noWatch, s.titles)
		}
	case "index":
		_ = fs.Parse(os.Args[2:])
		if s, serr := resolve(givenFlags(fs), dirs, *dbPath, *addr, *titleVia); serr != nil {
			err = serr
		} else {
			err = runIndex(s.dbPath, s.dirs, *full, *lexical, *titleLimit, s.titles)
		}
	case "stats":
		_ = fs.Parse(os.Args[2:])
		err = runStats(configuredDB(givenFlags(fs), *dbPath))
	case "doctor":
		_ = fs.Parse(os.Args[2:])
		err = runDoctor(configuredDB(givenFlags(fs), *dbPath))
	case "info":
		_ = fs.Parse(os.Args[2:])
		err = runInfo(configuredDB(givenFlags(fs), *dbPath), dirs)
	case "version":
		fmt.Println("spireweb", Version)
	default:
		fs.Usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// openIndex opens the index with semantic support when it is available, and
// falls back to lexical-only when it is not.
//
// Falling back rather than failing is deliberate: keyword search over an index
// with no vectors is still useful, and the reasons semantic search is
// unavailable are mostly environmental -- the model is not downloaded, or
// llama.cpp cannot reach a GPU. The fallback is reported, never silent, because
// an index quietly built without vectors looks identical to one with them until
// someone notices the results got worse.
func openIndex(path string, lexical bool) (*index.DB, index.Embedder, error) {
	if err := index.CheckFTS5(); err != nil {
		return nil, nil, err
	}

	if lexical {
		db, _, err := index.OpenOrReset(path, index.DriverName)
		return db, nil, err
	}

	paths := embed.DefaultPaths()
	if err := index.RegisterSemanticDriver(paths); err != nil {
		note("semantic search unavailable: %v", err)
		db, _, err := index.OpenOrReset(path, index.DriverName)
		return db, nil, err
	}

	// A failure here is usually the driver's ConnectHook reporting that the
	// model would not load. Never continue past it on the semantic driver: a
	// connection with no registered model does not return errors from lembed(),
	// it segfaults the process.
	db, reset, err := index.OpenOrReset(path, index.SemanticDriverName)
	if err != nil {
		note("semantic search unavailable: %v", err)
		db, _, err := index.OpenOrReset(path, index.DriverName)
		return db, nil, err
	}
	if reset {
		note("index was written by a different schema version; rebuilding")
	}

	stop := noteSlowModelLoad()
	e, err := embed.New(db.SQL(), paths)
	stop()
	if err != nil {
		db.Close()
		note("semantic search unavailable: %v", err)
		db, _, err := index.OpenOrReset(path, index.DriverName)
		return db, nil, err
	}
	if !e.IsPatchedFork() {
		db.Close()
		return nil, nil, fmt.Errorf(
			"sqlite-lembed %s is the unpatched upstream build, which crashes the process "+
				"on long or malformed input; run 'mise run setup'", e.Version())
	}
	if err := db.EnsureVectorTable(e.Dim()); err != nil {
		db.Close()
		return nil, nil, err
	}
	return db, e, nil
}

func runIndex(dbPath string, dirs []string, full, lexical bool, titleLimit int, titlesVia string) error {
	db, embedder, err := openIndex(dbPath, lexical)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	start := time.Now()
	var lastReport time.Time
	var announcedBackfill, announcedArchive bool
	p, err := index.Build(ctx, db, index.BuildOptions{
		Dirs:     dirs,
		Full:     full,
		Embedder: embedder,
		OnProgress: func(p index.Progress) {
			// Rate-limited because a skipped file takes microseconds and
			// printing every one of them costs more than the indexing does.
			if p.Backfill && !announcedBackfill {
				announcedBackfill = true
				note("index has chunks without embeddings; reindexing in full to add them")
			}
			if p.ArchiveBackfill && !announcedArchive {
				announcedArchive = true
				note("index predates the message archive; reindexing in full to store them " +
					"(this corpus is ~265MB of messages, so expect the database to grow)")
			}
			if !p.Finished && time.Since(lastReport) < 100*time.Millisecond {
				return
			}
			lastReport = time.Now()
			fmt.Printf("\r\033[K%d/%d  indexed %d  skipped %d  failed %d",
				p.Done, p.Total, p.Indexed, p.Skipped, p.Failed)
		},
	})
	fmt.Println()
	if err != nil {
		return err
	}

	if full {
		// A full rebuild leaves a large write-ahead log behind.
		if err := db.Checkpoint(); err != nil {
			return err
		}
	}
	if err := db.Optimize(); err != nil {
		return err
	}

	fmt.Printf("indexed %d sessions (%d chunks, %d messages archived) in %s\n",
		p.Indexed, p.Chunks, p.Messages, time.Since(start).Round(time.Millisecond))
	if p.Excluded > 0 {
		// Named rather than folded into "skipped", which means unchanged. A
		// corpus that is mostly excluded is a surprising thing to discover from
		// a session count that looks too low.
		note("excluded %d files driven through the SDK rather than typed "+
			"(claude-bridge duplicates, and spireweb's own title prompts)", p.Excluded)
	}
	if embedder == nil {
		note("built without embeddings; search will be keyword-only")
	}

	if titlesVia == config.TitlesOff {
		return nil
	}
	return runTitles(ctx, db, titleLimit, titlesVia)
}

// summarizer builds the title generator for the chosen backend.
func summarizer(via string) (titles.Summarizer, error) {
	return titles.NewSummarizer(via)
}

// confirmThreshold is how many sessions make a CLI run worth asking about.
// Below it the pass is quick and cheap enough that a prompt is just friction.
const confirmThreshold = 50

// confirmCLIRun asks before titling a large corpus through the Claude CLI.
//
// Only for that backend, and only when a person is there to answer. Using the
// API means someone deliberately set ANTHROPIC_API_KEY, which is its own
// opt-in; the CLI spends a subscription's rate limit, shared with the
// interactive sessions it is actually for, and AGENTS.md's rule against an
// "auto" backend is the same concern one step earlier.
//
// Not a tty means proceed: the backend was named on the command line or in the
// settings file, and a prompt nobody can answer would hang a cron job or a
// brew service rather than protect anyone.
func confirmCLIRun(ctx context.Context, db *index.DB, via string, limit int) (bool, error) {
	if via != titles.BackendClaude || !isTerminal(os.Stdin) {
		return true, nil
	}
	cands, err := db.TitleCandidates(ctx)
	if err != nil {
		return false, err
	}
	n := len(cands)
	if limit > 0 && limit < n {
		n = limit
	}
	if n < confirmThreshold {
		return true, nil
	}

	// "Up to", because a session whose conversation has not moved much since
	// it was last titled is answered from title_key without a call.
	fmt.Printf("%d sessions to title through the claude CLI: up to %d calls against your\n"+
		"subscription's rate limit, roughly %s. Continue? [y/N] ",
		n, n, (time.Duration(n) * 4500 * time.Millisecond / time.Duration(titles.CLIConcurrency)).Round(time.Minute))

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

// isTerminal reports whether f is a terminal, so that prompts are only asked
// where they can be answered.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// runTitles generates titles for sessions that do not have one.
//
// Separate from the build and after it, so a search index exists whatever
// happens here. Missing configuration is a note rather than an error: titles
// are an improvement to the list, and an index without them is the index this
// tool had for its first five milestones.
func runTitles(ctx context.Context, db *index.DB, limit int, via string) error {
	s, err := summarizer(via)
	if err != nil {
		note("skipping titles: %v", err)
		return nil
	}
	if ok, err := confirmCLIRun(ctx, db, via, limit); err != nil {
		return err
	} else if !ok {
		note("skipping titles; set titles in %s to choose once", config.Path())
		return nil
	}

	start := time.Now()
	var lastReport time.Time
	p, err := titles.Run(ctx, db, titles.Options{
		Summarizer:  s,
		Limit:       limit,
		Concurrency: titles.ConcurrencyFor(s),
		OnProgress: func(p titles.Progress) {
			if p.Total == 0 {
				return
			}
			if !p.Finished && time.Since(lastReport) < 100*time.Millisecond {
				return
			}
			lastReport = time.Now()
			fmt.Printf("\r\033[K%d/%d  titled %d  unchanged %d  failed %d",
				p.Done, p.Total, p.Titled, p.Cached, p.Failed)
		},
	})
	if p.Total > 0 {
		fmt.Println()
	}
	if err != nil {
		return err
	}
	if p.Titled > 0 {
		fmt.Printf("titled %d sessions with %s in %s\n",
			p.Titled, s.Name(), time.Since(start).Round(time.Millisecond))
	}
	if p.Failed > 0 {
		// One reason, not p.Failed of them: when this goes wrong it is almost
		// always one thing wrong with the configuration.
		note("%d of %d sessions could not be summarized (%v); they keep their "+
			"opening message and will be retried", p.Failed, p.Total, p.FirstErr)
	}
	return nil
}

func runStats(dbPath string) error {
	// Prefer the semantic driver: chunks_vec is a vec0 virtual table, and
	// without the extension loaded it is not readable, so a plain reader would
	// report an index full of embeddings as having none.
	driver := index.DriverName
	if err := index.RegisterSemanticDriver(embed.DefaultPaths()); err == nil {
		driver = index.SemanticDriverName
	}
	db, err := index.OpenReader(dbPath, driver)
	if err != nil && driver != index.DriverName {
		db, err = index.OpenReader(dbPath, index.DriverName)
	}
	if err != nil {
		return err
	}
	defer db.Close()

	st, err := db.Stats()
	if err != nil {
		return err
	}
	fmt.Printf("sessions  %d\nchunks    %d\nprojects  %d\n", st.Sessions, st.Chunks, st.Projects)
	if msgs, err := db.CountMessages(context.Background()); err == nil {
		fmt.Printf("messages  %d\n", msgs)
	}
	if titled, err := db.CountTitledSessions(context.Background()); err == nil {
		fmt.Printf("titles    %d\n", titled)
	}
	if st.Vectors < 0 {
		fmt.Println("vectors   none (keyword search only)")
	} else {
		fmt.Printf("vectors   %d\n", st.Vectors)
	}
	if fi, err := os.Stat(dbPath); err == nil {
		fmt.Printf("size      %.1f MB\n", float64(fi.Size())/(1<<20))
	}
	return nil
}

// runDoctor checks the invariants that indexing can silently violate.
//
// chunks_fts is an external-content FTS5 table and chunks_vec is a virtual
// table, and neither participates in foreign-key cascades. Deleting a chunk
// without also deleting its index rows leaves search returning hits that point
// at rows which no longer exist. Nothing detects that at query time, so it has
// to be checked directly -- and it cannot be checked with the sqlite3 CLI,
// because chunks_vec is unreadable without the extension this binary links in.
func runDoctor(dbPath string) error {
	// The extension is needed to read chunks_vec at all, but an index built
	// without embeddings has no such table and is still worth checking.
	driver := index.DriverName
	semantic := false
	if err := index.RegisterSemanticDriver(embed.DefaultPaths()); err == nil {
		driver, semantic = index.SemanticDriverName, true
	} else {
		note("extension unavailable, skipping vector checks: %v", err)
	}

	db, err := index.OpenReader(dbPath, driver)
	if err != nil && semantic {
		// Registering the driver only proves the files are on disk. Opening a
		// connection is where llama.cpp actually loads the model, and it fails
		// on a machine with no reachable GPU -- a sandbox, a headless runner,
		// someone's laptop with a broken install.
		//
		// Falling back rather than returning, which is what this did and what
		// made doctor the one command that refused to run. It is the command
		// someone reaches for *because* something is wrong, so it has to check
		// what it can and say what it could not.
		note("semantic search unavailable, skipping vector checks: %v", err)
		semantic = false
		db, err = index.OpenReader(dbPath, index.DriverName)
	}
	if err != nil {
		return err
	}
	defer db.Close()

	// Which tables exist at all. A lexical-only index has no vector table, and
	// an index that has not been rebuilt since the archive landed has no
	// messages table -- OpenReader creates nothing, by design.
	has := func(table string) bool {
		var n int
		_ = db.SQL().QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n)
		return n > 0
	}
	// chunks_vec is a vec0 virtual table: it appears in sqlite_master whether or
	// not the extension is loaded, but querying it without one errors. So the
	// checks need both that the table exists and that this connection can read
	// it.
	hasVec, hasMessages := semantic && has("chunks_vec"), has("messages")
	if !hasMessages {
		note("no message archive in this index; run 'spireweb index' to build one")
	}

	checks := []struct{ name, sql string }{
		{"vectors with no chunk", `SELECT COUNT(*) FROM chunks_vec v
			WHERE NOT EXISTS (SELECT 1 FROM chunks c WHERE c.id = v.rowid)`},
		{"chunks with no vector", `SELECT COUNT(*) FROM chunks c
			WHERE NOT EXISTS (SELECT 1 FROM chunks_vec v WHERE v.rowid = c.id)`},
		{"chunks with no session", `SELECT COUNT(*) FROM chunks c
			WHERE NOT EXISTS (SELECT 1 FROM sessions s WHERE s.id = c.session_id)`},
		{"fts rows minus chunks", `SELECT ABS((SELECT COUNT(*) FROM chunks_fts) -
			(SELECT COUNT(*) FROM chunks))`},
		// The archive is the part that is not derived from anything else, so a
		// session missing from it is the one inconsistency that cannot be
		// repaired by reindexing alone -- if the file is gone too, it is gone.
		{"sessions not archived", `SELECT COUNT(*) FROM sessions s
			WHERE s.n_msgs > 0
			  AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.session_id = s.id)`},
		{"messages minus n_msgs", `SELECT COUNT(*) FROM sessions s
			WHERE s.n_msgs <> (SELECT COUNT(*) FROM messages m WHERE m.session_id = s.id)`},
		{"messages with no session", `SELECT COUNT(*) FROM messages m
			WHERE NOT EXISTS (SELECT 1 FROM sessions s WHERE s.id = m.session_id)`},
		{"messages that are not json", `SELECT COUNT(*) FROM messages
			WHERE json_valid(content) = 0`},
	}

	bad := 0
	for _, c := range checks {
		switch {
		case !hasVec && strings.Contains(c.sql, "chunks_vec"):
			fmt.Printf("%-26s %7s  skipped (no embeddings)\n", c.name, "-")
			continue
		case !hasMessages && strings.Contains(c.sql, "messages"):
			fmt.Printf("%-26s %7s  skipped (no archive)\n", c.name, "-")
			continue
		}
		var n int
		if err := db.SQL().QueryRow(c.sql).Scan(&n); err != nil {
			return fmt.Errorf("%s: %w", c.name, err)
		}
		status := "ok"
		if n != 0 {
			status = "FAIL"
			bad++
		}
		fmt.Printf("%-26s %7d  %s\n", c.name, n, status)
	}
	if bad > 0 {
		return fmt.Errorf("%d checks failed; rebuild with 'spireweb index --full'", bad)
	}
	return nil
}

// runInfo resolves the session directories itself rather than being handed
// them, because it must not refuse to run when there are none. It is the
// command someone runs precisely because nothing is working, and "no sessions
// found" is the answer it exists to give.
func runInfo(dbPath string, flagged dirList) error {
	paths := embed.DefaultPaths()
	fmt.Printf("spireweb %s (%s %s/%s)\n\n", Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)

	// The same precedence the indexing commands apply, so that this reports
	// what would actually happen rather than a second opinion about it. A
	// malformed settings file is shown rather than raised: it is the thing
	// being diagnosed.
	cfg, hadFile, cfgErr := config.Load()
	dirs, from := chooseDirs(flagged, cfg)

	sessions := strings.Join(dirs, "\n           ") + "  (" + from + ")"
	if len(dirs) == 0 {
		sessions = "none found; looked in\n           " +
			strings.Join(session.Candidates(), "\n           ")
	}

	settingsLine := config.Path()
	switch {
	case cfgErr != nil:
		settingsLine += "  (unreadable: " + cfgErr.Error() + ")"
	case !hadFile:
		settingsLine += "  (not written yet)"
	}

	fmt.Printf("sessions   %s\nsettings   %s\nindex      %s\nextension  %s\nmodel      %s\n\n",
		sessions, settingsLine, dbPath, paths.Extension, paths.Model)

	if err := index.CheckFTS5(); err != nil {
		fmt.Println("fts5       missing:", err)
	} else {
		fmt.Println("fts5       available")
	}
	if err := paths.Check(); err != nil {
		fmt.Println("semantic   unavailable:", err)
		return nil
	}
	if err := index.RegisterSemanticDriver(paths); err != nil {
		fmt.Println("semantic   unavailable:", err)
		return nil
	}
	// Opening is the only honest test: the files can be present and the backend
	// still unable to load the model.
	stop := noteSlowModelLoad()
	probe, err := index.Open(dbPath, index.SemanticDriverName)
	stop()
	if err != nil {
		fmt.Println("semantic   unavailable:", err)
		return nil
	}
	probe.Close()
	fmt.Printf("semantic   available (%s, %d dims)\n", embed.ModelName, embed.Dim)
	return nil
}

// dirList collects a repeatable --dir flag.
//
// Repeatable rather than comma-separated because these are paths, and a path
// may contain a comma. Empty until the flag is given at least once, so that a
// caller can tell "not specified" from "specified as the default" and fall back
// to its own answer.
type dirList []string

func (d *dirList) String() string { return strings.Join(*d, ", ") }

func (d *dirList) Set(v string) error {
	if v == "" {
		return errors.New("empty path")
	}
	*d = append(*d, v)
	return nil
}

// settings is the resolved configuration for one run.
type settings struct {
	dirs   []string
	dbPath string
	addr   string

	// titles is a config.Titles* value. Off is spelled out rather than left
	// absent, because "decided not to" and "has not been asked" differ.
	titles string
}

// resolve applies the precedence: a flag beats the settings file, which beats
// working it out.
//
// Whether a flag was *given* is the question, not whether it differs from its
// default, so this takes the set of flags fs actually saw. Otherwise --addr
// with the default value would be indistinguishable from no --addr, and could
// not override a config that says otherwise.
//
// A first run with nothing configured writes down what it worked out. That is
// the point of the file: detection answers "what is on this machine" afresh
// every time, and the answer changes -- install pi to try it once and the
// corpus silently doubles, move ~/.claude and the index empties with no
// explanation. Written down, it is something a person can read and edit.
func resolve(given map[string]bool, flagged dirList, dbPath, addr, titleVia string) (settings, error) {
	cfg, hadFile, err := config.Load()
	if err != nil {
		// Named rather than ignored: falling back to detection would quietly
		// disregard what someone wrote.
		return settings{}, err
	}

	s := settings{dbPath: dbPath, addr: addr}
	if !given["db"] && cfg.Index != "" {
		s.dbPath = cfg.Index
	}
	if !given["addr"] && cfg.Addr != "" {
		s.addr = cfg.Addr
	}

	switch {
	case given["no-titles"]:
		s.titles = config.TitlesOff
	case given["titles-via"]:
		s.titles = titleVia
	case cfg.Titles != "":
		s.titles = cfg.Titles
	default:
		s.titles = config.TitlesAPI
	}

	var from string
	s.dirs, from = chooseDirs(flagged, cfg)
	if len(s.dirs) == 0 {
		// Finding nothing is worth an error rather than an empty interface.
		// Name everywhere that was looked: the usual cause is sessions living
		// somewhere this does not know about, and --dir is then the answer.
		return settings{}, fmt.Errorf(
			"no agent sessions found. Looked in:\n  %s\nUse --dir to name one",
			strings.Join(session.Candidates(), "\n  "))
	}

	if from == sourceDetected && !hadFile {
		cfg.Dirs = s.dirs
		if cfg.Titles == "" {
			// What spireweb does today, written down rather than changed. A
			// first run must not quietly turn titles off for someone who has
			// ANTHROPIC_API_KEY set and has been getting them all along; the
			// comments in the file are what tell a Claude Code user that
			// "claude" is the setting for them.
			cfg.Titles = config.TitlesAPI
		}
		if err := cfg.Save(); err != nil {
			// Not fatal: spireweb works perfectly well without being able to
			// write its settings, it just works them out again next time.
			note("could not write %s: %v", config.Path(), err)
		} else {
			note("found %s\nwrote %s", strings.Join(s.dirs, ", "), config.Path())
		}
	}
	return s, nil
}

// Where a setting came from, for reporting.
const (
	sourceFlag     = "--dir"
	sourceConfig   = "config"
	sourceDetected = "detected"
)

// chooseDirs applies the precedence and says which rule won. Returns no
// directories when there is nothing anywhere, which the caller turns into
// either an error or a message, depending on whether it is about to index.
func chooseDirs(flagged dirList, cfg config.Config) ([]string, string) {
	switch {
	case len(flagged) > 0:
		return flagged, sourceFlag
	case len(cfg.Dirs) > 0:
		return cfg.Dirs, sourceConfig
	default:
		return session.Detect(), sourceDetected
	}
}

// givenFlags reports which flags were actually passed, as opposed to left at
// their default.
func givenFlags(fs *flag.FlagSet) map[string]bool {
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	return given
}

func note(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "note: "+format+"\n", args...)
}

// configuredDB resolves only the index location, for the commands that read an
// existing index and index nothing.
//
// They must not resolve directories: stats and doctor work on what is already
// there, and info exists to explain a machine where nothing is set up. Making
// any of them fail because no sessions were found would be backwards.
func configuredDB(given map[string]bool, flagged string) string {
	if given["db"] {
		return flagged
	}
	if cfg, _, err := config.Load(); err == nil && cfg.Index != "" {
		return cfg.Index
	}
	return flagged
}

// slowNote prints a message if whatever follows has not finished within d,
// and returns the function that cancels it.
//
// For the one operation here that is slow without looking like it should be:
// the first connection through the semantic driver makes llama.cpp compile its
// Metal shaders, which takes about fifteen seconds. macOS caches the result,
// so the next run is instant -- but the cache lives under /var/folders and is
// evicted on the system's own schedule, so this is not a cost paid once.
//
// Printed only when it actually happens rather than before every load, because
// the warm case is the overwhelmingly common one and a warning that is almost
// always wrong is worse than no warning.
func slowNote(d time.Duration, format string, args ...any) (stop func()) {
	done := make(chan struct{})
	go func() {
		select {
		case <-done:
		case <-time.After(d):
			note(format, args...)
		}
	}()
	return func() { close(done) }
}

// noteSlowModelLoad is slowNote with the message this exists for.
func noteSlowModelLoad() (stop func()) {
	return slowNote(2*time.Second,
		"loading the embedding model; llama.cpp is compiling Metal shaders, "+
			"which takes about 15s the first time and is then cached")
}
