package session

// Claude Code session files.
//
// Sessions live in <config>/projects/<mangled-cwd>/<uuid>.jsonl, where <config>
// is $CLAUDE_CONFIG_DIR, ~/.config/claude, or ~/.claude. The layout differs
// from pi's in three ways that matter here:
//
//   - There is no header line. Identity and metadata -- sessionId, cwd,
//     timestamp, version, gitBranch -- repeat on every record. Across a corpus
//     of 1590 files, sessionId equalled the filename every time.
//   - The message body is an Anthropic API message rather than pi's own shape.
//   - Tool results have no role of their own. They arrive as "user" records
//     carrying tool_result blocks, and one record may carry several.
//
// Record types beyond user and assistant -- attachment, atis-latch,
// last-prompt, queue-operation, mode, system, permission-mode, cost-state,
// file-history-snapshot -- are skipped, the same way pi's model_change is.
// They were two thirds of the records in that corpus and none of them is
// conversation.

import (
	"encoding/json"
	"time"
)

// Claude Code content block types, which are the Anthropic API's.
const (
	claudeBlockText       = "text"
	claudeBlockThinking   = "thinking"
	claudeBlockToolUse    = "tool_use"
	claudeBlockToolResult = "tool_result"
	claudeBlockImage      = "image"
)

// claudeRecord is one line of a Claude Code session file.
type claudeRecord struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionId"`
	CWD       string          `json:"cwd"`
	Timestamp time.Time       `json:"timestamp"`
	Message   json.RawMessage `json:"message"`

	// ToolUseResult is the per-tool payload beside a tool_result block: for an
	// edit it holds structuredPatch, the hunks the diff renderer wants. It sits
	// on the record rather than inside the message, which is why Raw keeps the
	// whole line.
	ToolUseResult json.RawMessage `json:"toolUseResult"`
}

// claudeMessage is the Anthropic API message on a record.
//
// Content is raw because it is a string on some records and an array of blocks
// on others -- 1369 of the corpus's user records used the string form, written
// by older versions.
type claudeMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type claudeBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`

	// thinking. Unlike pi's, these usually carry the reasoning rather than an
	// opaque signature.
	Thinking string `json:"thinking"`

	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`

	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

// isClaudeLine reports whether a first line looks like a Claude Code record.
//
// Detection is per file rather than per directory so that a single --dir can
// hold either, and so that neither parser has to know where sessions live. pi's
// first line is a {"type":"session"} header, which Parse checks for; a Claude
// Code file has no header at all, and every record carries sessionId.
func isClaudeLine(line []byte) bool {
	var r claudeRecord
	if err := json.Unmarshal(line, &r); err != nil {
		return false
	}
	return r.SessionID != ""
}

// parseClaudeLine converts one record into zero or more messages.
//
// Zero for the record types that are not conversation, and for an assistant
// turn whose blocks are all empty. More than one for a user record answering
// several tool calls at once: Message.ToolCallID is singular, so each
// tool_result block becomes its own message. 622 records in the corpus did
// that, which is what parallel tool calls look like on the way back.
func parseClaudeLine(r *claudeRecord, raw []byte, keepRaw bool) []Message {
	if r.Type != RoleUser && r.Type != RoleAssistant {
		return nil
	}
	if len(r.Message) == 0 {
		return nil
	}

	var cm claudeMessage
	if err := json.Unmarshal(r.Message, &cm); err != nil {
		return nil
	}

	blocks, ok := claudeBlocks(cm.Content)
	if !ok {
		return nil
	}

	// The whole record, not just the message: toolUseResult lives on the
	// envelope and holds the diff an edit applied. Storing only the message
	// would make the archive lossy in exactly the way it exists to avoid.
	var rawLine json.RawMessage
	if keepRaw {
		rawLine = append(json.RawMessage(nil), raw...)
	}

	var out []Message
	var content []Block

	for _, b := range blocks {
		switch b.Type {
		case claudeBlockText:
			content = append(content, Block{Type: BlockText, Text: b.Text})

		case claudeBlockThinking:
			content = append(content, Block{Type: BlockThinking, Thinking: b.Thinking})

		case claudeBlockToolUse:
			content = append(content, Block{
				Type:      BlockToolCall,
				ID:        b.ID,
				Name:      b.Name,
				Arguments: b.Input,
			})

		case claudeBlockToolResult:
			// Its own message, so that ToolResults() can key it by the call it
			// answers. Every copy carries the same Raw: archiveMessages counts
			// rows to decide what is already stored, so a message with no Raw
			// would leave a gap that makes the count disagree with the message
			// list forever.
			out = append(out, Message{
				Role:       RoleToolResult,
				Content:    claudeResultContent(b.Content),
				ToolCallID: b.ToolUseID,
				IsError:    b.IsError,
				Details:    r.ToolUseResult,
				At:         r.Timestamp,
				Raw:        rawLine,
			})
		}
	}

	if len(content) > 0 {
		// Ahead of any tool results from the same record. In practice a record
		// holds one or the other, never both.
		out = append([]Message{{
			Role:    r.Type,
			Content: content,
			At:      r.Timestamp,
			Raw:     rawLine,
		}}, out...)
	}
	return out
}

// claudeBlocks normalizes a message's content, which is either an array of
// blocks or a bare string.
func claudeBlocks(content json.RawMessage) ([]claudeBlock, bool) {
	if len(content) == 0 {
		return nil, false
	}
	if content[0] == '"' {
		var s string
		if err := json.Unmarshal(content, &s); err != nil {
			return nil, false
		}
		return []claudeBlock{{Type: claudeBlockText, Text: s}}, true
	}
	var blocks []claudeBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil, false
	}
	return blocks, true
}

// claudeResultContent normalizes a tool result's content, which is a string
// about half the time and an array of blocks the rest.
func claudeResultContent(content json.RawMessage) []Block {
	blocks, ok := claudeBlocks(content)
	if !ok {
		return nil
	}
	out := make([]Block, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case claudeBlockText:
			out = append(out, Block{Type: BlockText, Text: b.Text})
		case claudeBlockImage:
			out = append(out, Block{Type: BlockImage})
		}
	}
	return out
}
