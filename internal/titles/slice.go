// Package titles generates one-line session titles with an LLM.
//
// The pass is deliberately separate from indexing. It is the only part of
// spireweb that talks to a network service, it costs money, and it is slow
// compared to everything else; search must work while it runs, and must keep
// working when it cannot run at all. A session with no title falls back to its
// opening message, so the interface never waits on this and never shows a
// blank row.
package titles

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode/utf8"

	"github.com/llimllib/spireweb/internal/session"
)

// MaxSliceChars bounds the prose handed to the model.
//
// Roughly 2k tokens: prose measures about 4.9 characters per token with the
// embedding model's tokenizer, and conversational English is close enough to
// that for a budget. Exactness is not needed -- this is a cost ceiling, and
// the summarizer caps its own output.
//
// Counting characters rather than tokens is a choice, not laziness. The only
// tokenizer in the process belongs to the embedding model and needs a database
// connection to reach, and titles must work on a machine with no model
// installed, which is most of the reasons semantic search is unavailable.
const MaxSliceChars = 10000

// Slice returns the opening prose of a conversation, labelled by role.
//
// Bounding it is what makes caching possible. Sessions are append-only, so a
// hash over the whole conversation changes with every message, and a session
// being actively worked in would be re-summarized on every reindex -- which is
// to say continuously. A hash over a bounded prefix stops changing once the
// conversation passes the bound, so a long session is paid for once.
//
// The last block is truncated rather than dropped, so the slice reaches its
// bound -- and therefore stops changing -- as early as it can.
func Slice(s *session.Session) string {
	var b strings.Builder
	for _, p := range s.Prose() {
		sep := ""
		if b.Len() > 0 {
			sep = "\n\n"
		}
		label := sep + p.Role + ": "
		// No room for the label and at least some of the text.
		if b.Len()+len(label) >= MaxSliceChars {
			break
		}
		b.WriteString(label)

		text := p.Text
		if room := MaxSliceChars - b.Len(); len(text) > room {
			b.WriteString(text[:runeSafeCut(text, room)])
			break
		}
		b.WriteString(text)
	}
	return b.String()
}

// runeSafeCut returns the largest offset at or below limit that lands on a
// rune boundary, so a truncated slice is still valid UTF-8.
func runeSafeCut(s string, limit int) int {
	if limit >= len(s) {
		return len(s)
	}
	for i := limit; i > 0; i-- {
		if utf8.RuneStart(s[i]) {
			return i
		}
	}
	return 0
}

// keySteps quantize the slice before hashing, which is what "grew
// meaningfully" means in practice.
//
// Bounding the slice settles long sessions, but a new one is nowhere near the
// bound: it grows a message at a time, and hashing the slice directly would
// re-summarize it on every single message -- thirty calls for a thirty-message
// session, each one a slightly longer version of the last. Hashing a prefix
// rounded down to one of these lengths caps that at one call per step, so a
// session is re-titled when it has roughly doubled in size and not otherwise.
//
// Under the first step the whole slice is hashed. A session of two messages
// really has changed subject when it becomes four, and those are the cheapest
// calls there are.
var keySteps = []int{100, 200, 400, 800, 1600, 3200, 6400, MaxSliceChars}

// Key is the cache key for a slice: what gets stored next to the title so a
// later run can tell "already summarized this" from "the opening of this
// conversation changed".
//
// Half of a SHA-256 is 128 bits, which is not going to collide across a few
// thousand sessions, and keeps the stored value short enough to read.
func Key(slice string) string {
	if slice == "" {
		return ""
	}
	n := len(slice)
	for _, step := range keySteps {
		if step <= len(slice) {
			n = step
		}
	}
	sum := sha256.Sum256([]byte(slice[:runeSafeCut(slice, n)]))
	return hex.EncodeToString(sum[:16])
}
