package titles

import "fmt"

// Backends.
const (
	// BackendAPI calls the Anthropic API with ANTHROPIC_API_KEY.
	BackendAPI = "api"

	// BackendClaude shells out to the Claude Code CLI, which uses whatever
	// authentication Claude Code has -- including a Pro or Max subscription,
	// so the pass costs no API credit and needs no key.
	BackendClaude = "claude"
)

// NewSummarizer builds the chosen backend.
//
// There is deliberately no "auto". Detecting the Claude CLI and using it
// because no API key was set would start spending a subscription's rate limit
// -- shared with the interactive sessions it is actually for -- on a pass over
// a thousand sessions, without anyone asking for it. The note for a missing
// key names the alternative instead, which is discoverable without being
// automatic.
func NewSummarizer(backend string) (Summarizer, error) {
	switch backend {
	case "", BackendAPI:
		s, err := NewAnthropic()
		if err != nil {
			// Name what the alternative costs, not just that it exists. Someone
			// without an API key is usually a Claude Code user, for whom
			// "claude" is the right answer -- but it spends the rate limit
			// their interactive sessions share, and finding that out afterwards
			// is the wrong order.
			return nil, fmt.Errorf("%w.\n"+
				"  --titles-via=claude uses the Claude Code CLI instead, which bills the\n"+
				"  subscription it is signed in to and shares its rate limit with your\n"+
				"  interactive sessions. --titles N caps a run, for trying it out", err)
		}
		return s, nil
	case BackendClaude:
		return NewClaudeCLI()
	default:
		return nil, fmt.Errorf("unknown titles backend %q, want %q or %q",
			backend, BackendAPI, BackendClaude)
	}
}

// ConcurrencyFor returns how many summaries to run at once for a backend.
//
// The CLI is a process per call rather than a request, and its rate limit is
// a subscription's, so it gets a smaller number.
func ConcurrencyFor(s Summarizer) int {
	if _, ok := s.(*ClaudeCLI); ok {
		return CLIConcurrency
	}
	return DefaultConcurrency
}
