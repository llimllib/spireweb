package session

import (
	"bufio"
	"bytes"
	"os"
)

// SkipSDK is the reason returned for a Claude Code session driven through the
// SDK rather than typed by a person.
const SkipSDK = "sdk session"

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
