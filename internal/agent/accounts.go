package agent

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
)

// openPasswd opens an account list in passwd(5) format.
//
// Every reader of accounts -- posture, discovery, the shim's login shell, the
// tunnel's account lookup -- goes through here, so they cannot disagree about
// who exists. A path reads that file. An empty path asks the platform's
// directory service: on macOS accounts live in Directory Services, and the
// /etc/passwd there lists only the few used in single-user mode, so reading it
// would report a Mac as having no one who can log in.
func openPasswd(path string) (io.ReadCloser, error) {
	if path != "" {
		return os.Open(path)
	}
	b, err := directoryPasswd()
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// dsclToPasswd converts `dscl . -readall /Users RecordName UniqueID
// PrimaryGroupID NFSHomeDirectory UserShell` into passwd(5) lines.
//
// The output is one block per record, separated by a line holding "-". An
// attribute is "Key: value", or "Key:" followed by indented values when it has
// more than one or the value is long; the first value is the one that counts
// (an account's short name comes first in RecordName). A record missing a
// name, home or shell is skipped rather than guessed at.
func dsclToPasswd(out []byte) []byte {
	var buf bytes.Buffer
	rec := map[string]string{}
	last := ""
	flush := func() {
		name := rec["RecordName"]
		if name != "" && rec["NFSHomeDirectory"] != "" && rec["UserShell"] != "" {
			fmt.Fprintf(&buf, "%s:*:%s:%s::%s:%s\n", name, rec["UniqueID"],
				rec["PrimaryGroupID"], rec["NFSHomeDirectory"], rec["UserShell"])
		}
		rec, last = map[string]string{}, ""
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "-":
			flush()
		case strings.HasPrefix(line, " "):
			if last != "" && rec[last] == "" {
				rec[last] = strings.TrimSpace(line)
			}
		default:
			k, v, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			last = k
			if _, seen := rec[k]; !seen {
				rec[k] = strings.TrimSpace(v)
			}
		}
	}
	flush()
	return buf.Bytes()
}
