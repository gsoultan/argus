//go:build linux

package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"os/user"
	"strconv"
	"time"
)

// ReadUnrecordedLogins returns the logins shims let in while the daemon was
// down, from the journal since the given time.
//
// The account is the journal's own record of who wrote the line (_UID), not
// what the line says: journald sets it from the socket, so a user cannot
// write a line that names somebody else. Everything else is the shim's claim.
func ReadUnrecordedLogins(ctx context.Context, since time.Time) ([]UnrecordedLogin, error) {
	if _, err := exec.LookPath("journalctl"); err != nil {
		return nil, nil
	}
	out, err := exec.CommandContext(ctx, "journalctl", "--no-pager", "-o", "json",
		"-t", UnrecordedTag, "--since", fmt.Sprintf("@%d", since.Unix())).Output()
	if err != nil {
		return nil, err
	}
	var logins []UnrecordedLogin
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e struct {
			Message string `json:"MESSAGE"`
			UID     string `json:"_UID"`
			Time    string `json:"__REALTIME_TIMESTAMP"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		var l UnrecordedLogin
		_ = json.Unmarshal([]byte(e.Message), &l)
		if u, err := user.LookupId(e.UID); err == nil {
			l.Principal = u.Username
		} else if e.UID != "" {
			l.Principal = "uid " + e.UID
		}
		if us, err := strconv.ParseInt(e.Time, 10, 64); err == nil {
			l.At = time.UnixMicro(us).UTC()
		}
		if l.Principal != "" {
			logins = append(logins, l)
		}
	}
	return logins, sc.Err()
}
