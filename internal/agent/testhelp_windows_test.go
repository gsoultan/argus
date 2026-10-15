package agent

import (
	"testing"

	"github.com/gsoultan/argus/internal/secrets"
)

const shortTempRoot = ""

// Mode bits mean nothing on Windows; the access list is what counts.
func assertPrivateFile(t *testing.T, path string) {
	t.Helper()
	if err := secrets.CheckPrivate(path); err != nil {
		t.Errorf("the credential must be private: %v", err)
	}
}
