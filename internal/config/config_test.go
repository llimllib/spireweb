package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSaveAndLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.toml")
	want := Config{
		Dirs:   []string{"/Users/me/.claude/projects", "/Users/me/.pi/agent/sessions"},
		Titles: TitlesClaude,
		Addr:   "127.0.0.1:8765",
		Index:  "/Users/me/.local/share/spireweb/index.db",
	}
	if err := want.SaveTo(path); err != nil {
		t.Fatal(err)
	}

	got, had, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if !had {
		t.Error("LoadFrom reported no file after writing one")
	}
	if !slices.Equal(got.Dirs, want.Dirs) {
		t.Errorf("Dirs = %v, want %v", got.Dirs, want.Dirs)
	}
	if got.Titles != want.Titles || got.Addr != want.Addr || got.Index != want.Index {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// The file exists to be opened and read by someone wondering what spireweb
// decided. A list of paths answers "what" without ever answering "why".
func TestSaveExplainsItself(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := (Config{Dirs: []string{"/a"}, Titles: TitlesOff}).SaveTo(path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# spireweb settings", "dirs", "titles"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the written file does not mention %q:\n%s", want, b)
		}
	}
}

// Every machine is in this state before the first run, so it cannot be an
// error -- but the caller has to be able to tell it from a file that exists
// and configures nothing.
func TestLoadTreatsAMissingFileAsEmpty(t *testing.T) {
	got, had, err := LoadFrom(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("LoadFrom = %v, want no error for a missing file", err)
	}
	if had {
		t.Error("had = true for a file that does not exist")
	}
	if len(got.Dirs) != 0 {
		t.Errorf("Dirs = %v, want none", got.Dirs)
	}
}

// Ignoring a malformed file would quietly disregard what someone wrote and
// behave as though it were not there.
func TestLoadReportsAMalformedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("dirs = [unclosed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, had, err := LoadFrom(path)
	if err == nil {
		t.Fatal("LoadFrom = nil error for a malformed file")
	}
	if !had {
		t.Error("had = false; the file is there, it just does not parse")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the file", err)
	}
}

// XDG rather than ~/Library/Application Support, matching SPIREWEB_DATA_DIR
// and embed.DefaultPaths. Two halves of one install on two schemes would be
// gratuitous.
func TestPathFollowsXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	if got, want := Path(), filepath.Join("/tmp/xdg", "spireweb", "config.toml"); got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}

	t.Setenv("XDG_CONFIG_HOME", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got, want := Path(), filepath.Join(home, ".config", "spireweb", "config.toml"); got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}

// Paths can contain characters TOML gives meaning to.
func TestSaveQuotesAwkwardPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	awkward := `/Users/me/a "quoted" dir\with-backslash`
	if err := (Config{Dirs: []string{awkward}, Titles: TitlesOff}).SaveTo(path); err != nil {
		t.Fatal(err)
	}
	got, _, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Dirs) != 1 || got.Dirs[0] != awkward {
		t.Errorf("Dirs = %q, want %q", got.Dirs, awkward)
	}
}
