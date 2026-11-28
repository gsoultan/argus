package agent

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
)

const (
	simFullUpgrade  = "apt-get -s -q -o Debug::NoLocking=1 -o APT::Get::Always-Include-Phased-Updates=true full-upgrade"
	realFullUpgrade = "apt-get -o DPkg::Lock::Timeout=120 -o APT::Get::Always-Include-Phased-Updates=true -y"
	notUpToDateSaid = "Checking for a new Ubuntu release\nPlease install all available updates for your release before upgrading.\n"
)

func TestParseAptFullSimulate(t *testing.T) {
	sim := parseAptFullSimulate([]byte(`Reading package lists...
The following packages have been kept back:
  postgresql-16 postgresql-client-16
  libpq5
The following packages will be REMOVED:
  libfoo1
The following packages will be upgraded:
  openssl
Remv libfoo1 [1.2-3]
Inst openssl [3.0.13-0ubuntu3.4] (3.0.13-0ubuntu3.5 Ubuntu:24.04/noble-updates [arm64])
Conf openssl (3.0.13-0ubuntu3.5 Ubuntu:24.04/noble-updates [arm64])
`))
	if len(sim.Install) != 1 || sim.Install[0].Name != "openssl" {
		t.Errorf("install = %+v", sim.Install)
	}
	if strings.Join(sim.Remove, ",") != "libfoo1" {
		t.Errorf("remove = %v", sim.Remove)
	}
	if strings.Join(sim.KeptBack, ",") != "libpq5,postgresql-16,postgresql-client-16" {
		t.Errorf("kept back = %v", sim.KeptBack)
	}
}

func TestParseAptPolicyHosts(t *testing.T) {
	got := parseAptPolicyHosts([]byte(`postgresql-16:
  Installed: 16.4-1.pgdg24.04+1
  Candidate: 16.6-1.pgdg24.04+1
  Version table:
     16.6-1.pgdg24.04+1 500
        500 http://apt.postgresql.org/pub/repos/apt noble-pgdg/main arm64 Packages
 *** 16.4-1.pgdg24.04+1 100
        100 /var/lib/dpkg/status
     16.3-0ubuntu0.24.04.1 500
        500 http://ports.ubuntu.com/ubuntu-ports noble-security/main arm64 Packages
openssl:
  Installed: 3.0.13-0ubuntu3.4
  Candidate: 3.0.13-0ubuntu3.5
  Version table:
     3.0.13-0ubuntu3.5 500
        500 http://ap-southeast-3a.clouds.ports.ubuntu.com/ubuntu-ports noble-updates/main arm64 Packages
`))
	if got["postgresql-16"] != "apt.postgresql.org" || got["openssl"] != "ap-southeast-3a.clouds.ports.ubuntu.com" {
		t.Fatalf("hosts = %v", got)
	}
	if ubuntuOrDebianHost(got["postgresql-16"]) || !ubuntuOrDebianHost(got["openssl"]) {
		t.Error("told the sources apart wrongly")
	}
}

// What the console's list leaves out (a phased update, one apt keeps back
// without a full upgrade) is installed before do-release-upgrade runs.
func TestApplyReleaseUbuntuCatchesUpFirst(t *testing.T) {
	h := newReleaseHost(t, jammy)
	installed := false
	upgradedAfter := false
	sr := &scriptRun{on: map[string]func(io.Writer, []string) int{
		"do-release-upgrade -c": func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "New release '24.04.1 LTS' available.")
			return 0
		},
		simFullUpgrade: func(w io.Writer, _ []string) int {
			if !installed {
				fmt.Fprintln(w, "Inst libssl3 [3.0.2-0ubuntu1.18] (3.0.2-0ubuntu1.19 Ubuntu:22.04/jammy-updates [arm64])")
				fmt.Fprintln(w, "Inst ubuntu-pro-client [33] (34 Ubuntu:22.04/jammy-updates [arm64])")
			}
			return 0
		},
		realFullUpgrade: func(_ io.Writer, env []string) int {
			installed = true
			return 0
		},
		"do-release-upgrade -f": func(io.Writer, []string) int {
			upgradedAfter = installed
			h.setOS(t, noble)
			return 0
		},
	}}
	a, rebooted := releaseApplier(t, sr, looks("apt-get", "dpkg-query", "do-release-upgrade"), "24.04")
	st := a.Apply(context.Background())
	if st.State != RunRebooting || !*rebooted {
		t.Fatalf("run = %+v", st)
	}
	if !upgradedAfter {
		t.Error("do-release-upgrade ran before the current release was up to date")
	}
	if !strings.Contains(st.Log, "installing 2 updates first: libssl3, ubuntu-pro-client") {
		t.Errorf("log = %s", st.Log)
	}
	found := false
	for _, c := range sr.calls {
		if strings.HasPrefix(c, realFullUpgrade) {
			found = strings.Contains(c, "--force-confold") && strings.HasSuffix(c, "full-upgrade")
		}
	}
	if !found {
		t.Errorf("no full upgrade keeping configuration files: %v", sr.calls)
	}
}

// A full upgrade of the current release that would remove a package is a
// person's call: nothing runs.
func TestApplyReleaseUbuntuCatchUpWouldRemove(t *testing.T) {
	newReleaseHost(t, jammy)
	sr := &scriptRun{on: map[string]func(io.Writer, []string) int{
		"do-release-upgrade -c": func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "New release '24.04.1 LTS' available.")
			return 0
		},
		simFullUpgrade: func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "Remv postgresql-16-postgis-3 [3.4.2+dfsg-1.pgdg22.04+1]")
			fmt.Fprintln(w, "Inst libgdal30 [3.4.1] (3.4.3 Ubuntu:22.04/jammy-updates [arm64])")
			return 0
		},
	}}
	a, rebooted := releaseApplier(t, sr, looks("apt-get", "dpkg-query", "do-release-upgrade"), "24.04")
	st := a.Apply(context.Background())
	if st.State != RunFailed || *rebooted || !strings.Contains(st.Message, "would remove postgresql-16-postgis-3") {
		t.Fatalf("run = %+v", st)
	}
	if st.Outcome == nil || st.Outcome.Changed != ChangedNo || len(st.Outcome.Next) == 0 {
		t.Fatalf("outcome = %+v", st.Outcome)
	}
	if sr.ran(realFullUpgrade) || sr.ran("do-release-upgrade -f") {
		t.Errorf("went ahead: %v", sr.calls)
	}
}

// do-release-upgrade still says the release is not up to date: the run
// names each package left, why, and what to run.
func TestApplyReleaseUbuntuNotUpToDateNamesTheBlockers(t *testing.T) {
	newReleaseHost(t, jammy)
	dpkgSaid(t)
	sr := &scriptRun{on: map[string]func(io.Writer, []string) int{
		"do-release-upgrade -c": func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "New release '24.04.1 LTS' available.")
			return 0
		},
		simFullUpgrade: func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "The following packages have been kept back:\n  nginx postgresql-16\n0 upgraded")
			return 0
		},
		"apt-mark showhold": func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "nginx")
			return 0
		},
		"apt-cache policy": func(w io.Writer, _ []string) int {
			fmt.Fprint(w, "nginx:\n  Installed: 1.18.0-6ubuntu14.4\n  Candidate: 1.18.0-6ubuntu14.5\n  Version table:\n"+
				"     1.18.0-6ubuntu14.5 500\n        500 http://ports.ubuntu.com/ubuntu-ports jammy-updates/main arm64 Packages\n"+
				"postgresql-16:\n  Installed: 16.4-1.pgdg22.04+1\n  Candidate: 16.6-1.pgdg22.04+1\n  Version table:\n"+
				"     16.6-1.pgdg22.04+1 500\n        500 http://apt.postgresql.org/pub/repos/apt jammy-pgdg/main arm64 Packages\n")
			return 0
		},
		"do-release-upgrade -f": func(w io.Writer, _ []string) int {
			fmt.Fprint(w, notUpToDateSaid)
			return 1
		},
	}}
	a, rebooted := releaseApplier(t, sr, looks("apt-get", "dpkg-query", "do-release-upgrade"), "24.04")
	st := a.Apply(context.Background())
	if st.State != RunFailed || *rebooted {
		t.Fatalf("run = %+v", st)
	}
	for _, want := range []string{"Still waiting: nginx (held), postgresql-16 (from apt.postgresql.org)",
		"`sudo apt-mark unhold nginx`", "`sudo apt install --only-upgrade postgresql-16`",
		"Always-Include-Phased-Updates=true full-upgrade"} {
		if !strings.Contains(st.Message, want) {
			t.Errorf("message lacks %q: %s", want, st.Message)
		}
	}
	if st.Outcome == nil || st.Outcome.Changed != ChangedNo || !strings.Contains(st.Outcome.Why, "postgresql-16") ||
		len(st.Outcome.Next) < 3 {
		t.Fatalf("outcome = %+v", st.Outcome)
	}
	if sr.ran("dpkg --force-confdef --force-confold --configure -a") {
		t.Error("repaired a server the upgrader did not touch")
	}
}

// Argus installed updates first, then the upgrader refused anyway: the run
// says the server changed and is still on its release.
func TestApplyReleaseUbuntuNotUpToDateAfterCatchingUp(t *testing.T) {
	newReleaseHost(t, jammy)
	installed := false
	sr := &scriptRun{on: map[string]func(io.Writer, []string) int{
		"do-release-upgrade -c": func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "New release '24.04.1 LTS' available.")
			return 0
		},
		simFullUpgrade: func(w io.Writer, _ []string) int {
			if !installed {
				fmt.Fprintln(w, "Inst libssl3 [3.0.2-0ubuntu1.18] (3.0.2-0ubuntu1.19 Ubuntu:22.04/jammy-updates [arm64])")
			}
			return 0
		},
		realFullUpgrade: func(io.Writer, []string) int { installed = true; return 0 },
		"do-release-upgrade -f": func(w io.Writer, _ []string) int {
			fmt.Fprint(w, notUpToDateSaid)
			return 1
		},
	}}
	a, _ := releaseApplier(t, sr, looks("apt-get", "dpkg-query", "do-release-upgrade"), "24.04")
	st := a.Apply(context.Background())
	if st.State != RunFailed || st.Outcome == nil || st.Outcome.Changed != ChangedYes {
		t.Fatalf("run = %+v outcome %+v", st, st.Outcome)
	}
	for _, want := range []string{"apt now lists nothing waiting", "main.log",
		"Argus installed 1 update for Ubuntu 22.04.5 LTS, and it stays installed; the server is still on Ubuntu 22.04.5 LTS"} {
		if !strings.Contains(st.Message, want) {
			t.Errorf("message lacks %q: %s", want, st.Message)
		}
	}
}

// The full upgrade of the current release fails before dpkg changes
// anything: nothing changed, and the upgrader does not run.
func TestApplyReleaseUbuntuCatchUpFails(t *testing.T) {
	newReleaseHost(t, jammy)
	dpkgSaid(t)
	sr := &scriptRun{on: map[string]func(io.Writer, []string) int{
		"do-release-upgrade -c": func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "New release '24.04.1 LTS' available.")
			return 0
		},
		simFullUpgrade: func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "Inst postgresql-16 [16.4-1.pgdg22.04+1] (16.6-1.pgdg22.04+1 apt.postgresql.org:jammy-pgdg [arm64])")
			return 0
		},
		realFullUpgrade: func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "E: Failed to fetch http://apt.postgresql.org/pub/repos/apt/pool/main/p/postgresql-16.deb 404 Not Found")
			return 100
		},
	}}
	a, rebooted := releaseApplier(t, sr, looks("apt-get", "dpkg-query", "do-release-upgrade"), "24.04")
	st := a.Apply(context.Background())
	if st.State != RunFailed || *rebooted || !strings.Contains(st.Message, "before any package changed") ||
		!strings.Contains(st.Message, "404 Not Found") {
		t.Fatalf("run = %+v", st)
	}
	if st.Outcome == nil || st.Outcome.Changed != ChangedNo {
		t.Fatalf("outcome = %+v", st.Outcome)
	}
	if sr.ran("do-release-upgrade -f") {
		t.Error("upgraded a release that is not up to date")
	}
}
