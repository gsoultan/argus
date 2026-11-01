package agent

import "context"

// SystemdOutside is nil on Windows: there is no systemd, and personal
// accounts are not made here yet.
func SystemdOutside(string) func(context.Context, []byte) ([]byte, []byte, error) { return nil }
