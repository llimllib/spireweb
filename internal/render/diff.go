package render

import (
	"encoding/json"
	"strings"
)

// Diff line kinds.
const (
	DiffContext = "ctx"
	DiffAdd     = "add"
	DiffDel     = "del"

	// DiffGap is the "..." pi emits where it skipped unchanged lines.
	DiffGap = "gap"
)

// DiffLine is one line of an edit's diff, split into the columns it is drawn
// with. Num is the line number in the file, empty on a gap.
type DiffLine struct {
	Kind string
	Num  string
	Text string
}

// Mark is the +/- column, as a character rather than as colour alone. Copying
// a diff should not pick it up, which is CSS's problem, but a screen reader
// has nothing else to go on.
func (l DiffLine) Mark() string {
	switch l.Kind {
	case DiffAdd:
		return "+"
	case DiffDel:
		return "-"
	default:
		return " "
	}
}

// ParseDiff reads the diff pi records on an edit's tool result.
//
// The session file carries the rendered diff -- line numbers, surrounding
// context, and gaps -- in details.diff, which is the same text the terminal
// draws. That is worth using rather than recomputing: the session stores only
// the old and new text of each edit, so a diff computed here could show what
// changed but never where it was in the file, and the line numbers are most of
// what makes a diff readable.
//
// Returns nil for anything that is not an edit result carrying a diff,
// including failed edits, whose details are empty. The caller falls back to
// showing the arguments, which for a failed edit is exactly what is wanted:
// the text that could not be found.
func ParseDiff(details json.RawMessage) ([]DiffLine, bool) {
	if len(details) == 0 {
		return nil, false
	}
	var d struct {
		Diff string `json:"diff"`
	}
	if err := json.Unmarshal(details, &d); err != nil || d.Diff == "" {
		return nil, false
	}

	var out []DiffLine
	var n int
	for _, raw := range strings.Split(strings.TrimRight(d.Diff, "\n"), "\n") {
		// Bounded like tool output is, and for the same reason. Real diffs are
		// nothing like this big -- 387 lines is the largest in a corpus of
		// 1128 sessions -- so this only catches something pathological.
		if n += len(raw) + 1; n > MaxOutputBytes {
			return out, true
		}
		out = append(out, parseDiffLine(raw))
	}
	return out, false
}

// parseDiffLine splits one line into marker, line number, and text.
//
// The format is a marker column, then a right-aligned line number, then a
// single space, then the line as it appears in the file. Everything after that
// space is kept exactly, because it is code and its indentation is meaningful.
func parseDiffLine(raw string) DiffLine {
	if raw == "" {
		return DiffLine{Kind: DiffContext}
	}

	kind := DiffContext
	switch raw[0] {
	case '+':
		kind = DiffAdd
	case '-':
		kind = DiffDel
	}
	rest := raw[1:]

	num, text, ok := splitLineNumber(rest)
	if !ok {
		// A gap marker carries no line number, which is how it is recognized.
		if strings.TrimSpace(rest) == "..." {
			return DiffLine{Kind: DiffGap}
		}
		return DiffLine{Kind: kind, Text: rest}
	}
	return DiffLine{Kind: kind, Num: num, Text: text}
}

// splitLineNumber consumes leading spaces, digits, and the single space that
// separates the number from the line.
func splitLineNumber(s string) (num, text string, ok bool) {
	i := 0
	for i < len(s) && s[i] == ' ' {
		i++
	}
	j := i
	for j < len(s) && s[j] >= '0' && s[j] <= '9' {
		j++
	}
	if j == i || j >= len(s) || s[j] != ' ' {
		return "", "", false
	}
	return s[i:j], s[j+1:], true
}
