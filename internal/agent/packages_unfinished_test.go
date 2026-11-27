package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
)

const auditGrub = `The following packages are only half configured, probably due to problems
configuring them the first time.  The configuration should be retried using
dpkg --configure <package> or the configure menu option in dselect:
 grub-pc              GRand Unified Bootloader, version 2 (PC/BIOS version)
`

func TestDpkgFailed(t *testing.T) {
	log := `Setting up grub-pc (2.12-1ubuntu7) ...
dpkg: error processing package grub-pc (--configure):
 installed grub-pc package post-installation script subprocess returned error exit status 1
dpkg: error processing archive /var/cache/apt/archives/libc6_2.39-0ubuntu8_amd64.deb (--unpack):
Errors were encountered while processing:
 grub-pc
 linux-image-6.8.0-31-generic:amd64
E: Sub-process /usr/bin/dpkg returned an error code (1)
 not-a-package-line
`
	got := dpkgFailed([]byte(log))
	want := []string{"grub-pc", "libc6", "linux-image-6.8.0-31-generic"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dpkgFailed = %v, want %v", got, want)
	}
	if got := dpkgFailed([]byte("all fine\n")); got != nil {
		t.Errorf("nothing failed, got %v", got)
	}
}

// A reboot asked for from the console is refused while dpkg has packages
// half configured: the server may not come back.
func TestApplyRebootRefusedWhileDpkgIsUnfinished(t *testing.T) {
	newReleaseHost(t, noble)
	sr := &scriptRun{on: map[string]func(io.Writer, []string) int{
		"dpkg --audit": func(w io.Writer, _ []string) int { fmt.Fprint(w, auditGrub); return 1 },
	}}
	a, rebooted := releaseApplier(t, sr, looks("apt-get", "dpkg", "dpkg-query"), "")
	a.Plan.Mode = PackagesReboot
	st := a.Apply(context.Background())
	if *rebooted || st.State != RunFailed || !strings.Contains(st.Message, "grub-pc") ||
		!strings.Contains(st.Message, "not rebooted") {
		t.Fatalf("rebooted=%v run=%+v", *rebooted, st)
	}
	if st.Outcome == nil || st.Outcome.Changed != ChangedNo || st.Outcome.Next[0] != "Do not reboot yet." {
		t.Fatalf("outcome = %+v", st.Outcome)
	}
}

func TestApplyRebootWhenDpkgIsClean(t *testing.T) {
	newReleaseHost(t, noble)
	sr := &scriptRun{on: map[string]func(io.Writer, []string) int{}}
	a, rebooted := releaseApplier(t, sr, looks("apt-get", "dpkg", "dpkg-query"), "")
	a.Plan.Mode = PackagesReboot
	if st := a.Apply(context.Background()); !*rebooted || st.State != RunRebooting {
		t.Fatalf("rebooted=%v run=%+v", *rebooted, st)
	}
}

// do-release-upgrade fails in dpkg part way: the message names the package
// from the upgrader's terminal log and what is still half configured after
// Argus's repair, and the server is not rebooted.
func TestApplyReleaseUbuntuPartWayNamesWhatDpkgFailedOn(t *testing.T) {
	newReleaseHost(t, jammy)
	dpkgSaid(t, "upgrade libc6:amd64 2.35-0ubuntu3.8 2.39-0ubuntu8")
	broke := false
	sr := &scriptRun{on: map[string]func(io.Writer, []string) int{
		"do-release-upgrade -c": func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "New release '24.04.1 LTS' available.")
			return 0
		},
		"do-release-upgrade -f": func(w io.Writer, _ []string) int {
			term := "Setting up grub-pc (2.12-1ubuntu7) ...\ndpkg: error processing package grub-pc (--configure):\n" +
				" installed grub-pc package post-installation script subprocess returned error exit status 1\n"
			if err := os.WriteFile(ubuntuUpgradeLogs[1], []byte(term), 0o644); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintln(w, "E:Sub-process /usr/bin/dpkg returned an error code (1)")
			broke = true
			return 1
		},
		"dpkg --audit": func(w io.Writer, _ []string) int {
			if !broke {
				return 0
			}
			fmt.Fprint(w, auditGrub)
			return 1
		},
	}}
	a, rebooted := releaseApplier(t, sr, looks("apt-get", "dpkg", "dpkg-query", "do-release-upgrade"), "24.04")
	st := a.Apply(context.Background())
	if *rebooted || st.State != RunFailed {
		t.Fatalf("rebooted=%v run=%+v", *rebooted, st)
	}
	for _, want := range []string{"dpkg failed on grub-pc.", "Still half configured: grub-pc.",
		"the server may not start again if it reboots now", "Argus tried to finish configuring the packages and could not"} {
		if !strings.Contains(st.Message, want) {
			t.Errorf("message lacks %q: %s", want, st.Message)
		}
	}
}

// The upgrader exits 0 but leaves a package unconfigured: Argus repairs, and
// when that does not clear it, does not reboot into the new release.
func TestApplyReleaseDoesNotRebootIntoUnfinishedPackages(t *testing.T) {
	h := newReleaseHost(t, jammy)
	broke := false
	sr := &scriptRun{on: map[string]func(io.Writer, []string) int{
		"do-release-upgrade -c": func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "New release '24.04.1 LTS' available.")
			return 0
		},
		"do-release-upgrade -f": func(w io.Writer, _ []string) int {
			h.setOS(t, noble)
			broke = true
			return 0
		},
		"dpkg --audit": func(w io.Writer, _ []string) int {
			if !broke {
				return 0
			}
			fmt.Fprint(w, strings.ReplaceAll(auditGrub, "grub-pc             ", "linux-image-6.8.0-31-generic"))
			return 1
		},
	}}
	a, rebooted := releaseApplier(t, sr, looks("apt-get", "dpkg", "dpkg-query", "do-release-upgrade"), "24.04")
	st := a.Apply(context.Background())
	if *rebooted || st.State != RunFailed || !strings.Contains(st.Message, "linux-image-6.8.0-31-generic") ||
		!strings.Contains(st.Message, "was not rebooted") {
		t.Fatalf("rebooted=%v run=%+v", *rebooted, st)
	}
	if !sr.ran("dpkg --force-confdef --force-confold --configure -a") {
		t.Error("Argus did not try to finish the packages first")
	}
	if st.Outcome == nil || st.Outcome.Changed != ChangedYes {
		t.Fatalf("outcome = %+v", st.Outcome)
	}
}

// The check says what dpkg left unfinished, and a release upgrade is
// blocked on it instead of on the reboot it would otherwise ask for.
func TestCheckReportsUnfinishedPackages(t *testing.T) {
	newReleaseHost(t, jammy)
	sr := &scriptRun{on: map[string]func(io.Writer, []string) int{
		"do-release-upgrade -c": func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "New release '24.04.1 LTS' available.")
			return 0
		},
		"dpkg --audit": func(w io.Writer, _ []string) int { fmt.Fprint(w, auditGrub); return 1 },
	}}
	c := PackageChecker{Run: sr.run, Look: looks("apt-get", "dpkg", "dpkg-query", "do-release-upgrade")}
	r, err := c.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.Unfinished, []string{"grub-pc"}) {
		t.Fatalf("unfinished = %v", r.Unfinished)
	}
	if r.Release == nil || len(r.Release.Blockers) != 1 || !strings.Contains(r.Release.Blockers[0], "dpkg --configure -a") {
		t.Fatalf("release = %+v", r.Release)
	}
}
