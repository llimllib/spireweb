package index

import (
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
)

// sqlite-vec is compiled with -DSQLITE_CORE, so it calls SQLite's symbols
// directly and takes its struct layouts from whichever sqlite3.h the
// preprocessor found. Nothing in the build declares which one that is: the
// macOS SDK ships 3.51, Debian ships 3.40, and the library actually linked is
// go-sqlite3's bundled 3.53.4.
//
// Older header against newer library is the direction SQLite supports, since
// the C API only grows and an older header sees a prefix of it. The reverse
// is not, and neither is silently depending on whatever a given machine
// happens to have. mise stages the matching header and points CGO_CFLAGS at
// it; this checks the staging actually agrees with the library, because a
// mismatch here surfaces as corrupted structs rather than a build error.
func TestStagedHeaderMatchesLinkedLibrary(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file")
	}
	root := filepath.Join(filepath.Dir(thisFile), "..", "..")
	header := filepath.Join(root, "third_party", "sqlite", "sqlite3.h")

	src, err := os.ReadFile(header)
	if err != nil {
		t.Skipf("no staged header (run 'mise run sqlite-header'): %v", err)
	}

	m := regexp.MustCompile(`#define SQLITE_VERSION\s+"([^"]+)"`).FindSubmatch(src)
	if m == nil {
		t.Fatalf("no SQLITE_VERSION in %s", header)
	}
	headerVersion := string(m[1])

	db, err := sql.Open(DriverName, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var linked string
	if err := db.QueryRow(`SELECT sqlite_version()`).Scan(&linked); err != nil {
		t.Fatal(err)
	}

	if headerVersion != linked {
		t.Errorf("staged sqlite3.h is %s but the linked library is %s;\n"+
			"run 'mise run sqlite-header' after changing go-sqlite3",
			headerVersion, linked)
	}
}
