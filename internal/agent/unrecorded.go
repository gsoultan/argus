package agent

import "time"

// UnrecordedTag is the system-log tag a shim writes a login under when it
// let the login in without the daemon. See ReadUnrecordedLogins.
const UnrecordedTag = "argus-shim"

// UnrecordedLogin is what a shim writes, and the daemon reads back.
type UnrecordedLogin struct {
	Principal  string    `json:"principal"`
	ClientAddr string    `json:"client_addr"`
	Command    string    `json:"command"`
	At         time.Time `json:"at"`
}
