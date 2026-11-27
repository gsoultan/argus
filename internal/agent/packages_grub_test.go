package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// grubDisk scripts a BIOS server whose grub-pc a release upgrade left half
// configured, with a disk on record that is gone (a Nutanix by-id name), and
// / on sda2. Everything else goes to next. It records the order of calls and
// what GRUB was told.
type grubDisk struct {
	mu       sync.Mutex
	status   string
	calls    []string
	answered string
	next     cmdRunner
}

func newGrubDisk(t *testing.T, next cmdRunner) *grubDisk {
	t.Helper()
	oldEFI, oldExists := efiFirmwareDir, deviceExists
	efiFirmwareDir = filepath.Join(t.TempDir(), "no-efi")
	deviceExists = func(string) bool { return false }
	t.Cleanup(func() { efiFirmwareDir, deviceExists = oldEFI, oldExists })
	return &grubDisk{status: "install ok half-configured", next: next}
}

func (g *grubDisk) run(ctx context.Context, out io.Writer, env []string, name string, args ...string) (int, error) {
	if out == nil {
		out = io.Discard
	}
	cmd := strings.TrimSpace(name + " " + strings.Join(args, " "))
	g.mu.Lock()
	g.calls = append(g.calls, cmd)
	g.mu.Unlock()
	switch {
	case cmd == "dpkg-query -W -f=${Status} grub-pc":
		fmt.Fprint(out, g.status)
		return 0, nil
	case cmd == "debconf-show grub-pc":
		fmt.Fprintln(out, "* grub-pc/install_devices: /dev/disk/by-id/scsi-1NUTANIX_NFS_1_0_216831_gone")
		return 0, nil
	case cmd == "findmnt -n -o SOURCE /boot":
		return 1, nil
	case cmd == "findmnt -n -o SOURCE /":
		fmt.Fprintln(out, "/dev/sda2")
		return 0, nil
	case cmd == "lsblk -n -r -s -p -o NAME,TYPE /dev/sda2":
		fmt.Fprintln(out, "/dev/sda2 part\n/dev/sda disk")
		return 0, nil
	case name == "debconf-set-selections":
		b, _ := os.ReadFile(args[0])
		g.mu.Lock()
		g.answered = string(b)
		g.mu.Unlock()
		return 0, nil
	}
	if g.next == nil {
		return 0, nil
	}
	return g.next(ctx, out, env, name, args...)
}

// at is where the first call holding prefix was made, -1 when none was.
func (g *grubDisk) at(prefix string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, c := range g.calls {
		if strings.Contains(c, prefix) {
			return i
		}
	}
	return -1
}

const sdaAnswer = "grub-pc grub-pc/install_devices multiselect /dev/sda\n"

// An ordinary update after a release upgrade, as on the server that hit
// "scsi-1NUTANIX_… does not exist, so cannot grub-install to it!": GRUB is
// told the boot disk before apt configures anything.
func TestApplyAllAnswersGrubsDiskFirst(t *testing.T) {
	f := newFakeApt()
	a, _ := applier(t, f, PackageRunRequest{ID: "run-grub-0001", Mode: PackagesAll})
	g := newGrubDisk(t, f.run)
	a.Run = g.run
	st := a.Apply(context.Background())
	if st.State != RunDone {
		t.Fatalf("state = %s (%s)", st.State, st.Message)
	}
	if g.answered != sdaAnswer {
		t.Errorf("GRUB was told %q", g.answered)
	}
	set, install := g.at("debconf-set-selections"), g.at("install --only-upgrade")
	if set < 0 || install < 0 || set > install {
		t.Errorf("GRUB answered at %d, install at %d: %v", set, install, g.calls)
	}
}

// A GRUB whose disk cannot be told does not stop an update that may not
// touch it; the run goes on and the log says why.
func TestApplyAllGoesOnWhenGrubsDiskCannotBeTold(t *testing.T) {
	f := newFakeApt()
	a, _ := applier(t, f, PackageRunRequest{ID: "run-grub-0002", Mode: PackagesAll})
	g := newGrubDisk(t, f.run)
	a.Run = func(ctx context.Context, out io.Writer, env []string, name string, args ...string) (int, error) {
		if name == "findmnt" {
			return 1, nil
		}
		return g.run(ctx, out, env, name, args...)
	}
	st := a.Apply(context.Background())
	if st.State != RunDone || g.answered != "" {
		t.Fatalf("state = %s (%s), answered %q", st.State, st.Message, g.answered)
	}
}

// A server without grub-pc, or one booted by UEFI, is not asked about.
func TestApplyAllLeavesGrubAloneWithoutGrubPC(t *testing.T) {
	f := newFakeApt()
	a, _ := applier(t, f, PackageRunRequest{ID: "run-grub-0003", Mode: PackagesAll})
	g := newGrubDisk(t, f.run)
	g.status = "unknown ok not-installed"
	a.Run = g.run
	if st := a.Apply(context.Background()); st.State != RunDone {
		t.Fatalf("state = %s (%s)", st.State, st.Message)
	}
	if g.at("debconf-show") >= 0 || g.answered != "" {
		t.Errorf("asked GRUB on a server without grub-pc: %v", g.calls)
	}
}

// The repair after an interrupted run answers GRUB before dpkg configures
// it, or `dpkg --configure -a` fails on grub-pc every time.
func TestAptRepairAnswersGrubFirst(t *testing.T) {
	g := newGrubDisk(t, nil)
	var log bytes.Buffer
	if err := (aptManager{}).repair(context.Background(), g.run, &log); err != nil {
		t.Fatal(err)
	}
	if g.answered != sdaAnswer {
		t.Errorf("GRUB was told %q", g.answered)
	}
	set, conf := g.at("debconf-set-selections"), g.at("dpkg --force-confdef --force-confold --configure -a")
	if set < 0 || conf < 0 || set > conf {
		t.Errorf("GRUB answered at %d, configure at %d: %v", set, conf, g.calls)
	}
	if !strings.Contains(log.String(), "installs to /dev/sda") {
		t.Errorf("log = %q", log.String())
	}
}

func TestGrubPresent(t *testing.T) {
	for status, want := range map[string]bool{
		"install ok installed":        true,
		"install ok half-configured":  true,
		"install ok unpacked":         true,
		"install ok triggers-pending": true,
		"deinstall ok config-files":   false,
		"unknown ok not-installed":    false,
		"":                            false,
	} {
		if got := grubPresent(status); got != want {
			t.Errorf("grubPresent(%q) = %v, want %v", status, got, want)
		}
	}
}

// bootDisk follows /boot (or /) down to the whole disk under it, through
// partitions, LVM, md and LUKS, and stops when that is more than one disk.
func TestBootDiskLayouts(t *testing.T) {
	for _, tc := range []struct {
		name, boot, root, chain, want, why string
	}{
		{
			// The Nutanix server: /boot on sda2, / on LVM on sda3, all on sda.
			// The old lookup read sda2's dependants and gave up on "sda, sda3".
			name: "boot partition beside an LVM partition", boot: "/dev/sda2",
			chain: "/dev/sda2 part\n/dev/sda disk\n", want: "/dev/sda",
		},
		{
			name: "LVM over two partitions of one disk", root: "/dev/mapper/ubuntu--vg-ubuntu--lv",
			chain: "/dev/mapper/ubuntu--vg-ubuntu--lv lvm\n/dev/sda3 part\n/dev/sda disk\n" +
				"/dev/sda2 part\n/dev/sda disk\n",
			want: "/dev/sda",
		},
		{
			name: "LUKS on LVM on NVMe", root: "/dev/mapper/root_crypt",
			chain: "/dev/mapper/root_crypt crypt\n/dev/mapper/vg-root lvm\n/dev/nvme0n1p3 part\n/dev/nvme0n1 disk\n",
			want:  "/dev/nvme0n1",
		},
		{
			name: "plain partition", root: "/dev/vda1",
			chain: "/dev/vda1 part\n/dev/vda disk\n", want: "/dev/vda",
		},
		{
			name: "RAID 1 over two disks", root: "/dev/md0",
			chain: "/dev/md0 raid1\n/dev/sda2 part\n/dev/sda disk\n/dev/sdb2 part\n/dev/sdb disk\n",
			why:   "/dev/md0 spans several disks (/dev/sda, /dev/sdb)",
		},
		{
			name: "no disk at all", root: "/dev/loop0",
			chain: "/dev/loop0 loop\n", why: "lsblk found no whole disk under /dev/loop0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := tc.boot
			if src == "" {
				src = tc.root
			}
			run := func(_ context.Context, out io.Writer, _ []string, name string, args ...string) (int, error) {
				switch cmd := strings.TrimSpace(name + " " + strings.Join(args, " ")); cmd {
				case "findmnt -n -o SOURCE /boot":
					if tc.boot == "" {
						return 1, nil
					}
					fmt.Fprintln(out, tc.boot)
				case "findmnt -n -o SOURCE /":
					fmt.Fprintln(out, tc.root)
				case "lsblk -n -r -s -p -o NAME,TYPE " + src:
					fmt.Fprint(out, tc.chain)
				default:
					return 1, nil
				}
				return 0, nil
			}
			disk, why := bootDisk(context.Background(), run)
			if disk != tc.want || why != tc.why {
				t.Errorf("bootDisk = %q, %q; want %q, %q", disk, why, tc.want, tc.why)
			}
		})
	}
}

// The update that failed on the Nutanix server: the disk on record is gone,
// /boot is on sda2 and / on LVM on sda3. GRUB is told /dev/sda before apt
// finishes configuring grub-pc, instead of the run giving up.
func TestApplyAllAnswersGrubOnSplitBootLayout(t *testing.T) {
	f := newFakeApt()
	a, _ := applier(t, f, PackageRunRequest{ID: "run-grub-0004", Mode: PackagesAll})
	g := newGrubDisk(t, f.run)
	a.Run = func(ctx context.Context, out io.Writer, env []string, name string, args ...string) (int, error) {
		switch cmd := strings.TrimSpace(name + " " + strings.Join(args, " ")); cmd {
		case "findmnt -n -o SOURCE /boot":
			fmt.Fprintln(out, "/dev/sda2")
			return 0, nil
		case "lsblk -n -r -s -p -o NAME,TYPE /dev/sda2":
			fmt.Fprint(out, "/dev/sda2 part\n/dev/sda disk\n")
			return 0, nil
		}
		return g.run(ctx, out, env, name, args...)
	}
	st := a.Apply(context.Background())
	if st.State != RunDone {
		t.Fatalf("state = %s (%s)", st.State, st.Message)
	}
	if g.answered != sdaAnswer {
		t.Errorf("GRUB was told %q", g.answered)
	}
	set, install := g.at("debconf-set-selections"), g.at("install --only-upgrade")
	if set < 0 || install < 0 || set > install {
		t.Errorf("GRUB answered at %d, install at %d: %v", set, install, g.calls)
	}
}
