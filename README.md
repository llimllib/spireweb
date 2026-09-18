# spireweb

A web interface for searching and reading agent sessions, from
[pi](https://github.com/badlogic/pi-mono) and from
[Claude Code](https://claude.com/claude-code).

Local semantic search via [sqlite-vec](https://github.com/asg017/sqlite-vec) and
[sqlite-lembed](https://github.com/landrix/sqlite-lembed/), combined with FTS5
keyword search by reciprocal rank fusion.

## Install

```bash
brew install llimllib/tap/spireweb
spireweb serve
```

macOS on Apple Silicon. The cask carries the embedding model and the search
extension alongside the binary, so nothing else is downloaded.

`spireweb` finds your sessions itself, looking in `$CLAUDE_CONFIG_DIR/projects`,
`~/.config/claude/projects`, `~/.claude/projects`, and `~/.pi/agent/sessions`,
and indexes everything it finds. What it found is written to
`~/.config/spireweb/config.toml` on the first run, so that installing another
agent later does not silently change what is indexed. Edit that file, or pass
`--dir` (repeatable), to choose differently.

The first search after a while takes about fifteen seconds while macOS compiles
the embedding model's Metal shaders. It says so when it happens.

## Titles

Sessions are listed under a generated title, falling back to their opening
message. Two backends, set with `titles` in the config file or `--titles-via`:

- `api` needs `ANTHROPIC_API_KEY`. The default.
- `claude` shells out to the Claude Code CLI, which bills whatever subscription
  it is signed in to and shares a rate limit with your interactive sessions. A
  run over a large corpus asks first. `--titles N` caps it, for trying it out.

`titles = "off"` lists every session under its opening message, which is what
spireweb did for its first five milestones.

## Working on it

```bash
mise run setup    # embedding model + sqlite-lembed, once
mise run index    # build the search index
mise run dev      # http://localhost:8080
mise run check    # vet, lint, typecheck, gofmt, test
```

See [AGENTS.md](AGENTS.md) for the things that cost somebody an hour.

## Status

Under construction; see the [milestones](https://github.com/llimllib/spireweb/milestones).
Browsing, search, live indexing, titles, and both session formats work.
