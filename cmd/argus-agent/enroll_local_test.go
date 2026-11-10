//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

/*
The helper on argus-control's own server reads a file argus-control's
unprivileged account wrote, as root. It takes it only as that account left
it: no symlink, nobody else able to write it, a sane size, https only.
*/

const goodRequest = `{"controlUrl":"https://127.0.0.1:8080","token":"argus-enrol-x","pin":"p","hostname":"ctl"}`

func writeReq(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "enroll.json")
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTheControlHostRequestIsReadAsWritten(t *testing.T) {
	r, err := readControlHostRequest(writeReq(t, goodRequest, 0o600))
	if err != nil || r.Token != "argus-enrol-x" || r.Hostname != "ctl" || r.Pin != "p" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestTheControlHostRequestRefusesWhatArgusControlDidNotLeave(t *testing.T) {
	good := writeReq(t, goodRequest, 0o600)
	link := filepath.Join(t.TempDir(), "enroll.json")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	big := `{"controlUrl":"https://a","token":"t","hostname":"h","pad":"` +
		strings.Repeat("x", maxControlHostRequest) + `"}`
	for name, c := range map[string]struct {
		path string
		want string
	}{
		"a symlink":           {link, ""},
		"writable by others":  {writeReq(t, goodRequest, 0o666), "writable by others"},
		"writable by a group": {writeReq(t, goodRequest, 0o620), "writable by others"},
		"plain http": {writeReq(t,
			`{"controlUrl":"http://127.0.0.1:8080","token":"t","hostname":"h"}`, 0o600), "https"},
		"no token":        {writeReq(t, `{"controlUrl":"https://a","hostname":"h"}`, 0o600), "no token"},
		"too large":       {writeReq(t, big, 0o600), "larger"},
		"missing":         {filepath.Join(t.TempDir(), "none.json"), "has not asked"},
		"not json at all": {writeReq(t, "token=1", 0o600), ""},
	} {
		_, err := readControlHostRequest(c.path)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if c.want != "" && !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want it to say %q", name, err, c.want)
		}
	}
}
