//go:build !linux

package agent

import (
	"context"
	"time"
)

// ReadUnrecordedLogins has no journal to read here. A login let in while the
// daemon was down is still in the system log, under UnrecordedTag.
func ReadUnrecordedLogins(context.Context, time.Time) ([]UnrecordedLogin, error) {
	return nil, nil
}
