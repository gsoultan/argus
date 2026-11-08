//go:build linux

package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestGrowARealFilesystem grows a real ext4 and xfs filesystem on a loop
// device whose file was made bigger: the scan must find the room from the
// superblock, and the grow must run the real tools. It needs root, loop
// devices and the tools, and skips itself without them.
func TestGrowARealFilesystem(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	for _, fs := range []string{"ext4", "xfs"} {
		t.Run(fs, func(t *testing.T) {
			mkfs := map[string][]string{"ext4": {"mkfs.ext4", "-q"}, "xfs": {"mkfs.xfs", "-q"}}[fs]
			for _, tool := range []string{"losetup", mkfs[0], "mount", "umount"} {
				if _, err := exec.LookPath(tool); err != nil {
					t.Skip(tool + " is not installed")
				}
			}
			dir := t.TempDir()
			img := filepath.Join(dir, "disk.img")
			if err := os.WriteFile(img, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(img, 512<<20); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command("losetup", "-f", "--show", img).Output()
			if err != nil {
				t.Skip("no loop device: " + err.Error())
			}
			dev := strings.TrimSpace(string(out))
			defer exec.Command("losetup", "-d", dev).Run()
			if out, err := exec.Command(mkfs[0], append(mkfs[1:], dev)...).CombinedOutput(); err != nil {
				t.Skip("mkfs: " + string(out))
			}
			mnt := filepath.Join(dir, "mnt")
			_ = os.Mkdir(mnt, 0o755)
			if out, err := exec.Command("mount", dev, mnt).CombinedOutput(); err != nil {
				t.Skip("cannot mount here: " + string(out))
			}
			defer exec.Command("umount", mnt).Run()

			// The hypervisor makes the disk bigger.
			if err := os.Truncate(img, 1<<30); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command("losetup", "-c", dev).CombinedOutput(); err != nil {
				t.Fatal(string(out))
			}
			h := liveDiskHost
			h.loops = true
			r, _ := h.scan(context.Background(), DiskScanRequest{})
			var f *DiskFS
			for i := range r.Filesystems {
				if r.Filesystems[i].Mount == mnt {
					f = &r.Filesystems[i]
				}
			}
			if f == nil {
				t.Fatalf("the scan did not find %s: %+v", mnt, r.Filesystems)
			}
			if f.Answer != DiskGrow || f.Room < 400<<20 || f.Path.FSRoom < 400<<20 {
				t.Fatalf("answer = %q room %d (%s)", f.Answer, f.Room, f.Why)
			}
			g := &DiskGrower{Plan: DiskPlan{DiskGrowRequest: DiskGrowRequest{ID: "e2e-00000001", Key: f.Key, Path: *f.Path},
				StateDir: dir}, ops: linuxDiskOps{h: h}, Now: time.Now}
			st := g.Grow(context.Background())
			if st.State != RunDone && strings.Contains(st.Steps[len(st.Steps)-1].Output, "Permission denied to resize") {
				t.Skip("this kernel or sandbox refuses online resizing: " + st.Steps[len(st.Steps)-1].Output)
			}
			if st.State != RunDone {
				t.Fatalf("grow = %s %q %+v", st.State, st.Message, st.Steps)
			}
			if st.After < 1000<<20 {
				t.Errorf("after = %d, want about 1 GiB", st.After)
			}
			r, _ = h.scan(context.Background(), DiskScanRequest{})
			for _, f := range r.Filesystems {
				if f.Mount == mnt && f.Answer != DiskFull {
					t.Errorf("after growing, %s should use all of its disk: %q %s", mnt, f.Answer, f.Why)
				}
			}
		})
	}
}
