package titles

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ClaudeCLI summarizes by shelling out to the Claude Code command line tool.
//
// The reason to want this is billing, not capability: `claude -p` uses
// whatever authentication Claude Code already has, so a Pro or Max
// subscription pays for the pass and no API key exists to be managed or
// leaked. It is the supported interface to that subscription -- as opposed to
// lifting the OAuth token out of Claude Code's credentials and calling the API
// with it, which works until it does not.
//
// It costs about 4.5 seconds a call against the API's ~1, nearly all of it
// starting a Node process. For a one-time pass over a corpus that is a trade
// worth making; it is why Concurrency exists.
type ClaudeCLI struct {
	// Bin is the executable, "claude" by default.
	Bin string

	// Model is passed through to --model. Empty uses whatever Claude Code
	// would choose, which is not what this wants: a title is the cheapest
	// possible job.
	Model string

	// Dir is the working directory. Empty means a temporary one, which is the
	// point: run in a project and Claude Code discovers its CLAUDE.md, its
	// settings, and its plugins, all to write eight words.
	Dir string
}

// NewClaudeCLI checks that the tool is actually there, so the failure is one
// message at startup rather than one per session.
func NewClaudeCLI() (*ClaudeCLI, error) {
	bin := "claude"
	if env := strings.TrimSpace(os.Getenv("SPIREWEB_CLAUDE_BIN")); env != "" {
		bin = env
	}
	if _, err := exec.LookPath(bin); err != nil {
		return nil, fmt.Errorf("%s is not on PATH: %w", bin, err)
	}
	return &ClaudeCLI{Bin: bin, Model: DefaultModel}, nil
}

func (c *ClaudeCLI) Name() string {
	model := c.Model
	if model == "" {
		model = "default"
	}
	return "claude-cli/" + model
}

// Summarize runs one prompt through the CLI.
func (c *ClaudeCLI) Summarize(ctx context.Context, slice string) (string, error) {
	if strings.TrimSpace(slice) == "" {
		return "", fmt.Errorf("empty session")
	}

	dir := c.Dir
	if dir == "" {
		// A directory with nothing in it: no CLAUDE.md to discover, no
		// project settings, nothing for the file tools to find interesting.
		tmp, err := os.MkdirTemp("", "spireweb-titles-")
		if err != nil {
			return "", err
		}
		defer func() { _ = os.RemoveAll(tmp) }()
		dir = tmp
	}

	bin := c.Bin
	if bin == "" {
		bin = "claude"
	}
	args := []string{
		"-p",
		"--output-format", "text",
		// Neither of these should be loaded to write a title, and both are
		// slow: MCP servers get started, settings files get read.
		"--strict-mcp-config",
		"--setting-sources", "",
	}
	if c.Model != "" {
		args = append(args, "--model", c.Model)
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	// On stdin rather than as an argument: the prompt is up to 10k characters
	// of someone else's conversation, and argv is not the place for that.
	cmd.Stdin = strings.NewReader(prompt(slice))

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return "", fmt.Errorf("%s: %w: %s", bin, err, msg)
	}

	title := Clean(stdout.String())
	if title == "" {
		return "", fmt.Errorf("no title in output")
	}
	return title, nil
}

// prompt puts the instructions and the conversation in one message.
//
// Not --system-prompt, which replaces Claude Code's own and produced an
// assistant that answered the transcript instead of titling it: given "how do
// I center a div" it explained flexbox.
//
// The instruction is repeated after the transcript, and the transcript is
// fenced on both sides. Without the closing half, a slice that stopped
// mid-sentence -- which is most of them, since it is cut at 10k characters --
// drew replies like "Your message cuts off mid-sentence. Could you complete
// the question?" as a title. Everything here is one message, so the last thing
// in it carries disproportionate weight; the end of the message should be the
// instruction rather than someone else's unfinished sentence.
func prompt(slice string) string {
	return systemPrompt +
		"\n\n--- begin transcript ---\n" + slice +
		"\n--- end transcript ---\n\n" +
		"Reply with the title for that transcript, and nothing else."
}

// CLIConcurrency is how many of these to run at once.
//
// Lower than DefaultConcurrency because each call is a Node process rather
// than an HTTP request, and because a subscription's rate limits are a shared
// budget with the interactive sessions the subscription is actually for.
const CLIConcurrency = 4
