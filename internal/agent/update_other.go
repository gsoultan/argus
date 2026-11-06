//go:build !linux && !windows

package agent

import (
	"context"
	"errors"
	"io"
	"runtime"
)

// Updating from the console is Linux with systemd and Windows as a service.
// macOS
// agents keep the upgrade they had: the archive, by hand.
type otherUpdates struct{}

func hostUpdatePlatform(string) updatePlatform { return otherUpdates{} }

func (otherUpdates) check() string {
	return "updating the agent from the console works on Linux and Windows servers for now; on " +
		runtime.GOOS + ", install the new version by hand"
}
func (otherUpdates) restartCheck() string {
	return "restarting the agent from the console works on Linux and Windows servers for now; on " +
		runtime.GOOS + ", restart its service by hand"
}
func (otherUpdates) format() string         { return "" }
func (otherUpdates) fileName(string) string { return "" }
func (otherUpdates) launch(context.Context, string) error {
	return errors.New("not supported on " + runtime.GOOS)
}

// RunUpdateApply is not used here.
func RunUpdateApply(string, io.Writer) int { return 2 }
