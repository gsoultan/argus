//go:build linux

package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeSys lays out the sysfs entries a rescan writes to, under a temp root.
func fakeSys(t *testing.T, links map[string]string, files ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range files {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for at, to := range links {
		p := filepath.Join(root, at)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(to, p); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func wrote(t *testing.T, root, f string) bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, f))
	if err != nil {
		t.Fatal(err)
	}
	return string(b) == "1"
}

// Root cause: a rescan only ever asked SCSI disks, so an NVMe disk an old
// kernel had not noticed growing stayed at its old size however often
// "Look for more space" was pressed.
func TestAnNVMeRescanAsksItsController(t *testing.T) {
	root := fakeSys(t, map[string]string{
		"/sys/class/block/nvme1n1/device": "../../nvme1",
	}, "/sys/class/nvme/nvme1/rescan_controller", "/sys/class/nvme/nvme0/rescan_controller")
	h := diskHost{root: root}
	if err := h.rescan("nvme1n1"); err != nil {
		t.Fatal(err)
	}
	if !wrote(t, root, "/sys/class/nvme/nvme1/rescan_controller") {
		t.Error("nvme1 was not asked to rescan")
	}
	if wrote(t, root, "/sys/class/nvme/nvme0/rescan_controller") {
		t.Error("nvme0 has nothing to do with nvme1n1")
	}
}

func TestAMultipathNVMeRescanAsksEachController(t *testing.T) {
	root := fakeSys(t, map[string]string{
		"/sys/class/block/nvme0n1/device":              "../../nvme-subsys0",
		"/sys/class/nvme-subsystem/nvme-subsys0/nvme0": "../../nvme/nvme0",
		"/sys/class/nvme-subsystem/nvme-subsys0/nvme2": "../../nvme/nvme2",
	}, "/sys/class/nvme/nvme0/rescan_controller", "/sys/class/nvme/nvme2/rescan_controller",
		"/sys/class/nvme-subsystem/nvme-subsys0/subsysnqn")
	h := diskHost{root: root}
	if err := h.rescan("nvme0n1"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"nvme0", "nvme2"} {
		if !wrote(t, root, "/sys/class/nvme/"+c+"/rescan_controller") {
			t.Errorf("%s was not asked to rescan", c)
		}
	}
}

func TestAnNVMeDiskWithNoControllerSaysSo(t *testing.T) {
	h := diskHost{root: t.TempDir()}
	if err := h.rescan("nvme0n1"); err == nil {
		t.Fatal("a disk with no device link cannot be rescanned")
	}
}

func TestAGrowOnNVMeRescansItsDiskFirst(t *testing.T) {
	if got := (linuxDiskOps{}).rescanDisk(GrowPath{Disk: "nvme0n1"}); got != "nvme0n1" {
		t.Errorf("rescanDisk = %q", got)
	}
	if got := (linuxDiskOps{}).rescanDisk(GrowPath{Disk: "vda"}); got != "" {
		t.Errorf("virtio has no rescan; got %q", got)
	}
}
