//go:build linux

package agent

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"testing"
	"time"
)

// The account comes from the journal's _UID, which journald sets, not from
// the line, which whoever wrote it chose.
func TestUnrecordedLoginsNameTheAccountJournaldSaw(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	script := `#!/bin/sh
echo '{"MESSAGE":"{\"principal\":\"mallory\",\"client_addr\":\"203.0.113.9\",\"command\":\"id\"}","_UID":"` + me.Uid + `","__REALTIME_TIMESTAMP":"1761818400000000"}'
echo 'not json'
`
	if err := os.WriteFile(filepath.Join(dir, "journalctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	logins, err := ReadUnrecordedLogins(context.Background(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(logins) != 1 {
		t.Fatalf("got %d logins: %+v", len(logins), logins)
	}
	l := logins[0]
	if l.Principal != me.Username || l.ClientAddr != "203.0.113.9" || l.Command != "id" {
		t.Errorf("login = %+v; want the journal's account %s, not the line's", l, me.Username)
	}
	if !l.At.Equal(time.UnixMicro(1761818400000000)) {
		t.Errorf("at = %v", l.At)
	}
}
