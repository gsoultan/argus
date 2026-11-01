//go:build linux

package agent

import "testing"

// The agent builds its Provisioner with no Passwd, so "" has to read the
// server's own /etc/passwd. It read the (absent) account directory instead,
// and every account came back "the agent cannot read the server's account
// list" -- no personal account could be made, or locked when taken away.
//
// Checked through the list Reconcile reads rather than Reconcile itself, which
// on a real /etc/passwd would lock any Argus-made account it found.
func TestAProvisionerWithNoPasswdReadsTheServersAccounts(t *testing.T) {
	p := &Provisioner{}
	if got := p.passwd(); got != "/etc/passwd" {
		t.Fatalf("passwd() = %q, want /etc/passwd", got)
	}
	got, err := readPasswdEntries(p.passwd())
	if err != nil {
		t.Fatalf("cannot read the server's accounts: %v", err)
	}
	if _, ok := got["root"]; !ok {
		t.Errorf("root is not among the %d accounts read", len(got))
	}
}
