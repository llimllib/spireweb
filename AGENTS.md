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
attestation check.

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
  is not safe for concurrent use; two goroutines embedding at once segfault.
  Uses `_txlock=immediate`, because indexing reads before it writes and lock
  upgrades fail rather than wait.
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

## Rendering

Transcripts are parsed from the `.jsonl` on demand, not stored. The DB is a
search index; session files are the source of truth. Prose is 0.3–2.6% of a
large session's bytes -- a 7MB session is well under 100KB of conversation --
so tool output is fetched lazily and nothing needs pagination.

goldmark runs with raw HTML **disabled**. Session content is arbitrary text
that routinely contains HTML and JavaScript.

## Corpus

1128 files, 258MB, median 95KB, p90 567KB, max 7.2MB; 46k chunks. Useful for
judging whether an approach scales. Lexical index build: ~4s cold, ~55ms when
one session changed.
