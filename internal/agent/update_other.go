//go:build !linux

package agent

import (
	"context"
	"errors"
	"io"
	"runtime"
)

// Updating from the console is Linux with systemd for now. macOS and Windows
// agents keep the upgrade they had: the package or installer, by hand.
type otherUpdates struct{}

func hostUpdatePlatform(string) updatePlatform { return otherUpdates{} }

func (otherUpdates) check() string {
	return "updating the agent from the console works on Linux servers for now; on " +
		runtime.GOOS + ", install the new version by hand"
}
func (otherUpdates) format() string         { return "" }
func (otherUpdates) fileName(string) string { return "" }
func (otherUpdates) launch(context.Context, string) error {
	return errors.New("not supported on " + runtime.GOOS)
}

// RunUpdateApply is not used off Linux.
func RunUpdateApply(string, io.Writer) int { return 2 }
