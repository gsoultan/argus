package agent

import (
	"bytes"
	"encoding/binary"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
utmp as glibc writes it on every 64-bit Linux Argus supports (x86-64 and
arm64 share the layout): 384-byte records, little-endian.

	0   int16  ut_type
	4   int32  ut_pid
	8   [32]   ut_line
	40  [4]    ut_id
	44  [32]   ut_user
	76  [256]  ut_host
	332 exit, session
	340 int32  tv_sec
	344 int32  tv_usec
	348 [16]   ut_addr_v6
*/

const (
	utmpRecord      = 384
	utmpUserProcess = 7
)

// parseUtmp returns the logged-in users in a utmp file. It is a pure
// function of the bytes so that it is tested on any platform.
func parseUtmp(b []byte) []tunnel.Logon {
	var out []tunnel.Logon
	for off := 0; off+utmpRecord <= len(b) && len(out) < tunnel.MaxLogons; off += utmpRecord {
		r := b[off : off+utmpRecord]
		if int16(binary.LittleEndian.Uint16(r[0:2])) != utmpUserProcess {
			continue
		}
		user := cstr(r[44:76])
		if user == "" {
			continue
		}
		line := cstr(r[8:40])
		host := cstr(r[76:332])
		l := tunnel.Logon{
			Account: user,
			State:   tunnel.LogonActive,
			Line:    line,
			From:    host,
			Remote:  host != "",
			// A login on the machine's own screen: tty1, or the X display.
			Console: host == "" || strings.HasPrefix(host, ":"),
		}
		if sec := int32(binary.LittleEndian.Uint32(r[340:344])); sec > 0 {
			t := time.Unix(int64(sec), 0).UTC()
			l.LogonAt = &t
		}
		out = append(out, l)
	}
	return out
}

func cstr(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return strings.TrimSpace(string(b))
}
