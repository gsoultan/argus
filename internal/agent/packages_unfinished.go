package agent

import (
	"context"
	"regexp"
	"strings"
)

/* ── Packages dpkg left half done, and not rebooting on them ─────────────── */

// An upgrade that fails part way leaves packages unpacked but not
// configured. Rebooting then can leave a server that does not come back: a
// new kernel without its initramfs, a boot loader nobody installed, a
// systemd or libc half replaced. Argus finds them with `dpkg --audit`, says
// so in every report, and does not reboot a server while there are any,
// whoever asks.

// dpkgUnfinished is what `dpkg --audit` lists, nil when nothing (or when it
// cannot be run, which is not a reason to stop anything).
func dpkgUnfinished(ctx context.Context, run cmdRunner) []string {
	b, _, err := output(ctx, run, nil, "dpkg", "--audit")
	if err != nil {
		return nil
	}
	return parseDpkgAudit(b)
}

// unfinishedNext is what a person does about packages dpkg left half done
// before the server may be rebooted.
var unfinishedNext = []string{
	"Do not reboot yet.",
	"On the server, run `sudo dpkg --configure -a` and then `sudo apt-get -f install`, until both finish without " +
		"errors. If grub-pc is what fails, run `sudo dpkg-reconfigure grub-pc` and tick the disk the server boots from.",
	"If a release upgrade stopped part way, run `sudo apt full-upgrade` to finish it.",
	"Press Check for updates; once Argus no longer lists unfinished packages, reboot.",
}

// rebootRefusal is why the server must not be rebooted now, or "".
func (a *PackageApplier) rebootRefusal(ctx context.Context) (string, []string) {
	if _, err := a.Look("dpkg"); err != nil {
		return "", nil
	}
	left := dpkgUnfinished(ctx, a.Run)
	if len(left) == 0 {
		return "", nil
	}
	return "not rebooted: dpkg has left " + strings.Join(left, ", ") + " half installed or half configured, and " +
		"the server may not start again if it reboots like this. Finish configuring them first (`sudo dpkg " +
		"--configure -a`, `sudo apt-get -f install`), then reboot", left
}

// reDpkgError is the line dpkg writes when a package fails, and the list
// it ends with:
//
//	dpkg: error processing package grub-pc (--configure):
//	Errors were encountered while processing:
//	 grub-pc
//	 linux-image-6.8.0-31-generic
var (
	reDpkgError       = regexp.MustCompile(`dpkg: error processing (?:package|archive) (\S+)`)
	reDpkgErrorsBlock = regexp.MustCompile(`^Errors were encountered while processing:`)
)

// dpkgFailed names the packages dpkg says failed, in the order it first
// said so, from an apt or upgrader terminal log. Archives are named by
// package (the part of a .deb's file name before its first _).
func dpkgFailed(log []byte) []string {
	var out []string
	add := func(n string) {
		n = strings.TrimSuffix(strings.TrimSpace(n), ":")
		if i := strings.LastIndex(n, "/"); i >= 0 {
			n = n[i+1:]
		}
		if strings.HasSuffix(n, ".deb") {
			n, _, _ = strings.Cut(n, "_")
		}
		n, _, _ = strings.Cut(n, ":")
		if n != "" && rePackageName.MatchString(n) && !hasString(out, n) {
			out = append(out, n)
		}
	}
	inBlock := false
	for _, line := range strings.Split(string(log), "\n") {
		if m := reDpkgError.FindStringSubmatch(line); m != nil {
			add(m[1])
		}
		switch {
		case reDpkgErrorsBlock.MatchString(strings.TrimSpace(line)):
			inBlock = true
		case inBlock && strings.HasPrefix(line, " ") && strings.TrimSpace(line) != "":
			add(strings.Fields(line)[0])
		default:
			inBlock = false
		}
	}
	return out
}
