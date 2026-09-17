package render

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Diff line kinds.
const (
	DiffContext = "ctx"
	DiffAdd     = "add"
	DiffDel     = "del"

	// DiffGap is the "..." pi emits where it skipped unchanged lines, and what
	// the space between two of Claude Code's hunks means.
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

// patchHunk is one hunk of Claude Code's structuredPatch: a standard unified
// diff hunk, split into its header and its lines.
type patchHunk struct {
	OldStart int      `json:"oldStart"`
	OldLines int      `json:"oldLines"`
	NewStart int      `json:"newStart"`
	NewLines int      `json:"newLines"`
	Lines    []string `json:"lines"`
}

// ParseDiff reads the diff an agent recorded on an edit's tool result.
//
// Both agents record one, in different shapes, and the argument for using
// either rather than recomputing is the same: the call's arguments carry only
// the old and new text, so a diff derived from them could show what changed but
// never where in the file it landed, and the line numbers are most of what
// makes a diff readable.
//
//   - pi writes details.diff, the rendered text its terminal drew: a marker
//     column, a right-aligned line number, context, and "..." where it skipped
//     unchanged lines. The numbers have to be parsed back out of it.
//   - Claude Code writes toolUseResult.structuredPatch, real hunks with
//     oldStart/newStart, so the numbers are computed by walking each hunk
//     rather than read off the line.
//
// Returns nil for anything that is not an edit result carrying a diff. That
// includes failed edits, whose details are empty in pi and carry no
// structuredPatch in Claude Code, and every other tool: a bash result's
// toolUseResult is stdout and stderr, a read's is a file object. The caller
// falls back to showing the arguments, which for a failed edit is exactly what
// is wanted -- the text that could not be found.
func ParseDiff(details json.RawMessage) ([]DiffLine, bool) {
	if len(details) == 0 {
		return nil, false
	}
	var d struct {
		Diff            string      `json:"diff"`
		StructuredPatch []patchHunk `json:"structuredPatch"`
	}
	if err := json.Unmarshal(details, &d); err != nil {
		return nil, false
	}
	if d.Diff != "" {
		return parseRenderedDiff(d.Diff)
	}
	if len(d.StructuredPatch) > 0 {
		return parseStructuredPatch(d.StructuredPatch)
	}
	return nil, false
}

// parseRenderedDiff reads pi's already-drawn diff.
func parseRenderedDiff(diff string) ([]DiffLine, bool) {
	var out []DiffLine
	var n int
	for _, raw := range strings.Split(strings.TrimRight(diff, "\n"), "\n") {
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

// parseStructuredPatch renders Claude Code's hunks.
//
// One number column, as pi's format has: a deleted line is numbered in the old
// file and everything else in the new one, which is what "where is this line
// now" means for the lines that still exist.
//
// Hunks are separated by a gap marker, the same one pi emits where it skipped
// unchanged lines -- which is exactly what the space between two hunks is.
func parseStructuredPatch(hunks []patchHunk) ([]DiffLine, bool) {
	var out []DiffLine
	var n int
	for i, h := range hunks {
		if i > 0 {
			out = append(out, DiffLine{Kind: DiffGap})
		}
		oldNum, newNum := h.OldStart, h.NewStart
		for _, raw := range h.Lines {
			if n += len(raw) + 1; n > MaxOutputBytes {
				return out, true
			}
			if raw == "" {
				// A context line is " " plus the text, so an empty line in the
				// file is " ". Nothing should produce "", but a line with no
				// marker is still content and dropping it would misalign the
				// numbers that follow.
				out = append(out, DiffLine{Kind: DiffContext, Num: strconv.Itoa(newNum)})
				oldNum++
				newNum++
				continue
			}
			marker, text := raw[0], raw[1:]
			switch marker {
			case '-':
				out = append(out, DiffLine{Kind: DiffDel, Num: strconv.Itoa(oldNum), Text: text})
				oldNum++
			case '+':
				out = append(out, DiffLine{Kind: DiffAdd, Num: strconv.Itoa(newNum), Text: text})
				newNum++
			case '\\':
				// "\ No newline at end of file": a note about the previous line
				// rather than a line of the file, so it takes no number and
				// advances nothing.
				out = append(out, DiffLine{Kind: DiffContext, Text: raw})
			default:
				out = append(out, DiffLine{Kind: DiffContext, Num: strconv.Itoa(newNum), Text: text})
				oldNum++
				newNum++
			}
		}
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
