//go:build windows

package agent

import "os"

func chownLike(string, os.FileInfo) error { return nil }
