# Third-party components

The release archive ships two files alongside the `spireweb` binary. Both are
third-party work, redistributed under the licences below.

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
