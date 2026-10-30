//go:build !windows

package agent

import (
	"encoding/json"
	"log/syslog"
	"time"
)

// noteUnrecordedLogin writes the login to the system log, the one place an
// unprivileged process can leave a record the daemon finds later. Best
// effort: the login goes ahead either way, because refusing it is what this
// path exists not to do.
func noteUnrecordedLogin(principal, clientAddr, command string) {
	w, err := syslog.New(syslog.LOG_AUTHPRIV|syslog.LOG_WARNING, UnrecordedTag)
	if err != nil {
		return
	}
	defer w.Close()
	b, _ := json.Marshal(UnrecordedLogin{Principal: principal, ClientAddr: clientAddr,
		Command: command, At: time.Now().UTC()})
	_ = w.Warning(string(b))
}
