package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

/* ── The helper: GRUB's boot disk, before a release upgrade ──────────────── */

// On a server that boots with BIOS GRUB (grub-pc), the new release's grub-pc
// asks which disk to install the boot loader on whenever the disk it has on
// record is missing or gone: a cloud image built on another machine, a disk
// renamed since. Nobody answers that during a non-interactive upgrade, so
// grub-pc is left unconfigured and the server is between releases with a boot
// loader nobody installed. The helper answers it before upgrading, with the
// disk the server boots from, or stops before changing anything.

var (
	// efiFirmwareDir exists on a server booted by UEFI, which grub-pc does
	// not install to.
	efiFirmwareDir = "/sys/firmware/efi"
	// deviceExists says whether a disk on record is still there. A var for
	// tests.
	deviceExists = func(p string) bool { _, err := os.Stat(p); return err == nil }
)

const grubDevicesQuestion = "grub-pc/install_devices"

// grubNeedsNoAnswer names why the question cannot come up, or "" when it can.
func (a *PackageApplier) grubNeedsNoAnswer(ctx context.Context) string {
	if _, err := os.Stat(efiFirmwareDir); err == nil {
		return "the server boots with UEFI"
	}
	if _, err := a.Look("dpkg-query"); err != nil {
		return "not a dpkg system"
	}
	b, code, err := output(ctx, a.Run, nil, "dpkg-query", "-W", "-f=${Status}", "grub-pc")
	if err != nil || code != 0 || !strings.Contains(string(b), "install ok installed") {
		return "grub-pc is not installed"
	}
	return ""
}

// parseGrubDevices reads grub-pc/install_devices from `debconf-show grub-pc`:
//
//   - grub-pc/install_devices: /dev/disk/by-id/ata-QEMU_HARDDISK_QM00001
func parseGrubDevices(b []byte) []string {
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimLeft(strings.TrimSpace(line), "* ")
		v, ok := strings.CutPrefix(line, grubDevicesQuestion+":")
		if !ok {
			continue
		}
		var out []string
		for _, d := range strings.Split(v, ",") {
			if d = strings.TrimSpace(d); d != "" {
				out = append(out, d)
			}
		}
		return out
	}
	return nil
}

// bootDisk is the whole disk the server boots from: the one under /boot, or
// under / when /boot is not a mount of its own, followed down through
// partitions, LVM and the like. "" with a reason when it cannot be told, as
// for a RAID set spread over several disks.
func (a *PackageApplier) bootDisk(ctx context.Context) (string, string) {
	var src string
	for _, mnt := range []string{"/boot", "/"} {
		b, code, err := output(ctx, a.Run, nil, "findmnt", "-n", "-o", "SOURCE", mnt)
		if err == nil && code == 0 {
			if src = strings.TrimSpace(string(b)); src != "" {
				break
			}
		}
	}
	if !strings.HasPrefix(src, "/dev/") {
		return "", "findmnt did not name the device / is on"
	}
	dev := src
	for range 6 {
		b, code, err := output(ctx, a.Run, nil, "lsblk", "-n", "-o", "PKNAME", dev)
		if err != nil || code != 0 {
			return "", "lsblk could not say what " + dev + " is on"
		}
		var parents []string
		for _, p := range strings.Split(string(b), "\n") {
			if p = strings.TrimSpace(p); p != "" && !hasString(parents, p) {
				parents = append(parents, p)
			}
		}
		switch len(parents) {
		case 0:
			return dev, ""
		case 1:
			dev = "/dev/" + parents[0]
		default:
			return "", src + " spans several disks (" + strings.Join(parents, ", ") + ")"
		}
	}
	return "", "lsblk's chain under " + src + " does not end"
}

// answerGrub makes sure grub-pc knows which disk to install to before an
// upgrade: kept when a disk it has on record is still there, set to the boot
// disk when not. An error means nothing was changed and the upgrade must not
// start.
func (a *PackageApplier) answerGrub(ctx context.Context) error {
	if why := a.grubNeedsNoAnswer(ctx); why != "" {
		return nil
	}
	b, _, err := output(ctx, a.Run, nil, "debconf-show", "grub-pc")
	if err != nil {
		return err
	}
	var have []string
	for _, d := range parseGrubDevices(b) {
		if deviceExists(d) {
			have = append(have, d)
		}
	}
	if len(have) > 0 {
		fmt.Fprintf(a.lw, "argus: GRUB installs to %s\n", strings.Join(have, ", "))
		return nil
	}
	disk, why := a.bootDisk(ctx)
	if disk == "" {
		return fmt.Errorf("GRUB does not know which disk to install the boot loader on, and Argus could not tell "+
			"(%s), so the upgrade would stop part way; nothing was changed. Run `sudo dpkg-reconfigure grub-pc` "+
			"on the server, tick the disk it boots from, and ask again", why)
	}
	sel := filepath.Join(a.Plan.StateDir, packagesDir, "grub-pc.debconf")
	if err := writeFileAtomic(sel, []byte("grub-pc "+grubDevicesQuestion+" multiselect "+disk+"\n"), 0o600); err != nil {
		return fmt.Errorf("cannot write GRUB's answer; nothing was changed: %w", err)
	}
	defer os.Remove(sel)
	out, code, err := output(ctx, a.Run, nil, "debconf-set-selections", sel)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("could not tell GRUB to install to %s (debconf-set-selections exit %d), so nothing was "+
			"changed: %s", disk, code, tail(out, 300))
	}
	fmt.Fprintf(a.lw, "argus: GRUB had no disk on record that is still there; it installs to %s, the disk the server boots from\n", disk)
	return nil
}
