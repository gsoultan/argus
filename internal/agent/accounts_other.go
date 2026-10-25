//go:build !darwin && !windows

package agent

import "errors"

// directoryPasswd is never reached here: systemPasswd names a file.
func directoryPasswd() ([]byte, error) {
	return nil, errors.New("no account file configured")
}
