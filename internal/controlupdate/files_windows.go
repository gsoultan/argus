package controlupdate

import "os"

// The helper runs on Linux only; on Windows the control plane reads and
// writes these files for itself and nobody else shares the directory.
func openNoFollow(path string) (*os.File, error) { return os.Open(path) }

func chownLike(string, string) error { return nil }
