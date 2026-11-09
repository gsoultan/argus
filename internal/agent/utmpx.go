package agent

import (
	"encoding/binary"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
utmpx as macOS keeps it in /var/run/utmpx. Every record has the same start:

	0   [256]  ut_user
	256 [4]    ut_id
	260 [32]   ut_line
	292 int32  ut_pid
	296 int16  ut_type

and then a timeval, ut_host [256] and 64 bytes of padding. Libc writes the
file with a 32-bit timeval (628-byte records) so 32- and 64-bit programs
share it; a 64-bit struct as it is in memory would be 640 bytes. The parser
takes whichever size the file is made of, so a change in the layout shows
as "could not read" rather than as nobody being signed in.

The machine's own screen is the login whose line is "console".
*/

const (
	utmpxRecord32    = 628
	utmpxRecord64    = 640
	utmpxUserProcess = 7
	utmpxMaxType     = 11
)

// utmpxLayout is the record size of a utmpx file, or 0 when the bytes are
// neither layout.
func utmpxLayout(b []byte) int {
	for _, size := range []int{utmpxRecord32, utmpxRecord64} {
		if len(b) == 0 || len(b)%size != 0 {
			continue
		}
		ok := true
		for off := 0; off < len(b) && ok; off += size {
			r := b[off : off+size]
			typ := int16(binary.LittleEndian.Uint16(r[296:298]))
			ok = typ >= 0 && typ <= utmpxMaxType && utf8.ValidString(cstr(r[0:256])) && utf8.ValidString(cstr(r[260:292]))
		}
		if ok {
			return size
		}
	}
	return 0
}

// parseUtmpx returns the logged-in users in a macOS utmpx file, and false
// when the bytes are not one. A pure function of the bytes so that it is
// tested on any platform.
func parseUtmpx(b []byte) ([]tunnel.Logon, bool) {
	if len(b) == 0 {
		return nil, true
	}
	size := utmpxLayout(b)
	if size == 0 {
		return nil, false
	}
	var out []tunnel.Logon
	for off := 0; off+size <= len(b) && len(out) < tunnel.MaxLogons; off += size {
		r := b[off : off+size]
		if int16(binary.LittleEndian.Uint16(r[296:298])) != utmpxUserProcess {
			continue
		}
		user := cstr(r[0:256])
		if user == "" {
			continue
		}
		line := cstr(r[260:292])
		var sec int64
		var host string
		if size == utmpxRecord32 {
			sec = int64(int32(binary.LittleEndian.Uint32(r[300:304])))
			host = cstr(r[308:564])
		} else {
			sec = int64(binary.LittleEndian.Uint64(r[304:312]))
			host = cstr(r[320:576])
		}
		l := tunnel.Logon{
			Account: user,
			State:   tunnel.LogonActive,
			Line:    line,
			From:    host,
			Remote:  host != "",
			Console: line == "console",
		}
		if sec > 0 {
			t := time.Unix(sec, 0).UTC()
			l.LogonAt = &t
		}
		out = append(out, l)
	}
	return out, true
}

// withConsoleOwner adds the owner of the screen, when someone other than
// root has it and utmpx did not already say so: the login window hands
// /dev/console to whoever signs in, so this holds even if utmpx is behind.
func withConsoleOwner(logons []tunnel.Logon, owner string) []tunnel.Logon {
	if owner == "" || owner == "root" {
		return logons
	}
	for _, l := range logons {
		if l.Console && l.Account == owner {
			return logons
		}
	}
	return append([]tunnel.Logon{{Account: owner, State: tunnel.LogonActive, Line: "console", Console: true}}, logons...)
}

// parseMacAnswer reads what the dialog script printed: "Allow", "No" or
// "timeout". Anything else is an error, which declines.
func parseMacAnswer(out string) (allowed, answered bool, err error) {
	switch strings.TrimSpace(out) {
	case "Allow":
		return true, true, nil
	case "No":
		return false, true, nil
	case "timeout":
		return false, false, nil
	}
	return false, false, errors.New("the question gave no answer: " + strings.TrimSpace(out))
}
