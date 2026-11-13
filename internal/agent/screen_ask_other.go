//go:build !windows && !darwin && !linux

package agent

import (
	"context"
	"errors"

	"github.com/gsoultan/argus/internal/tunnel"
)

func askOnDesktop(context.Context, tunnel.Logon, string, string, int) (bool, bool, error) {
	return false, false, errors.New("asking at the machine is not done on this system")
}
