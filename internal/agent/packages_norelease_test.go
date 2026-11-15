package agent

import (
	"strings"
	"testing"
)

const aptNoRelease = `Hit:1 http://security.ubuntu.com/ubuntu noble-security InRelease
Ign:2 http://id.archive.ubuntu.com/ubuntu noble InRelease
Err:3 http://id.archive.ubuntu.com/ubuntu noble Release
  404  Not Found [IP: 103.28.149.66 80]
Reading package lists...
E: The repository 'http://id.archive.ubuntu.com/ubuntu noble Release' no longer has a Release file.
N: Updating from such a repository can't be done securely, and is therefore disabled by default.
E: The repository 'http://id.archive.ubuntu.com/ubuntu noble-updates Release' no longer has a Release file.
E: The repository 'http://id.archive.ubuntu.com/ubuntu noble-backports Release' no longer has a Release file.
`

func TestAptMissingReleaseAfterAnUpgrade(t *testing.T) {
	got := aptMissingRelease([]byte(aptNoRelease), map[string]string{
		"VERSION_CODENAME": "resolute", "PRETTY_NAME": "Ubuntu 26.04.1 LTS"})
	for _, want := range []string{"still name noble", "noble, noble-updates, noble-backports",
		"http://id.archive.ubuntu.com/ubuntu", "runs Ubuntu 26.04.1 LTS", "at resolute"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

func TestAptMissingReleaseFromABrokenMirror(t *testing.T) {
	got := aptMissingRelease([]byte(aptNoRelease), map[string]string{
		"VERSION_CODENAME": "noble", "PRETTY_NAME": "Ubuntu 24.04.3 LTS"})
	if !strings.Contains(got, "mirror http://id.archive.ubuntu.com/ubuntu no longer serves noble") ||
		!strings.Contains(got, "archive.ubuntu.com") || strings.Contains(got, "left them behind") {
		t.Errorf("got %q", got)
	}
}

func TestAptMissingReleaseOtherwise(t *testing.T) {
	if got := aptMissingRelease([]byte("E: Could not get lock /var/lib/apt/lists/lock\n"), nil); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestAptLockHeldByTheUpgrader(t *testing.T) {
	out := []byte("Reading package lists...\nE: Could not get lock /var/lib/apt/lists/lock. It is held by process 106866 (resolute)\nE: Unable to lock directory /var/lib/apt/lists/\n")
	why := aptLockHeld(out, func(pid string) string {
		if pid != "106866" {
			t.Errorf("pid %q", pid)
		}
		return "/usr/bin/python3 /tmp/ubuntu-release-upgrader-x1y2/resolute --mode=server --frontend=DistUpgradeViewNonInteractive"
	})
	if !strings.Contains(why, "operating system upgrade is running") || !strings.Contains(why, "do not stop it") {
		t.Errorf("why = %q", why)
	}
	why = aptLockHeld(out, func(string) string { return "/usr/bin/unattended-upgrade" })
	if !strings.Contains(why, "apt is busy: process 106866 (resolute)") {
		t.Errorf("why = %q", why)
	}
	if aptLockHeld([]byte("E: Something else\n"), func(string) string { return "" }) != "" {
		t.Error("read a lock that was not there")
	}
}
