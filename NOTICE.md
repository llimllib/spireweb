# Third-party components

spireweb is MIT-licensed; see `LICENSE`. This file covers the third-party work
it redistributes: two files that the release archive ships alongside the
`spireweb` binary, and one library compiled into the binary itself.

## lembed0.dylib

The [sqlite-lembed](https://github.com/landrix/sqlite-lembed) SQLite extension,
built from the landrix fork, statically linked against
[llama.cpp](https://github.com/ggml-org/llama.cpp).

- sqlite-lembed — MIT, © Alex Garcia
- llama.cpp — MIT, © Georgi Gerganov and contributors

The fork rather than upstream because upstream's v0.0.1-alpha.8 kills the host
process rather than returning an error on input over 512 tokens or on invalid
UTF-8. See `mise-tasks/setup`.

## all-MiniLM-L6-v2.Q8_0.gguf

[all-MiniLM-L6-v2](https://huggingface.co/sentence-transformers/all-MiniLM-L6-v2),
quantised to Q8_0 and converted to GGUF by
[leliuga](https://huggingface.co/leliuga/all-MiniLM-L6-v2-GGUF).

- Apache-2.0

## sqlite-vec

[sqlite-vec](https://github.com/asg017/sqlite-vec) provides the `vec0` virtual
table the embeddings live in. Unlike the two above it is not a visible file: the
cgo bindings compile `sqlite-vec.c` into the `spireweb` binary itself, which is
exactly why it needs saying here.

- sqlite-vec, and its
  [Go bindings](https://github.com/asg017/sqlite-vec-go-bindings) — Apache-2.0,
  © Alex Garcia

Full text at <https://www.apache.org/licenses/LICENSE-2.0>. Apache-2.0 asks that
its notices travel with redistributions, and MIT on spireweb's own code does not
change that: every file keeps the licence it arrived under.
