package session

import (
	"bufio"
	"bytes"
	"os"
)

// SkipSDK is the reason returned for a Claude Code session driven through the
// SDK rather than typed by a person.
const SkipSDK = "sdk session"

// SkipEmpty is the reason returned for a session file that parsed cleanly and
// holds no conversation at all.
const SkipEmpty = "no conversation"

// entrypointKey is the JSON key that says how a Claude Code session was
// started. Matched as bytes rather than parsed: the check runs over every
// candidate file on a cold build, and unmarshalling 284MB to read one string
// per line is the expensive way to answer a cheap question.
//
// The literal can only appear as a real key. A conversation that discusses
// `"entrypoint":"` has it escaped as \" inside the JSON string, so the
// unescaped form never occurs in content.
var entrypointKey = []byte(`"entrypoint":"`)

var entrypointCLI = []byte("cli")

// SkipReason reports why a session file should not be indexed, or "" to index
// it.
//
// The only exclusion so far is a Claude Code session with an entrypoint other
// than "cli" -- that is, one produced by the SDK rather than by a person at a
// terminal. Two kinds of file look like that, and both are noise:
//
//   - **Bridge duplicates.** pi tunnelling through claude-bridge records the
//     conversation a second time, as sdk-ts. Indexing it duplicates the pi
//     corpus, and with the worse copy: pi's file carries the rendered diff, the
//     bridged one only structuredPatch.
//   - **spireweb's own title prompts.** The titles pass shells out to
//     `claude -p`, which writes a session file. Index the directory it writes
//     into and the next build indexes that file, generates a title for it, and
//     writes another. On the corpus this was measured at 1215 files and 148MB
//     of transcripts of spireweb asking for titles.
//
// pi sessions have no entrypoint field at all and are never excluded.
//
// Whole-file rather than a bounded prefix: 94 bridge sessions in the corpus
// open with one or two cli records before the bridge takes over, and the first
// non-cli record was as deep as 429, with 13 files past 200. A prefix scan
// would have admitted those as interactive and indexed a truncated fragment of
// each. A full scan of the corpus measured 0.3s, and it exits at the first
// non-cli record, so the files this rejects are the cheapest to reject.
//
// An unreadable file is not skipped: let the parser produce the error, so one
// code path reports it.
func SkipReason(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	for sc.Scan() {
		v, ok := entrypointValue(sc.Bytes())
		if !ok {
			continue
		}
		if !bytes.Equal(v, entrypointCLI) {
			return SkipSDK
		}
	}
	return ""
}

// SkipParsedReason reports why an already-parsed session should not be
// indexed, or "" to index it.
//
// This is the half of the rule that cannot be answered from bytes. SkipReason
// decides from the file; this decides from what the file turned out to mean,
// and a caller owes the index both.
//
// The only case so far is a session with no messages. Claude Code writes a
// session file for a bare slash command -- open it, run /model, quit, and the
// result is a handful of `system`, `cost-state` and `last-prompt` records and
// not one `user` or `assistant`. Only those two become messages, so the
// session parses to nothing: no prose to search, no opening message to preview
// with, and nothing for the titles pass to summarize. It reaches the list as a
// blank row, which is the whole of its contribution.
//
// Emptiness is the test rather than the record types that caused it. A file
// pi is midway through creating is momentarily empty too, and excluding it is
// correct for as long as it stays that way -- the write that gives it a first
// message is what brings it back, through the same path any other change takes.
func SkipParsedReason(s *Session) string {
	if len(s.Messages) == 0 {
		return SkipEmpty
	}
	return ""
}

// entrypointValue returns the value of the entrypoint field in one line.
func entrypointValue(line []byte) ([]byte, bool) {
	i := bytes.Index(line, entrypointKey)
	if i < 0 {
		return nil, false
	}
	rest := line[i+len(entrypointKey):]
	end := bytes.IndexByte(rest, '"')
	if end < 0 {
		return nil, false
	}
	return rest[:end], true
}
