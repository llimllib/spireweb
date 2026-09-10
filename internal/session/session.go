// Package session parses pi agent session files.
//
// Sessions live in ~/.pi/agent/sessions/<mangled-cwd>/<timestamp>_<uuid>.jsonl
// as JSON Lines. The first line is a header; subsequent lines are events.
//
// Note the stored format differs from the streaming event format documented in
// pi's docs/json.md (message_start/message_update/...). Stored files were
// surveyed empirically across 80 files: the record types present are "message",
// "model_change", "thinking_level_change", and "custom_message".
//
// Parsing is deliberately lenient. Sessions are appended to while pi runs, so
// the final line may be a partial write, and the format carries a version field
// that will change. Unknown record types and malformed lines are skipped and
// counted rather than treated as fatal.
package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Roles seen in stored sessions. toolResult carries command output and file
// dumps; it is the bulk of the bytes on disk and is not indexed as prose.
const (
	RoleUser       = "user"
	RoleAssistant  = "assistant"
	RoleToolResult = "toolResult"
)

// Content block types within a message.
const (
	BlockText     = "text"
	BlockToolCall = "toolCall"
	BlockThinking = "thinking"
	BlockImage    = "image"
)

// maxLineBytes caps a single JSONL line. Tool results embedding large files can
// be enormous; bufio.Scanner would otherwise fail the whole file.
const maxLineBytes = 64 << 20 // 64MB

// Header is the first line of a session file.
type Header struct {
	Type      string    `json:"type"`
	Version   int       `json:"version"`
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	CWD       string    `json:"cwd"`
}

// Block is one piece of a message's content.
//
// The fields are a union over block types: text and thinking carry prose,
// toolCall carries a name and an argument object. Unmarshalling all of them
// into one struct keeps parsing to a single pass, at the cost of most fields
// being empty for any given block.
type Block struct {
	Type string `json:"type"`
	Text string `json:"text"`

	// toolCall
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`

	// thinking. Stored separately from Text by pi, and in practice usually
	// empty: the model returns an opaque signature without the reasoning.
	Thinking string `json:"thinking"`
}

// Message is a single conversational turn.
//
// The tool fields are set only on toolResult messages, which reference the
// toolCall they answer by ID. Pairing them is what lets a transcript show a
// call and its output as one collapsible unit.
type Message struct {
	Role    string  `json:"role"`
	Content []Block `json:"content"`

	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"toolName"`
	IsError    bool   `json:"isError"`

	// At comes from the record envelope rather than the message body, whose
	// timestamp field is a string on some roles and a unix milliseconds number
	// on others.
	At time.Time `json:"-"`
}

// record is the envelope around every non-header line.
type record struct {
	Type      string          `json:"type"`
	Timestamp time.Time       `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}

// Session is a parsed session file.
type Session struct {
	ID        string
	Path      string
	CWD       string
	StartedAt time.Time
	ModTime   time.Time
	Size      int64

	Messages []Message

	// SkippedLines counts lines that could not be parsed. A nonzero value is
	// normal for a session pi is actively writing; a large value suggests the
	// format moved.
	SkippedLines int
}

// Project is the display name for a session: the last two segments of its
// working directory, e.g. "git-ls/main".
//
// The basename alone is not enough. A git worktree layout puts the branch name
// in the leaf, so basenames collapse unrelated repositories together: across
// 342 real project directories, 13 distinct repos were all displayed as "main"
// and 9 names were ambiguous overall. Including the parent reduces that to 4
// ambiguous names, worst case 3 directories.
//
// The remaining collisions are deep, genuinely similar paths (several
// terraform directories named databricks/prd_ws on different branches). The
// full CWD is shown in the preview pane, so they stay distinguishable.
//
// Falls back to the mangled session directory name when cwd is missing, and
// finally to "unknown".
func (s *Session) Project() string {
	if cwd := strings.TrimRight(s.CWD, string(filepath.Separator)); cwd != "" {
		base := filepath.Base(cwd)
		if base != "" && base != "." && base != string(filepath.Separator) {
			parent := filepath.Base(filepath.Dir(cwd))
			if parent == "" || parent == "." || parent == string(filepath.Separator) {
				return base
			}
			return parent + "/" + base
		}
	}
	if d := filepath.Base(filepath.Dir(s.Path)); d != "" && d != "." {
		return strings.Trim(strings.ReplaceAll(d, "-", " "), " ")
	}
	return "unknown"
}

// Preview returns the first user message, collapsed to a single line and
// truncated to n runes, for display in result lists.
func (s *Session) Preview(n int) string {
	return s.firstText(RoleUser, n)
}

// Reply returns the first assistant message, on the same terms as Preview.
//
// The session list shows three lines -- project, title, and opening exchange --
// and until titles are generated the title line falls back to Preview. Reply
// keeps the third line from repeating it.
func (s *Session) Reply(n int) string {
	return s.firstText(RoleAssistant, n)
}

func (s *Session) firstText(role string, n int) string {
	for _, m := range s.Messages {
		if m.Role != role {
			continue
		}
		for _, b := range m.Content {
			if b.Type != BlockText {
				continue
			}
			if t := collapse(b.Text); t != "" {
				return truncate(t, n)
			}
		}
	}
	return ""
}

// ToolResults indexes toolResult messages by the toolCall ID they answer, so a
// transcript can render a call together with its output.
func (s *Session) ToolResults() map[string]*Message {
	out := make(map[string]*Message)
	for i := range s.Messages {
		m := &s.Messages[i]
		if m.Role == RoleToolResult && m.ToolCallID != "" {
			out[m.ToolCallID] = m
		}
	}
	return out
}

// TextBlock is an indexable unit of prose with enough provenance to locate it
// in the original session.
type TextBlock struct {
	MsgIdx int
	Role   string
	Text   string
}

// Prose returns text blocks from user and assistant messages: the searchable
// content of a session.
//
// Tool results are excluded deliberately. They are mostly command output and
// file contents, they dwarf the conversation, and matching them tends to
// surface a file that was read rather than a discussion that happened. Tool
// call arguments and thinking blocks are excluded for the same reason.
func (s *Session) Prose() []TextBlock {
	var out []TextBlock
	for i, m := range s.Messages {
		if m.Role != RoleUser && m.Role != RoleAssistant {
			continue
		}
		for _, b := range m.Content {
			if b.Type != BlockText {
				continue
			}
			if t := collapse(b.Text); t != "" {
				out = append(out, TextBlock{MsgIdx: i, Role: m.Role, Text: t})
			}
		}
	}
	return out
}

// Parse reads a session file. It returns an error only when the file cannot be
// opened or has no usable header; malformed individual lines are counted in
// SkippedLines.
func Parse(path string) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}

	s := &Session{Path: path, ModTime: fi.ModTime(), Size: fi.Size()}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)

	first := true
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}

		if first {
			first = false
			var h Header
			if err := json.Unmarshal(line, &h); err != nil || h.Type != "session" {
				return nil, fmt.Errorf("%s: missing session header", filepath.Base(path))
			}
			s.ID, s.CWD, s.StartedAt = h.ID, h.CWD, h.Timestamp
			continue
		}

		var r record
		if err := json.Unmarshal(line, &r); err != nil {
			// Expected for a partial trailing line while pi is writing.
			s.SkippedLines++
			continue
		}
		if r.Type != "message" || len(r.Message) == 0 {
			continue // model_change, thinking_level_change, custom_message, future types
		}

		var m Message
		if err := json.Unmarshal(r.Message, &m); err != nil {
			s.SkippedLines++
			continue
		}
		m.At = r.Timestamp
		s.Messages = append(s.Messages, m)
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		// Truncated final line: keep what parsed rather than discarding the file.
		if errors.Is(err, bufio.ErrTooLong) {
			s.SkippedLines++
		} else {
			return nil, err
		}
	}
	if s.ID == "" {
		return nil, fmt.Errorf("%s: empty session", filepath.Base(path))
	}
	return s, nil
}

// DefaultDir is where pi stores sessions.
func DefaultDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".pi", "agent", "sessions")
	}
	return filepath.Join(home, ".pi", "agent", "sessions")
}

// FileInfo identifies a session file without parsing it. Indexing uses ModTime
// and Size to skip unchanged files.
type FileInfo struct {
	Path    string
	ModTime time.Time
	Size    int64
}

// Discover walks dir and returns every .jsonl session file.
func Discover(dir string) ([]FileInfo, error) {
	var out []FileInfo
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable subtree: skip, do not abort the walk
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		out = append(out, FileInfo{Path: p, ModTime: fi.ModTime(), Size: fi.Size()})
		return nil
	})
	return out, err
}

// collapse normalizes whitespace so chunk sizes reflect content rather than
// formatting, and excerpts render on one line.
func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}
