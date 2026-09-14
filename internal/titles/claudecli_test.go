package titles

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeClaude writes a stand-in for the CLI that records how it was called.
// Driving the real one would spend the subscription this exists to use.
func fakeClaude(t *testing.T, script string) (bin, logPath string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "claude")
	logPath = filepath.Join(dir, "call.log")

	body := "#!/usr/bin/env bash\n" +
		"{ echo \"ARGS: $*\"; echo '--- stdin ---'; cat; } > " + logPath + "\n" + script + "\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, logPath
}

func TestClaudeCLISummarize(t *testing.T) {
	bin, logPath := fakeClaude(t, `echo '  "Centering a div with flexbox."  '`)
	c := &ClaudeCLI{Bin: bin, Model: "claude-haiku-4-5-20251001"}

	got, err := c.Summarize(context.Background(), "user: how do I center a div")
	if err != nil {
		t.Fatal(err)
	}
	// Cleaned on the way out, the same as the API backend: a title is one
	// short line, whatever the model returns.
	if got != "Centering a div with flexbox" {
		t.Errorf("Summarize() = %q", got)
	}

	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	call := string(log)

	for _, want := range []string{"-p", "--output-format text", "--strict-mcp-config", "--model claude-haiku-4-5-20251001"} {
		if !strings.Contains(call, want) {
			t.Errorf("call is missing %q:\n%s", want, call)
		}
	}
	// The conversation goes on stdin, not argv.
	stdin := call[strings.Index(call, "--- stdin ---"):]
	if !strings.Contains(stdin, "how do I center a div") {
		t.Error("the transcript did not reach stdin")
	}
	if !strings.Contains(stdin, "title") {
		t.Error("the instructions did not reach stdin")
	}
	if strings.Contains(strings.SplitN(call, "--- stdin ---", 2)[0], "center a div") {
		t.Error("the transcript was passed as an argument")
	}
}

// Claude Code reads CLAUDE.md, project settings and plugins from its working
// directory. None of that belongs in a request to write eight words, and in a
// large repository it is most of the latency.
func TestClaudeCLIDefaultsToATempDir(t *testing.T) {
	dir := t.TempDir()
	listing := filepath.Join(dir, "listing")
	bin, _ := fakeClaude(t, `ls -A . > `+listing+`; echo "A title"`)

	c := &ClaudeCLI{Bin: bin}
	if _, err := c.Summarize(context.Background(), "user: hello"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(listing)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != "" {
		t.Errorf("working directory was not empty: %q", got)
	}
}

func TestClaudeCLIReportsFailures(t *testing.T) {
	bin, _ := fakeClaude(t, `echo "Credit balance is too low" >&2; exit 1`)
	c := &ClaudeCLI{Bin: bin}

	_, err := c.Summarize(context.Background(), "user: hello")
	if err == nil {
		t.Fatal("want an error")
	}
	// The reason has to survive: a whole pass failing for one reason is the
	// common case, and "1162 sessions could not be summarized" is not a
	// diagnosis.
	if !strings.Contains(err.Error(), "Credit balance is too low") {
		t.Errorf("error = %v, want the tool's own message", err)
	}
}

func TestClaudeCLIEmptyOutputIsAnError(t *testing.T) {
	bin, _ := fakeClaude(t, `echo ""`)
	c := &ClaudeCLI{Bin: bin}
	if _, err := c.Summarize(context.Background(), "user: hello"); err == nil {
		t.Error("empty output was accepted as a title")
	}
}

func TestClaudeCLIHonoursCancellation(t *testing.T) {
	bin, _ := fakeClaude(t, `sleep 30`)
	c := &ClaudeCLI{Bin: bin}

	ctx, cancel := context.WithCancel(context.Background())
	go cancel()
	if _, err := c.Summarize(ctx, "user: hello"); err == nil {
		t.Error("want an error once the context is cancelled")
	}
}
