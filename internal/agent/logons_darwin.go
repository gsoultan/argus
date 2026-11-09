//go:build darwin

package agent

import (
	"context"
	"errors"
	"os"
	"os/user"
	"runtime"
	"strconv"
	"syscall"

	"github.com/gsoultan/argus/internal/tunnel"
)

// utmpxPath is where macOS keeps who is logged in now.
const utmpxPath = "/var/run/utmpx"

type utmpxLogons struct{ path string }

func platformLogons() LogonsHost { return utmpxLogons{path: utmpxPath} }

// List reads utmpx and adds whoever owns the screen. Every SSH login is its
// own session, as on Linux; the one at the screen is the person a
// workstation asks. If neither can be read the answer is an error, so a
// question is never skipped because the agent could not see who was there.
func (u utmpxLogons) List(context.Context) (tunnel.LogonsResult, error) {
	res := tunnel.LogonsResult{Platform: runtime.GOOS}
	owner, ownerErr := consoleOwner()
	b, err := os.ReadFile(u.path)
	var logons []tunnel.Logon
	ok := err == nil
	if ok {
		logons, ok = parseUtmpx(b)
	}
	if !ok && ownerErr != nil {
		if err == nil {
			err = errors.New("its layout is not one this agent knows")
		}
		return res, errors.New("could not read who is logged in: " + err.Error())
	}
	res.Logons = withConsoleOwner(logons, owner)
	return res, nil
}

func (utmpxLogons) SignOut(context.Context, uint32) error {
	return errors.New("signing a session out is only done on Windows")
}

// consoleOwner is the account that owns /dev/console: the person signed in
// at the screen, or root at the login window.
func consoleOwner() (string, error) {
	fi, err := os.Stat("/dev/console")
	if err != nil {
		return "", err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("no owner for /dev/console")
	}
	u, err := user.LookupId(strconv.FormatUint(uint64(st.Uid), 10))
	if err != nil {
		return "", err
	}
	return u.Username, nil
}
