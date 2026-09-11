# spireweb

A web interface for searching and reading [pi](https://github.com/badlogic/pi-mono) agent sessions.

Local semantic search via [sqlite-vec](https://github.com/asg017/sqlite-vec) and
[sqlite-lembed](https://github.com/landrix/sqlite-lembed/), combined with FTS5
keyword search by reciprocal rank fusion.

## Getting started

```bash
mise run setup    # embedding model + sqlite-lembed, once
mise run index    # build the search index
mise run dev      # http://localhost:8080
```

Set `ANTHROPIC_API_KEY` to have sessions titled by an LLM as they are indexed.
Without it, a session is listed under its opening message.

## Status

Under construction; see the [milestones](https://github.com/llimllib/spireweb/milestones).
Browsing, search, live indexing, and titles work.
