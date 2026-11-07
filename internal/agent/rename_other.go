//go:build !linux

package agent

// renameExclusive moves from to to; the caller has checked that to does
// not exist.
func renameExclusive(from, to string) error { return renamePlain(from, to) }
