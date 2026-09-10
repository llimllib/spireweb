package render

import (
	"encoding/json"
	"fmt"
	"html/template"
	"sort"
	"strings"
	"time"

	"github.com/llimllib/spireweb/internal/session"
)

// Entry kinds. A transcript is a flat sequence rather than a tree, because
// that is how it reads: an assistant turn interleaves prose and tool calls,
// and nesting the calls under the message would put them out of order.
const (
	KindUser      = "user"
	KindAssistant = "assistant"
	KindThinking  = "thinking"
	KindTool      = "tool"
)

// Entry is one renderable item in a transcript.
type Entry struct {
	Kind string
	At   time.Time

	// HTML is set for prose entries.
	HTML template.HTML

	// Tool is set when Kind is KindTool.
	Tool *ToolEntry
}

// ToolEntry is a collapsed tool call. It carries only what the closed summary
// line needs; arguments and output are fetched on expand, because a single
// call can embed an entire file in either direction.
type ToolEntry struct {
	Name    string
	Summary string
	MsgIdx  int
	Blk     int
	IsError bool

	// HasResult is false for a call whose result is missing, which happens in
	// a session that was interrupted mid-call.
	HasResult bool
}

// Transcript converts a parsed session into renderable entries.
func Transcript(s *session.Session) []Entry {
	results := s.ToolResults()

	var out []Entry
	for i, m := range s.Messages {
		switch m.Role {
		case session.RoleUser, session.RoleAssistant:
		default:
			// toolResult messages are rendered through the call that produced
			// them, so they are not entries of their own.
			continue
		}

		for b, blk := range m.Content {
			switch blk.Type {
			case session.BlockText:
				if strings.TrimSpace(blk.Text) == "" {
					continue
				}
				out = append(out, Entry{Kind: m.Role, At: m.At, HTML: Markdown(blk.Text)})

			case session.BlockThinking:
				// Usually empty: the model returns a signature without the
				// reasoning, and rendering an empty disclosure is just noise.
				if strings.TrimSpace(blk.Thinking) == "" {
					continue
				}
				out = append(out, Entry{Kind: KindThinking, At: m.At, HTML: Markdown(blk.Thinking)})

			case session.BlockToolCall:
				res, ok := results[blk.ID]
				t := &ToolEntry{
					Name:      blk.Name,
					Summary:   ToolSummary(blk.Name, blk.Arguments),
					MsgIdx:    i,
					Blk:       b,
					HasResult: ok,
				}
				if ok {
					t.IsError = res.IsError
				}
				out = append(out, Entry{Kind: KindTool, At: m.At, Tool: t})
			}
		}
	}
	return out
}

// summaryArg names the argument worth showing on a collapsed tool line, per
// tool. "ls" tells you nothing; "ls /etc/nginx" tells you what happened.
var summaryArg = map[string][]string{
	"bash":  {"command"},
	"read":  {"path"},
	"write": {"path"},
	"edit":  {"path"},
	"grep":  {"pattern"},
	"find":  {"pattern", "glob", "path"},
	"fetch": {"url"},
	"task":  {"description"},
}

// ToolSummary renders the one-line description shown on a collapsed call.
//
// Falls back to the first short string argument, so a tool this code has never
// heard of still gets a useful line instead of a bare name. Deliberately does
// not return template.HTML: the caller escapes it, because these strings are
// arbitrary session content.
func ToolSummary(name string, args json.RawMessage) string {
	if len(args) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return ""
	}

	for _, key := range summaryArg[name] {
		if v, ok := m[key].(string); ok && v != "" {
			return collapse(v, 160)
		}
	}

	// Unknown tool: prefer the shortest string value, which is almost always
	// the identifying one (a path or a query) rather than a body of content.
	type kv struct {
		k string
		v string
	}
	var candidates []kv
	for k, v := range m {
		if s, ok := v.(string); ok && s != "" && len(s) < 400 {
			candidates = append(candidates, kv{k, s})
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	sort.Slice(candidates, func(i, j int) bool {
		if len(candidates[i].v) != len(candidates[j].v) {
			return len(candidates[i].v) < len(candidates[j].v)
		}
		return candidates[i].k < candidates[j].k
	})
	return collapse(candidates[0].v, 160)
}

// ToolDetail is the expanded body of a tool call.
type ToolDetail struct {
	Name    string
	Args    string
	Output  string
	IsError bool

	// Truncated reports that Output was clipped.
	Truncated bool
	FullBytes int
}

// MaxOutputBytes caps what an expanded tool call sends.
//
// Tool output is around 98% of a session's bytes and a single result can be
// megabytes, which is the whole reason expansion is lazy. Clipping keeps one
// enormous result from undoing that.
const MaxOutputBytes = 64 << 10

// Tool builds the expanded view of one tool call.
func Tool(s *session.Session, msgIdx, blk int) (ToolDetail, error) {
	if msgIdx < 0 || msgIdx >= len(s.Messages) {
		return ToolDetail{}, fmt.Errorf("message %d out of range", msgIdx)
	}
	m := s.Messages[msgIdx]
	if blk < 0 || blk >= len(m.Content) {
		return ToolDetail{}, fmt.Errorf("block %d out of range", blk)
	}
	call := m.Content[blk]
	if call.Type != session.BlockToolCall {
		return ToolDetail{}, fmt.Errorf("block %d is %q, not a tool call", blk, call.Type)
	}

	d := ToolDetail{Name: call.Name, Args: prettyJSON(call.Arguments)}

	if res, ok := s.ToolResults()[call.ID]; ok {
		d.IsError = res.IsError
		var b strings.Builder
		for _, rb := range res.Content {
			switch rb.Type {
			case session.BlockText:
				b.WriteString(rb.Text)
			case session.BlockImage:
				b.WriteString("[image]")
			}
		}
		out := b.String()
		d.FullBytes = len(out)
		if len(out) > MaxOutputBytes {
			d.Output = out[:runeSafeCut(out, MaxOutputBytes)]
			d.Truncated = true
		} else {
			d.Output = out
		}
	}
	return d, nil
}

// runeSafeCut returns the largest offset at or below limit that lands on a
// rune boundary, so clipping cannot produce invalid UTF-8.
func runeSafeCut(s string, limit int) int {
	if limit >= len(s) {
		return len(s)
	}
	for i := limit; i > 0; i-- {
		if s[i]&0xC0 != 0x80 {
			return i
		}
	}
	return 0
}

func prettyJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(raw)
	}
	return string(out)
}

// collapse flattens whitespace and truncates, for one-line display.
func collapse(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
