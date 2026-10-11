package secrets

import (
	"fmt"
	"os"
)

// CheckPrivate refuses a secret file that anyone but its owner can read.
//
// Every private key Argus loads — the gateway host key, the CA key that mints
// session certificates, the injected credentials, the TLS keys — was read with
// os.ReadFile and no look at its mode. A world-readable CA key is a complete
// compromise of the product: anyone on the host can mint a certificate for any
// principal on any target. The Settings page tells operators to treat gateway
// nodes as their highest-value asset; this is one line of that threat model
// the binary can enforce itself rather than leaving to prose.
//
// Refused at startup rather than warned about, because a warning in a log
// nobody reads at 3am is how a permission gets loosened by a deployment script
// and stays that way. A process that will not start gets fixed.
//
// Group and other bits are what matter. Ownership is deliberately not checked:
// containers and init systems routinely run as a uid that differs from the
// file's owner, and refusing on that would break more correct deployments than
// it would catch bad ones.
//
// On Windows the mode bits say nothing about who can read a file -- Go reports
// 0666 for any writable file -- so there the file's access list is read
// instead; see private_windows.go.
func CheckPrivate(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory, expected a private key file", path)
	}
	return ownerOnly(path, info)
}

// ReadPrivate is ReadFile with the mode check first.
func ReadPrivate(path string) ([]byte, error) {
	if err := CheckPrivate(path); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}
