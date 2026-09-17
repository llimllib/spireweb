package embed

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// install writes empty stand-ins for the extension and model. Their contents
// never matter to path resolution, only their existence.
func install(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{extensionFile(), ModelFile} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDefaultPathsPrefersAnInstallThatExists(t *testing.T) {
	dir := t.TempDir()
	install(t, dir)
	t.Setenv("SPIREWEB_DATA_DIR", dir)

	p := DefaultPaths()
	if p.Extension != filepath.Join(dir, extensionFile()) {
		t.Errorf("extension = %q, want it under %q", p.Extension, dir)
	}
	if err := p.Check(); err != nil {
		t.Errorf("Check() = %v, want nil", err)
	}
}

// With nothing installed anywhere, the reported path has to be the one 'mise
// run setup' fills, because the error names it and that is the whole value of
// the message.
//
// HOME is redirected as well as SPIREWEB_DATA_DIR: the machine running this
// very likely has a real install in ~/.local/share/spireweb, which would
// satisfy DefaultPaths and make the test assert nothing.
func TestDefaultPathsFallsBackToTheDevelopmentLocation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SPIREWEB_DATA_DIR", t.TempDir()) // exists, but empty

	p := DefaultPaths()
	err := p.Check()
	if err == nil {
		t.Fatal("Check() = nil, want an error naming where to install")
	}

	want := filepath.Join(home, ".local", "share", "spireweb")
	if filepath.Dir(p.Extension) != want {
		t.Errorf("fell back to %q, want %q", filepath.Dir(p.Extension), want)
	}
	if !strings.Contains(err.Error(), "mise run setup") {
		t.Errorf("Check() = %v, want it to say how to install", err)
	}
}

// The release layout: extension and model beside the binary. A Homebrew cask
// stages the archive and symlinks only spireweb onto PATH, so this is the only
// way the sibling files are found.
func TestCandidateDirsIncludesTheResolvedExecutablesDirectory(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip("no executable path")
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	want := filepath.Dir(exe)

	for _, dir := range candidateDirs() {
		if dir == want {
			return
		}
	}
	t.Errorf("candidateDirs() = %v, want it to include %q", candidateDirs(), want)
}

// SPIREWEB_DATA_DIR is how mise points a development build at a shared install,
// so it has to win over a stray file next to the binary.
func TestCandidateDirsPutsTheEnvironmentFirst(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SPIREWEB_DATA_DIR", dir)

	got := candidateDirs()
	if len(got) == 0 || got[0] != dir {
		t.Errorf("candidateDirs() = %v, want %q first", got, dir)
	}
}
