package titles

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/llimllib/spireweb/internal/session"
)

// sess builds a session from alternating user/assistant text.
func sess(texts ...string) *session.Session {
	s := &session.Session{ID: "s1"}
	for i, t := range texts {
		role := session.RoleUser
		if i%2 == 1 {
			role = session.RoleAssistant
		}
		s.Messages = append(s.Messages, session.Message{
			Role:    role,
			Content: []session.Block{{Type: session.BlockText, Text: t}},
		})
	}
	return s
}

func TestSliceLabelsRolesInOrder(t *testing.T) {
	got := Slice(sess("center a div", "use flexbox"))
	want := "user: center a div\n\nassistant: use flexbox"
	if got != want {
		t.Errorf("Slice() = %q, want %q", got, want)
	}
}

func TestSliceExcludesToolResults(t *testing.T) {
	s := sess("read the file")
	s.Messages = append(s.Messages, session.Message{
		Role:     session.RoleToolResult,
		ToolName: "read",
		Content:  []session.Block{{Type: session.BlockText, Text: "SECRET FILE CONTENTS"}},
	})
	if strings.Contains(Slice(s), "SECRET") {
		t.Error("tool output reached the summarizer; it is most of the bytes and none of the subject")
	}
}

func TestSliceIsBounded(t *testing.T) {
	long := strings.Repeat("all work and no play ", 5000)
	got := Slice(sess(long, long, long))
	if len(got) > MaxSliceChars {
		t.Errorf("len(Slice()) = %d, want <= %d", len(got), MaxSliceChars)
	}
	if len(got) < MaxSliceChars/2 {
		t.Errorf("len(Slice()) = %d: the bound should be filled, not undershot", len(got))
	}
}

// The whole reason the slice is bounded: a session that keeps growing must
// stop producing new keys, or a conversation being actively worked in is
// re-summarized on every reindex.
func TestKeyStopsChangingOnceBounded(t *testing.T) {
	opening := []string{strings.Repeat("a long opening question. ", 600)}
	first := Key(Slice(sess(opening...)))

	for range 5 {
		opening = append(opening, "and another message on the end")
		if got := Key(Slice(sess(opening...))); got != first {
			t.Fatalf("key changed after appending: %s -> %s", first, got)
		}
	}
}

// ...while a session still short enough to fit does change, because its title
// was written before most of the conversation existed.
func TestKeyChangesWhileSessionIsShort(t *testing.T) {
	first := Key(Slice(sess("how do I center a div")))
	second := Key(Slice(sess("how do I center a div", "use flexbox")))
	if first == second {
		t.Error("key unchanged after the conversation grew within the bound")
	}
}

// Between those two extremes, growth has to be substantial to be worth paying
// for again: a session a few thousand characters long adds a message and the
// key does not move.
func TestKeyIgnoresSmallGrowth(t *testing.T) {
	base := []string{strings.Repeat("a question about the indexer. ", 70)} // ~2100 chars
	first := Key(Slice(sess(base...)))

	grown := append(base, "one more short message")
	if got := Key(Slice(sess(grown...))); got != first {
		t.Error("key changed after one more message; a live session would be re-summarized per message")
	}

	// Doubling in size is another matter: by then the conversation is mostly
	// content the title never saw.
	doubled := append(base, strings.Repeat("a long digression about something else. ", 70))
	if got := Key(Slice(sess(doubled...))); got == first {
		t.Error("key unchanged after the session doubled in size")
	}
}

func TestSliceTruncatesOnRuneBoundaries(t *testing.T) {
	// Three-byte runes, so almost every byte offset is mid-character. Invalid
	// UTF-8 is not hypothetical here: it is what a naive byte slice produces.
	got := Slice(sess(strings.Repeat("日本語テキスト", 5000)))
	if !utf8.ValidString(got) {
		t.Error("Slice() returned invalid UTF-8")
	}
}

func TestKeyOfEmptySliceIsEmpty(t *testing.T) {
	if Key("") != "" {
		t.Error("an empty session must not get a key, or it would look summarized")
	}
	if Slice(sess()) != "" {
		t.Error("a session with no prose should produce no slice")
	}
}
