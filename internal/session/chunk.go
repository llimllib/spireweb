package session

import (
	"strings"
	"unicode/utf8"
)

// MaxTokens is the model's context limit. all-MiniLM-L6-v2 accepts 512 tokens.
//
// Exceeding it must be prevented, not handled: sqlite-lembed v0.0.1-alpha.8
// SEGFAULTS on over-length input rather than returning an error. Measured
// precisely with lembed_tokenize_json: 512 tokens embeds fine, 522 crashes the
// process. llama.cpp's CLI reports the same condition as a clean error, so this
// is a bug in the alpha extension.
const MaxTokens = 512

// TokenSafetyMargin leaves room for tokenizer disagreement.
//
// Chunking is done by character count for speed, then verified by token count.
// The margin reduces how often the verification pass has to re-split.
const TokenSafetyMargin = 32

// MaxChunkChars is the primary chunking limit, in characters.
//
// Character-based splitting is a fast first pass. It is not sufficient on its
// own: token density varies enormously with content. Measured with the model's
// own tokenizer:
//
//	800 chars of ordinary prose  ->  162 tokens   (4.9 chars/token)
//	800 chars of dense JSON      ->  802 tokens   (1.0 chars/token)
//	400 CJK characters           ->  402 tokens   (1.0 chars/token)
//
// So 800 characters is safe for prose but 5x over the limit for dense
// punctuation, which appears in sessions constantly: JSON payloads, base64,
// minified code, stack traces. A token-aware second pass (see TokenSplitter)
// handles those.
const MaxChunkChars = 800

// SafeChunkChars is the conservative limit used when re-splitting content that
// exceeded the token limit. At roughly 1 token per character in the worst case,
// this keeps even the densest input under MaxTokens.
const SafeChunkChars = MaxTokens - TokenSafetyMargin

// Chunk splits text into pieces of at most limit bytes, preferring word
// boundaries. A single word longer than the limit is split at the limit, since
// leaving it whole would risk the context overflow this function exists to
// prevent.
//
// Splits never fall inside a multi-byte character. Slicing a string by byte
// offset can cut a rune in half, and the resulting invalid UTF-8 crashes
// sqlite-lembed with an uncaught C++ std::invalid_argument. Real sessions
// contain plenty of CJK, emoji, and box-drawing characters, so this is not
// hypothetical.
//
// Input is assumed whitespace-collapsed (see collapse); Prose does that.
func Chunk(text string, limit int) []string {
	if limit <= 0 {
		limit = MaxChunkChars
	}
	if text == "" {
		return nil
	}
	if len(text) <= limit {
		return []string{text}
	}

	var out []string
	var b strings.Builder

	flush := func() {
		if b.Len() > 0 {
			out = append(out, b.String())
			b.Reset()
		}
	}

	for _, w := range strings.Fields(text) {
		// A word that cannot fit on its own line: hard-split it on rune
		// boundaries.
		for len(w) > limit {
			flush()
			cut := runeSafeCut(w, limit)
			if cut == 0 {
				// A single rune wider than the limit; emit it whole rather than
				// corrupt it or loop forever.
				_, size := utf8.DecodeRuneInString(w)
				cut = size
			}
			out = append(out, w[:cut])
			w = w[cut:]
		}
		switch {
		case b.Len() == 0:
			b.WriteString(w)
		case b.Len()+1+len(w) <= limit:
			b.WriteByte(' ')
			b.WriteString(w)
		default:
			flush()
			b.WriteString(w)
		}
	}
	flush()
	return out
}

// runeSafeCut returns the largest offset at or below limit that lands on a rune
// boundary.
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

// Chunks expands a session's prose into embeddable units, preserving the
// message index and role of each source block.
func (s *Session) Chunks(limit int) []TextBlock {
	var out []TextBlock
	for _, p := range s.Prose() {
		for _, c := range Chunk(p.Text, limit) {
			out = append(out, TextBlock{MsgIdx: p.MsgIdx, Role: p.Role, Text: c})
		}
	}
	return out
}

// TokenCounter reports how many tokens a string becomes. Implemented by the
// embedding backend, which owns the tokenizer.
type TokenCounter interface {
	CountTokens(text string) (int, error)
}

// SplitToTokenLimit ensures every returned piece is within maxTokens.
//
// Character-based chunking is the fast path and usually sufficient. This pass
// catches the cases it cannot predict: text whose token density is far higher
// than prose. Any over-limit chunk is re-split at SafeChunkChars and each piece
// re-verified, so a pathological input degrades into more, smaller chunks rather
// than crashing the embedding extension.
//
// A nil counter returns the input unchanged, so lexical-only indexing does not
// pay for tokenization.
func SplitToTokenLimit(tc TokenCounter, chunks []string, maxTokens int) ([]string, error) {
	if tc == nil {
		return chunks, nil
	}
	if maxTokens <= 0 {
		maxTokens = MaxTokens
	}

	out := make([]string, 0, len(chunks))
	for _, c := range chunks {
		n, err := tc.CountTokens(c)
		if err != nil {
			return nil, err
		}
		if n <= maxTokens {
			out = append(out, c)
			continue
		}

		// Too dense: re-split with a limit low enough for ~1 token per character.
		for _, piece := range Chunk(c, SafeChunkChars) {
			pn, err := tc.CountTokens(piece)
			if err != nil {
				return nil, err
			}
			if pn <= maxTokens {
				out = append(out, piece)
				continue
			}
			// Still over: halve until it fits. Guaranteed to terminate because
			// token count is bounded by character count.
			out = append(out, forceSplit(tc, piece, maxTokens)...)
		}
	}
	return out, nil
}

// forceSplit halves text until every piece is within the token limit.
func forceSplit(tc TokenCounter, text string, maxTokens int) []string {
	limit := len(text) / 2
	for limit > 16 {
		pieces := Chunk(text, limit)
		ok := true
		for _, p := range pieces {
			n, err := tc.CountTokens(p)
			if err != nil || n > maxTokens {
				ok = false
				break
			}
		}
		if ok {
			return pieces
		}
		limit /= 2
	}
	// Last resort: drop the chunk rather than risk a crash. Losing one dense
	// blob from the index is far better than losing the whole index build.
	return nil
}
