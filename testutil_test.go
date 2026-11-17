package lmget

import (
	"runtime"
	"testing"
)

// setHome points the user's home at dir: HOME on Unix, USERPROFILE on
// Windows, where os.UserHomeDir reads it.
func setHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}

// skipOnWindows skips a test whose subject does not exist on Windows.
func skipOnWindows(t *testing.T, why string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("not on Windows: " + why)
	}
}
