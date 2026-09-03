package gateway

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrNoCredential reports that no credential is vaulted for a principal.
var ErrNoCredential = errors.New("no credential is vaulted for this principal")

// credentialFor reads the password Argus delegates for principal on asset.
//
// The point of reading it here, rather than holding it in the inventory or in
// memory from startup, is that the secret exists in this process only for the
// length of one connection. A gateway that loaded every password at boot would
// keep the whole estate's credentials resident for an attacker to find.
func credentialFor(asset Asset, principal string) (string, error) {
	if asset.CredentialDir == "" {
		return "", fmt.Errorf("%w: %s has no credential_dir", ErrNoCredential, asset.Hostname)
	}
	// The principal reaches here from a client-supplied cookie. Anything that
	// could climb out of the directory has to be refused rather than cleaned
	// up: a "sanitised" path that still resolves somewhere unexpected is worse
	// than a rejection, because it looks like it worked.
	if principal == "" || strings.ContainsAny(principal, `/\`) ||
		principal == "." || principal == ".." {
		return "", fmt.Errorf("%w: %q is not a usable principal name",
			ErrNoCredential, principal)
	}

	path := filepath.Join(asset.CredentialDir, principal)
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrNoCredential, path)
	}
	// A password every local account can read is not a vaulted credential. This
	// is the kind of thing that is set up correctly and then loosened by a
	// deployment script nobody re-reads.
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("credential %s is mode %04o; it must not be readable "+
			"by group or others", path, info.Mode().Perm())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read credential: %w", err)
	}
	// A trailing newline is what any editor leaves behind, and a password with
	// one is not the password.
	return strings.TrimRight(string(data), "\r\n"), nil
}
