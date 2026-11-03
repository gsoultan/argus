//go:build !windows

package agent

import "io"

// Screen sharing is the Windows console's; elsewhere there is none to share.
func platformScreenHost() screenHost { return nil }

// RunScreenHelper is not used off Windows.
func RunScreenHelper(io.Reader, io.Writer) int { return 2 }
