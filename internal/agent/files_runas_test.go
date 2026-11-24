//go:build !windows

package agent

import (
	"bytes"
	"context"
	"os/user"
	"strings"
	"testing"
	"time"
)

// A helper that outlives its deadline, or leaves a child holding its output,
// no longer keeps the agent waiting: runAs returns soon after ctx ends and
// says the helper ran out of time.
func TestRunAsReturnsWhenItsDeadlinePasses(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	acct, err := lookupAccount(me.Username)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	start := time.Now()
	err = runAs(ctx, helperRun{acct: acct, helper: "/bin/sh", spawn: SpawnDirect,
		args: []string{"-c", "sleep 30 & sleep 30"}, stdin: strings.NewReader(""), stdout: &out})
	took := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "did not finish in time") {
		t.Errorf("err = %v", err)
	}
	if took > helperWaitDelay+5*time.Second {
		t.Errorf("runAs returned after %s", took)
	}
}
