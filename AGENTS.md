# AGENTS.md

Non-obvious things about this repository. Everything here cost somebody an hour.

## Build

```bash
mise run setup    # once: embedding model + sqlite-lembed (~30s)
mise run check    # vet, lint, typecheck, gofmt, test -- run before committing
mise run dev      # server + tsc, both watching
```

`GOFLAGS=-tags=sqlite_fts5` lives in `mise.toml`'s `[env]`. mattn/go-sqlite3
omits FTS5 without it and the schema fails at runtime with "no such module:
fts5". Because it is in the environment, plain `go test` works too.

`pnpm` uses mise's npm backend; the default aqua backend fails pnpm's GitHub
attestation check. Its version is pinned equal to `package.json`'s
`packageManager`, or pnpm re-downloads that version on every invocation.

`mise run test` depends on `ts`: the web package embeds
`internal/web/static`, and `app.js` is generated, not committed.

CI is Linux and has no GPU, so the semantic tests skip -- see below.

## sqlite3.h

`sqlite-vec`'s cgo bindings compile with `-DSQLITE_CORE` and `#include
"sqlite3.h"`, taking struct layouts from whatever header the preprocessor
finds. That is not the library being linked:

| | |
| --- | --- |
| macOS SDK | 3.51.0 |
| debian bookworm | 3.40.1 |
| **actually linked** | **3.53.4** (go-sqlite3's amalgamation) |

Older header against newer library is the direction SQLite supports, so this
worked -- but the header was an unpinned input that varied per machine, and
that class of mismatch corrupts structs rather than failing to compile.

`mise run sqlite-header` copies the header go-sqlite3 ships for its own
amalgamation into `third_party/sqlite/`, and `CGO_CFLAGS` points there. It is
derived from `go.mod`, so bumping go-sqlite3 restages it. `build`, `test`, and
`lint` depend on it, and a test asserts the staged header's version equals
`sqlite_version()`. Nothing needs `libsqlite3-dev`, including CI.

## sqlite-lembed

Must be the **landrix fork** (`mise run setup` verifies). Upstream
v0.0.1-alpha.8 kills the process rather than returning errors: SIGSEGV on input
over 512 tokens, SIGABRT on invalid UTF-8.

**A connection whose model failed to register will segfault, not error.**
`lembed_model_from_file` returns null when llama.cpp cannot load the model, the
vtab rejects it with a bare "SQL logic error", and a later `lembed()` call
dereferences the null model. A crash inside cgo cannot be recovered. So a
registration failure must always fall back to lexical-only; never continue on
that connection.

The usual cause of that failure is no reachable GPU, not a bad file. On macOS
llama.cpp needs a Metal device, which a sandbox or headless CI runner lacks.
The extension still loads and `lembed_version()` still answers, so the only
honest test of "is semantic search working" is opening a connection.

## Connections

- **Writer: one connection** (`SetMaxOpenConns(1)`). lembed's `llama_context`
  is not safe for concurrent use; two goroutines embedding through the *same*
  connection segfault. Uses `_txlock=immediate`, because indexing reads before
  it writes and lock upgrades fail rather than wait.
- **Concurrent embedding on *different* connections is fine.** Each has its
  own model and context. This matters because the background indexer embeds
  chunks while handlers embed queries; there is a test that runs seven
  connections at once, and no process-wide lock is needed.
- **Each context costs ~30MB.** `serve` is 151MB idle and ~270MB once the
  reader pool is open, almost all of it the five loaded models. Raising
  `ReaderConns` costs 30MB a connection, which is the reason it is 4 and not
  something larger.
- **Readers: a pool**, `_query_only=true` — *not* `mode=ro`, which cannot
  create the `-shm`/`-wal` files WAL needs.
- **The model is registered per connection**, via the driver's `ConnectHook`.
  It lives in `temp.lembed_models`, which is connection-scoped, and
  `database/sql` recycles connections whenever it likes. Registering once after
  opening a pool leaves later connections modelless, and the symptom is silent:
  the ranker errors, gets skipped, and search degrades to keyword-only.

## Data

- `chunks.id` is `AUTOINCREMENT`. A plain `INTEGER PRIMARY KEY` reuses rowids
  freed by `DELETE`, and `chunks_vec` is keyed by chunk id, so a recycled id
  collides with a surviving vector.
- `chunks_fts` (external content) and `chunks_vec` (virtual table) do not
  participate in foreign-key cascades. Deleting a chunk must delete from both
  by hand, or search returns hits pointing at rows that are gone.
- Never `DELETE FROM sessions` to update one: `chunks.session_id` cascades and
  destroys the chunks the reindex meant to reuse. Upsert.
- `sessions.title` is excluded from the upsert's UPDATE list, so a reindex does
  not clobber a generated title. Sessions are append-only and get reindexed on
  every new message.
- Identity is `sessions.id`, a UUID from pi's header. `path` and `host` are
  machine-local metadata, so indexes from different machines can be merged.
- Chunks must stay under `session.MaxTokens`. Character limits cannot predict
  this: 800 chars of prose is 162 tokens, 800 chars of dense JSON is 802.
- `Chunk` splits only on rune boundaries. Invalid UTF-8 crashed the extension.

After changing anything about indexing, both of these must return 0:

```sql
SELECT COUNT(*) FROM chunks_vec v
  WHERE NOT EXISTS (SELECT 1 FROM chunks c WHERE c.id = v.rowid);
SELECT COUNT(*) FROM chunks c
  WHERE NOT EXISTS (SELECT 1 FROM chunks_vec v WHERE v.rowid = c.id);
```

## Live indexing

`serve` opens a second, writing handle and runs a catch-up build followed by
an fsnotify watcher. The catch-up finishes before the watcher starts: both
write, through one connection, and overlapping them is the same-connection
case above.

Events are coalesced after a 2s lull, because pi writes once per message.

The header polls `/status`, which **replaces itself**, so the server picks the
next interval (2s busy, 10s idle) rather than the page choosing once at load.
The page seeds the poll with the session count it rendered with, which is how
"3 new sessions" works without the server tracking per-client state.

Rankers are chosen once, at startup, so nothing there may depend on index
*contents*: a server started against an empty index would otherwise stay
keyword-only for its whole life. The semantic ranker is attached whenever the
model loads, and returns nothing until vectors exist.

## Titles

`sessions.title` is written by `internal/titles`, a pass that runs *after* a
build and never during one: both write, and the writer is one connection. It
needs `ANTHROPIC_API_KEY`; without it the pass is skipped with a note, and
every row falls back to its opening message, which is what the list did for
five milestones. `ANTHROPIC_BASE_URL` points it at a fake or a gateway.

Two columns decide whether a session is paid for again, and they guard
different costs:

- `title_msgs` guards the **parse**. It is `n_msgs` as of the last time the
  pass looked. Without it, every run would parse all 1128 files to discover
  that nothing moved.
- `title_key` guards the **API call**. It hashes a bounded prefix of the
  conversation -- and that prefix is quantized to `keySteps`, so the hash moves
  when a session roughly doubles rather than on every message. Sessions are
  append-only and get reindexed per message; hashing the prose directly would
  re-summarize a live session continuously, which is the whole failure this is
  built to avoid.

A failure writes **neither**, so the row keeps its fallback and the next run
retries it. `MarkTitleChecked` writes only `title_msgs`, which is how a session
with no prose at all settles without ever getting a title.

Both columns are added by `migrate()` (`ALTER TABLE ADD COLUMN`), not by
`schema`. Bumping `SchemaVersion` would have discarded the database and
re-embedded 46k chunks to gain two nullable columns.

`--titles N` caps a run. The corpus is on the order of a dollar all at once, so
a trial run over the newest few is worth having.

## Rendering

Transcripts are parsed from the `.jsonl` on demand, not stored. The DB is a
search index; session files are the source of truth. Prose is 0.3–2.6% of a
large session's bytes -- a 7MB session is well under 100KB of conversation --
so tool output is fetched lazily and nothing needs pagination.

goldmark runs with raw HTML **disabled**. Session content is arbitrary text
that routinely contains HTML and JavaScript.

A `toolResult` carries a per-tool `details` object, and an edit's holds the
diff pi drew in the terminal -- markers, file line numbers, context, `...`
gaps -- under `details.diff`. Render that rather than computing one: the call's
arguments hold only the old and new text, so a diff derived from them could
never say *where* in the file the edit landed, and the line numbers are most of
what makes a diff readable. When a diff is present the arguments are hidden,
because they are the same edit as escaped JSON. A failed edit has `details:
{}`, and there the arguments are the useful part: the text that was not found.

## Keyboard

The cursor is real DOM focus on a row's `<a>`, not a class we track. Focus
gives scroll-into-view, Enter-to-activate, and screen reader support for
free. Consequences: rows must be anchors with an `href`, and `.row:focus`
rather than `:focus-visible` does the styling, because a programmatic
`focus()` does not always count as keyboard-initiated.

`app.ts` reaches the page through selectors, which nothing type-checks.
`markup_test.go` pins that contract; it cannot tell whether `j` *works*, which
is what `e2e/smoke.spec.ts` is for. Both are worth having: the Go test says
which selector broke, in the suite that runs on every commit.

## Tests

`mise run check` is ~20s. Nearly all of it is `internal/index` and
`internal/indexer`: the watcher tests wait out a 2s settle timer, and each
semantic test loads the model. The other five packages total under 2s.

Semantic tests skip when the backend cannot actually run, not when its files
are missing -- see the GPU note above.

## Browser tests

`mise run e2e` drives Chromium through Playwright. Not part of `mise run
check`: it is ~7s against check's ~20s but needs a browser that is not a
repository dependency, and a failure there is a different kind of signal.

`mise run e2e-install` fetches that browser, once. The `@playwright/test`
version is pinned to `~1.58`, whose browser revision is **chromium-1208**,
because that build was already in the shared cache -- a minor bump is a 130MB
download, so it is worth knowing that is what changed. Playwright says what to
run when the build is missing.

Playwright starts the server itself, through `e2e/serve.sh`, which compiles
`app.js` (embedded, generated, so a stale one means testing the previous
keyboard handling), builds the binary, and indexes `e2e/sessions` into a temp
database. The fixtures are committed for a reason: the tests assert which
session is newest and how many rows a filter leaves, and neither survives
contact with a real corpus. `e2e-charlie` is 41 messages with one occurrence of
"quicksand" at the end, so the scroll-to-match test starts below the fold.

These tests are only worth their weight if they fail when the behaviour breaks,
which is worth re-checking after editing them: disabling `scrollToMatch()` and
renaming the `j` case both produce failures.

## Changing GitHub Actions

Run `aver` after editing a workflow; it reports outdated action versions.
`actionlint` (via docker) catches syntax errors.

## Corpus

1128 files, 258MB, median 95KB, p90 567KB, max 7.2MB; 46k chunks. Useful for
judging whether an approach scales. Lexical index build: ~4s cold, ~55ms when
one session changed.
