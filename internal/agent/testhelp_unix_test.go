//go:build !windows

package agent

import (
	"os"
	"testing"
)

// shortTempRoot keeps socket paths under the ~104-byte limit on macOS.
const shortTempRoot = "/tmp"

func assertPrivateFile(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("%s is %v; the credential must be root-only", path, info.Mode().Perm())
	}
}
