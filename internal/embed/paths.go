package embed

import "runtime"

// extensionFile is the sqlite-lembed shared library name for this platform.
//
// Only the filename differs between platforms; the SQL interface is the same.
// See mise-tasks/setup, which builds it from the landrix fork.
func extensionFile() string {
	switch runtime.GOOS {
	case "darwin":
		return "lembed0.dylib"
	case "windows":
		return "lembed0.dll"
	default:
		return "lembed0.so"
	}
}
