//go:build !windows

package agent

import (
	"context"
	"errors"
	"os"
	"runtime"

	"github.com/gsoultan/argus/internal/tunnel"
)

// utmpPath is where Linux keeps who is logged in now.
const utmpPath = "/var/run/utmp"

type utmpLogons struct{ path string }

func platformLogons() LogonsHost { return utmpLogons{path: utmpPath} }

// List reads utmp. Every SSH login is its own session on Linux, so there is
// no limit to report and nothing that would have to be signed out first.
func (u utmpLogons) List(context.Context) (tunnel.LogonsResult, error) {
	res := tunnel.LogonsResult{Platform: runtime.GOOS}
	b, err := os.ReadFile(u.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return res, nil
		}
		return res, errors.New("could not read who is logged in: " + err.Error())
	}
	res.Logons = parseUtmp(b)
	return res, nil
}

func (utmpLogons) SignOut(context.Context, uint32) error {
	return errors.New("signing a session out is only done on Windows")
}
