//go:build !linux

package agent

import (
	"context"
	"errors"
)

// personalSupported is false: making accounts on macOS and Windows is its own
// work, and until it is done the console is told so rather than shown a list
// of accounts that do not exist.
const personalSupported = false

func newAccountShell() string { return "/bin/sh" }

func writeAuthorizedKeys(passwdEntry, []string) error {
	return errors.New("not supported on this platform")
}

func writeSudoers(context.Context, *Provisioner, []string) error { return nil }
