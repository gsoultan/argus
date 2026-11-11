//go:build windows

package main

import (
	"errors"
	"os"
)

// openNoFollow: the control server's helper is Linux only.
func openNoFollow(string) (*os.File, error) {
	return nil, errors.New("enroll-local is for argus-control's own server, which runs on Linux")
}

func checkRequestOwner(*os.File) error { return nil }
