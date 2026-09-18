# AGENTS.md

Non-obvious things about this repository. Everything here cost somebody an hour.

## Build

```bash
mise run setup    # once: embedding model + sqlite-lembed (~1 min)
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

`setup` deletes its build tree when it finishes. Building the fork leaves
473MB behind -- 345MB of it the llama.cpp submodule's git objects, which
`--depth 1` barely dents -- to produce a 3.2MB dylib, and a rebuild from
nothing is a minute. `SPIREWEB_KEEP_SRC=1` keeps it for working on the
extension itself.

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

## Two session formats

`internal/session` parses pi's files and Claude Code's, told apart by the first
line: pi opens with a `{"type":"session"}` header, Claude Code has no header at
all. Detection is **per file**, not per directory, so one `--dir` may hold
either. Everything downstream consumes `*Session` and never sees bytes, which
is what keeps the difference to that one package.

Claude Code's shape differs in ways that are not a remapping:

- **Tool results have no role.** They are `user` records carrying `tool_result`
  blocks, and one record may carry several -- that is what parallel tool calls
  look like coming back. `Message.ToolCallID` is singular, so one record fans
  out into N messages, or `ToolResults()` finds one call's output and the rest
  render as missing.
- **Every fanned-out message carries the same `Raw`.** `archiveMessages` uses
  `COUNT(*)` as its resume point, so a message with no `Raw` leaves a gap that
  makes the count disagree with the message list on every later build.
- **`Raw` is the whole record**, unlike pi's, where it is the message.
  `toolUseResult` sits on the envelope and holds the `structuredPatch` an edit
  produced, so keeping only the message would make the archive lossy in exactly
  the way it exists to prevent.
- `message.content` is a bare string on older records, an array on newer ones.
- One assistant turn spans several records, so `n_msgs` is not comparable
  between agents.

`render.ParseDiff` takes either: pi's rendered `details.diff`, whose line
numbers are parsed back out of the text, or Claude Code's `structuredPatch`,
whose numbers come from walking the hunk. Tool names are normalized before the
`summaryArg` lookup -- Claude Code capitalizes its built-ins, and MCP tools
arrive as `mcp__<server>__<tool>`.

## SDK sessions are excluded

Claude Code writes a session file for **every SDK invocation**, not just for
what a person types. On the corpus that was 1600 of 1617 files and 283 of
284MB:

| entrypoint | files | what it is |
| --- | --- | --- |
| `sdk-cli` in `spireweb-titles-*` | 1215 | spireweb's own title prompts |
| any `sdk-ts` | 252 | pi tunnelling through claude-bridge |
| `cli` | **17** | someone typing |

The bridge files duplicate the pi corpus, with the worse copy. The title files
are a **feedback loop**: the titles pass shells out to `claude -p`, which writes
a session into the directory being indexed, which gets indexed and titled, which
writes another.

`session.SkipReason` filters on `entrypoint`, which is present on every
message-bearing record across every version seen. **Whole file, not a prefix**:
94 bridge sessions open with one or two `cli` records before the bridge takes
over, the deepest at record 429. Matched as bytes rather than parsed, because
it runs over every candidate on a cold build.

It is checked in `Build` *after* the mtime test, so an unchanged indexed session
still costs a stat. The consequence is that a session indexed before it became
excluded survives until `--full`; excluded files are deliberately not marked
`seen`, so the deletion sweep is what removes them.

## Directories and config

`BuildOptions.Dirs` is a slice. It cannot be one `Build` per directory:
`build.go` removes every indexed session it did not see, so two builds would
have each root's sweep delete the other's. `Discover` takes them all and
returns one deduplicated list.

With no `--dir`, `cmd` probes `$CLAUDE_CONFIG_DIR/projects`,
`~/.config/claude/projects`, `~/.claude/projects`, `~/.pi/agent/sessions` and
keeps those holding at least one `.jsonl`. Existence is not the test:
`~/.claude` survives as a home for settings after `CLAUDE_CONFIG_DIR` has moved
everything else. The candidates are deduplicated because they genuinely
overlap -- `CLAUDE_CONFIG_DIR` is usually `~/.config/claude`.

`index.Build` still falls back to pi's directory alone. Only `cmd` detects,
because a test indexing a fixture must not depend on the machine running it.

Precedence is flag, then `~/.config/spireweb/config.toml`, then detection, and
**"was the flag given"** is the question rather than "does it differ from its
default" -- otherwise `--addr` with the default value could not override a file.
`resolve` is handed the set of flags the FlagSet saw.

A first run writes down what it detected, because detection's answer moves:
install pi to try it once and the corpus doubles. It records `titles = "api"`,
which is what spireweb already did -- writing `off` would quietly stop titling
for someone who has a key and has been getting them.

XDG, not `~/Library/Application Support`, matching `SPIREWEB_DATA_DIR` and
`embed.DefaultPaths`.

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

## The archive

`messages` holds every message of every session as its agent wrote it, and is
the one table that is not derived from something else: chunks, vectors, and titles can
all be rebuilt from it, and it cannot be rebuilt from them. `spireweb doctor`
checks it separately for that reason.

`content` is JSON **text, verbatim and uncompressed**, and both halves are
load-bearing. Verbatim because the corpus already contains a `bashExecution`
role nothing in this repo handles, so re-marshalling `session.Message` would
silently drop fields. Uncompressed because tool calls and their arguments live
in there, and `json_each` over them is the point of having an archive at all;
compressing `toolResult` would save ~150MB and cost that.

The key is `(session_id, idx)` -- a UUID from pi and a position in an
append-only file. Both survive a merge, which no autoincrement does.

`session.ParseWithRaw` is what fills it. Plain `Parse` does not keep raw bytes,
because they are a second copy of the file and the server caches eight parsed
sessions at a time.

It costs, measured on the corpus:

| | before | after |
| --- | --- | --- |
| lexical index | ~30MB | **424MB** |
| cold build | ~5s | ~9.5s |
| one changed session | ~55ms | ~140ms |

Writes are incremental the same way chunk reuse is: the stored count is the
starting point, so a session that gained a message inserts one row. An index
built before the table existed is backfilled by promoting the run to a full
pass, exactly as `needsVectors` does, because mtime and size will never change
again on an old session.

`index --full` is cheaper than it sounds and costs nothing in money. The
session upsert's `DO UPDATE` list omits `title`, `title_msgs` and `title_key`,
and `TitleCandidates` keys on `title_msgs <> n_msgs` -- so a reindex of an
unchanged file re-titles nothing. `reusableChunks` matches on chunk *content*,
not on mtime, so unchanged chunks keep their row ids and their vectors and
nothing is re-embedded. `--full` only bypasses the mtime skip.

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

A new project directory gets a watch **and a sweep of what is already in it**.
Creating the directory and writing the first session into it are two operations
milliseconds apart, so the session that caused the directory to appear is
exactly the one the watch would miss.

**`Build` and `Watcher.reindex` are two paths over the same decision.** The
watcher does not call `Build`; it parses and upserts the changed files itself.
So any rule about *which* sessions get indexed has to be written twice, and the
watcher is the path that matters more -- it is the one running while an agent
writes. The SDK filter shipped in `Build` alone and leaked 16 rows into a live
index within a day, because the titles pass writes the very files it excludes
*while the server is up*. They share `upsertSession`, so anything about how a
session is indexed is safe; anything about whether it is indexed is not.

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
build and never during one: both write, and the writer is one connection.
Without a backend the pass is skipped with a note and every row falls back to
its opening message, which is what the list did for five milestones.

Two backends, chosen with `--titles-via`:

- `api` needs `ANTHROPIC_API_KEY`, and `ANTHROPIC_BASE_URL` points it at a fake
  or a gateway. ~1s a call.
- `claude` shells out to `claude -p`, which bills whatever authentication
  Claude Code has, including a Pro or Max subscription. ~4.5s a call, nearly
  all of it starting Node, so `CLIConcurrency` is 4 rather than 8 -- and its
  rate limit is shared with the interactive sessions the subscription is for.

There is no `auto`. Picking the CLI because no API key was set would spend a
subscription's rate limit on a thousand sessions without being asked; the
missing-key note names the flag instead.

The CLI runs in an empty temp directory with `--strict-mcp-config
--setting-sources ""`. In a project it discovers CLAUDE.md, settings and
plugins, all to write eight words.

The instructions are repeated **after** the transcript, which is fenced on both
sides. Everything is one message there, the slice is cut at 10k characters, and
without the closing half a transcript ending mid-sentence drew *"Your message
cuts off mid-sentence. Could you complete the question?"* as a title. Passing
them via `--system-prompt` is worse still: it replaces Claude Code's own, and
the model answers the transcript instead of titling it.

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

## Query syntax

Double quotes mean a phrase, and a phrase **excludes**: a session without it
does not appear, however any ranker scores it. Demotion was the alternative and
is worse in the case that matters -- if a phrase only sorted matches upward,
a page of plausible results would look the same whether or not the corpus
contained the phrase, so "did I ever discuss exactly this?" would have no
observable answer. Empty is the answer, which is why `emptyNote` names the
phrase and says to drop the quotes.

Unquoted words stay ranking hints. One FTS5 expression does both:

    P AND (P OR w1 OR w2)

The AND-ed part constrains; the OR-ed group exists only so the bare words reach
BM25, which scores every phrase in the expression it is handed. Drop the second
group and a mixed query ranks as though the unquoted words were never typed.

**The lexical ranker is not enough on its own.** The semantic ranker has no
notion of a phrase -- it embeds the text and returns neighbours -- so it will
happily contribute sessions containing none of the words. `sessionsMatching`
filters the fused results, and without it quoting visibly fails to do the one
thing it promises. There is a test with a deliberately ignorant ranker for
exactly this.

Three things that are true and worth not rediscovering:

- Exact means exact **modulo stemming**: the tokenizer is `porter`, so
  `"deploying pipelines"` matches "deploy pipeline". Byte-exact needs a second
  unstemmed index over 46k chunks.
- A phrase spanning a chunk boundary is invisible, chunks being 800 characters.
- Single-character tokens are dropped from bare words and **kept inside
  phrases**. `"is a weird choice to"` matches nothing if the `a` is dropped.
- An unterminated quote is a phrase in progress, not an error. Search runs on
  every keystroke, so `"deploy pip` is a state the parser has to hold an
  opinion about.

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

A watcher test asserting a file was **not** indexed has to wait out a second
settle first. "Not indexed yet" and "never indexed" look identical, so an
assertion made immediately after some other file appears passes whether or not
the code works -- one did, once, before the sleep was added.

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

## Releasing

`mise run release`, from a `v*` tag, through goreleaser. **darwin/arm64 only**:
go-sqlite3, the sqlite-vec bindings and FTS5 all need cgo, so the
`CGO_ENABLED=0` cross-compilation other repositories here use is not available.
Restricting to one architecture removes the problem instead -- a macOS runner is
already arm64.

The entry point is `mise run release` and not bare `goreleaser`, because
`CGO_CFLAGS` comes from `mise.toml`'s `[env]`. A preflight hook fails the build
without it rather than compiling against whichever `sqlite3.h` the machine has.
`before` hooks also run `sqlite-header` and `ts`; a missing `app.js` is embedded
silently and ships the previous release's keyboard handling.

The archive carries `lembed0.dylib` and the model beside the binary, and
`embed.DefaultPaths` resolves the running executable and looks in its own
directory. That is what makes both an unpacked tarball and the cask work:
Homebrew puts only `spireweb` on PATH and leaves the rest in the Caskroom, so
resolving *through* the symlink is what finds them.

**Homebrew quarantines cask artifacts by default** -- that is what
`--no-quarantine` overrides -- and nothing shipped is signed by a Developer ID.
Both files are refused, and differently: the binary dies with `Killed: 9` behind
an "Apple could not verify" dialog, while the dylib fails `dlopen` and SQLite
retries with the suffix appended, so the error names `lembed0.dylib.dylib` and
says "no such file" about a file that is right there. The result is a working
spireweb with semantic search silently gone. The `postflight_steps` + `xattr -dr`
in `.goreleaser.yaml` clears both; notarization would be the real fix.

Pushing the cask needs `HOMEBREW_TAP_TOKEN`, a PAT secret on this repo.
`GITHUB_TOKEN` cannot push to the tap.

## Metal shaders

The first connection through the semantic driver makes llama.cpp compile its
Metal shaders: **about 15 seconds**, and it is not paid once. macOS caches the
result under `/var/folders/.../C/com.apple.metal` and evicts it on its own
schedule; there were three generations a week apart on one machine. It is
caused by `-DGGML_METAL_EMBED_LIBRARY=ON`, which is also what makes the dylib
relocatable and therefore shippable.

`noteSlowModelLoad` says so after a two second grace rather than before every
load, because the warm case is overwhelmingly common. Removing the cost rather
than narrating it means building with the flag off and shipping a precompiled
`.metallib` -- unmeasured, and it trades away the single-file property.

## Changing GitHub Actions

Run `aver` after editing a workflow; it reports outdated action versions.
`actionlint` (via docker) catches syntax errors.

## Corpus

**pi**: 1190 files, 258MB, median 95KB, p90 567KB, max 7.2MB; 48k chunks.
Lexical index build ~9.5s cold with the archive, ~55ms when one session
changed. Useful for judging whether an approach scales.

**Claude Code**: 1617 files, 284MB -- but only **17 files and 1MB** of it is
conversation this repository does not already have. The rest is excluded as SDK
output, so it is worthless for judging scale and actively misleading for judging
quality. Almost everything done through Claude Code on this machine arrives via
claude-bridge and is therefore a duplicate of a pi session. Anything that needs
a real Claude Code corpus needs someone else's.
