//go:build linux

package agent

import (
	"context"
	"encoding/binary"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const gib = int64(1) << 30

// ubuntuLVM is Ubuntu's default layout on a VMware disk that was made 200 GB
// after install: sda1 BIOS boot, sda2 /boot, sda3 an LVM PV holding /.
func ubuntuLVM(diskSize int64) linuxTopo {
	t := linuxTopo{devs: map[string]*topoDev{}, tables: map[string]topoTable{}, fsinfo: map[string]topoFS{}}
	t.devs["sda"] = &topoDev{Name: "sda", Kind: "disk", Size: diskSize, Model: "Virtual disk"}
	t.devs["sda1"] = &topoDev{Name: "sda1", Kind: "part", Parent: "sda", PartNum: 1, Start: mib, Size: mib}
	t.devs["sda2"] = &topoDev{Name: "sda2", Kind: "part", Parent: "sda", PartNum: 2, Start: 2 * mib, Size: 2 * gib}
	t.devs["sda3"] = &topoDev{Name: "sda3", Kind: "part", Parent: "sda", PartNum: 3, Start: 2*gib + 2*mib,
		Size: 98 * gib}
	t.devs["dm-0"] = &topoDev{Name: "dm-0", Kind: "dm", Size: 98*gib - 4*mib, DMName: "ubuntu--vg-ubuntu--lv",
		DMUUID: "LVM-AAAAbbbbCCCCddddLVLVLVLVuuuuuuuu", Slaves: []string{"sda3"}}
	t.tables["sda"] = topoTable{Type: "gpt", UsableEnd: diskSize - 17*1024 - 512, Kinds: map[int64]string{mib: "BIOS boot"}}
	t.pvs = []topoPV{{Name: "/dev/sda3", UUID: "pv1", Size: 98*gib - mib, DevSize: 98 * gib, VG: "ubuntu-vg"}}
	t.vgs = []topoVG{{Name: "ubuntu-vg", UUID: "vg1", Size: 98*gib - 4*mib, Free: 0, Extent: 4 * mib, PVs: 1}}
	t.lvs = []topoLV{{Name: "ubuntu-lv", VG: "ubuntu-vg", UUID: "lv1", Path: "/dev/ubuntu-vg/ubuntu-lv",
		DMPath: "/dev/mapper/ubuntu--vg-ubuntu--lv", Size: 98*gib - 4*mib, Layout: "linear"}}
	t.mounts = []topoMount{
		{Mount: "/", FS: "ext4", Dev: "dm-0", Total: uint64(96 * gib), Used: uint64(90 * gib), Avail: uint64(2 * gib)},
		{Mount: "/boot", FS: "ext4", Dev: "sda2", Total: uint64(2 * gib), Used: uint64(gib / 2), Avail: uint64(gib)},
	}
	t.fsinfo["dm-0"] = topoFS{Size: 98*gib - 4*mib, UUID: "root-uuid", Devices: 1}
	t.fsinfo["sda2"] = topoFS{Size: 2 * gib, UUID: "boot-uuid", Devices: 1}
	return t
}

func fsByMount(t *testing.T, fss []DiskFS, m string) DiskFS {
	t.Helper()
	for _, f := range fss {
		if f.Mount == m {
			return f
		}
	}
	t.Fatalf("no filesystem at %s in %+v", m, fss)
	return DiskFS{}
}

func TestADiskMadeBiggerLetsLVMRootGrow(t *testing.T) {
	fss, disks := ubuntuLVM(200 * gib).analyze()
	root := fsByMount(t, fss, "/")
	if root.Answer != DiskGrow {
		t.Fatalf("answer = %q (%s), want grow", root.Answer, root.Why)
	}
	if root.Room < 99*gib || root.Room > 101*gib {
		t.Errorf("room = %d GiB, want about 100", root.Room/gib)
	}
	if !root.LVM || root.Path == nil || root.Path.Part != "sda3" || root.Path.PartNum != 3 || root.Path.LV != "/dev/ubuntu-vg/ubuntu-lv" {
		t.Errorf("path = %+v", root.Path)
	}
	if !root.Rescan {
		t.Error("a SCSI disk should offer a rescan")
	}
	if root.Key != "uuid:root-uuid" {
		t.Errorf("key = %q", root.Key)
	}
	kinds := []string{}
	for _, l := range root.Layers {
		kinds = append(kinds, l.Kind)
	}
	if strings.Join(kinds, ",") != "disk,partition,pv,vg,lv,fs" {
		t.Errorf("layers = %v, want the disk first and the filesystem last", kinds)
	}
	if !strings.Contains(root.Why, "sda3") || !strings.Contains(root.Why, "LVM") {
		t.Errorf("why = %q", root.Why)
	}
	// /boot sits before sda3: the free space is not right after it.
	boot := fsByMount(t, fss, "/boot")
	if boot.Answer != DiskBlocked || !strings.Contains(boot.Why, "sda3 comes right after it") {
		t.Errorf("/boot = %q %q, want blocked by sda3", boot.Answer, boot.Why)
	}
	if len(disks) != 1 || disks[0].Unallocated < 99*gib {
		t.Errorf("disks = %+v", disks)
	}
	if fss[0].Mount != "/" {
		t.Errorf("the fullest filesystem should come first, got %s", fss[0].Mount)
	}
}

func TestADiskThatFitsSaysFullAndOffersARescan(t *testing.T) {
	fss, _ := ubuntuLVM(100*gib + 2*mib + 17*1024 + 512).analyze()
	root := fsByMount(t, fss, "/")
	if root.Answer != DiskFull || root.Path != nil {
		t.Fatalf("answer = %q (%s), path %+v; want full with no path", root.Answer, root.Why, root.Path)
	}
	if !root.Rescan {
		t.Error("a full SCSI disk should offer to look for more space")
	}
}

func TestVolumeGroupFreeSpaceCanGoToOneVolume(t *testing.T) {
	tp := ubuntuLVM(100*gib + 2*mib + 17*1024 + 512)
	tp.vgs[0].Free = 40 * gib
	fss, _ := tp.analyze()
	root := fsByMount(t, fss, "/")
	if root.Answer != DiskGrow || root.Room != 40*gib || root.Path.VGFree != 40*gib || root.Path.PartRoom != 0 {
		t.Fatalf("root = %q room %d path %+v", root.Answer, root.Room, root.Path)
	}
	if root.Why != "Growing extends the LVM volume and the filesystem, online." {
		t.Errorf("why = %q", root.Why)
	}
}

func TestNVMeCloudImageGrowsItsFirstPartition(t *testing.T) {
	tp := linuxTopo{devs: map[string]*topoDev{}, tables: map[string]topoTable{}, fsinfo: map[string]topoFS{}}
	tp.devs["nvme0n1"] = &topoDev{Name: "nvme0n1", Kind: "disk", Size: 200 * gib, Serial: "vol0a91b2c3d4e5f6789"}
	tp.devs["nvme0n1p15"] = &topoDev{Name: "nvme0n1p15", Kind: "part", Parent: "nvme0n1", PartNum: 15, Start: mib,
		Size: 105 * mib}
	tp.devs["nvme0n1p1"] = &topoDev{Name: "nvme0n1p1", Kind: "part", Parent: "nvme0n1", PartNum: 1, Start: 106 * mib,
		Size: 100*gib - 106*mib}
	tp.tables["nvme0n1"] = topoTable{Type: "gpt", UsableEnd: 200*gib - 17*1024 - 512,
		Kinds: map[int64]string{mib: "EFI"}}
	tp.mounts = []topoMount{{Mount: "/", FS: "xfs", Dev: "nvme0n1p1", Total: uint64(100 * gib), Used: uint64(91 * gib),
		Avail: uint64(9 * gib)}}
	tp.fsinfo["nvme0n1p1"] = topoFS{Size: 100*gib - 106*mib, UUID: "x", Devices: 1}
	fss, _ := tp.analyze()
	root := fsByMount(t, fss, "/")
	if root.Answer != DiskGrow || root.Path.Part != "nvme0n1p1" || root.Path.PartNum != 1 || root.Path.Disk != "nvme0n1" {
		t.Fatalf("root = %q %+v", root.Answer, root.Path)
	}
	if root.Rescan {
		t.Error("NVMe sees a new size by itself; no rescan")
	}
	if root.Path.DiskSerial != "vol0a91b2c3d4e5f6789" {
		t.Error("the NVMe serial should travel with the path: it matches the disk to its cloud volume")
	}
	if root.LVM {
		t.Error("a plain partition is not told how much")
	}
	ops := linuxDiskOps{}
	haveTool = func(n string) bool { return n == "growpart" }
	defer func() { haveTool = func(string) bool { return false } }()
	steps, err := ops.steps(DiskGrowRequest{}, *root.Path)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, s := range steps {
		got = append(got, s.String())
	}
	if strings.Join(got, "; ") != "growpart /dev/nvme0n1 1; xfs_growfs /" {
		t.Errorf("steps = %v", got)
	}
}

func TestSwapBetweenRootAndTheFreeSpaceBlocks(t *testing.T) {
	tp := linuxTopo{devs: map[string]*topoDev{}, tables: map[string]topoTable{}, fsinfo: map[string]topoFS{}}
	tp.devs["sda"] = &topoDev{Name: "sda", Kind: "disk", Size: 40 * gib}
	tp.devs["sda1"] = &topoDev{Name: "sda1", Kind: "part", Parent: "sda", PartNum: 1, Start: mib, Size: 30 * gib}
	tp.devs["sda2"] = &topoDev{Name: "sda2", Kind: "part", Parent: "sda", PartNum: 2, Start: 30*gib + mib, Size: 2 * gib}
	tp.tables["sda"] = topoTable{Type: "dos", UsableEnd: 40 * gib, Kinds: map[int64]string{30*gib + mib: "swap"}}
	tp.mounts = []topoMount{{Mount: "/", FS: "ext4", Dev: "sda1", Total: uint64(30 * gib), Used: uint64(26 * gib),
		Avail: uint64(3 * gib)}}
	tp.fsinfo["sda1"] = topoFS{Size: 30 * gib, Devices: 1}
	fss, _ := tp.analyze()
	root := fsByMount(t, fss, "/")
	if root.Answer != DiskBlocked || !strings.Contains(root.Why, "The swap partition sda2") {
		t.Fatalf("root = %q %q", root.Answer, root.Why)
	}
	if root.Path != nil {
		t.Error("a blocked filesystem must carry no path to grow")
	}
}

func TestLUKSOnAPartitionGrowsThroughTheEncryption(t *testing.T) {
	tp := linuxTopo{devs: map[string]*topoDev{}, tables: map[string]topoTable{}, fsinfo: map[string]topoFS{}}
	tp.devs["vda"] = &topoDev{Name: "vda", Kind: "disk", Size: 100 * gib}
	tp.devs["vda1"] = &topoDev{Name: "vda1", Kind: "part", Parent: "vda", PartNum: 1, Start: mib, Size: 50 * gib}
	tp.devs["dm-0"] = &topoDev{Name: "dm-0", Kind: "dm", Size: 50*gib - 16*mib, DMName: "data",
		DMUUID: "CRYPT-LUKS2-abc-data", Slaves: []string{"vda1"}}
	tp.tables["vda"] = topoTable{Type: "gpt", UsableEnd: 100*gib - 17*1024 - 512}
	tp.mounts = []topoMount{{Mount: "/srv", FS: "ext4", Dev: "dm-0", Total: uint64(49 * gib), Used: uint64(40 * gib),
		Avail: uint64(9 * gib)}}
	tp.fsinfo["dm-0"] = topoFS{Size: 50*gib - 16*mib, Devices: 1}
	fss, _ := tp.analyze()
	f := fsByMount(t, fss, "/srv")
	if f.Answer != DiskGrow || f.Path.Crypt != "data" || f.Path.PartRoom < 49*gib {
		t.Fatalf("f = %q %+v", f.Answer, f.Path)
	}
	haveTool = func(n string) bool { return n == "sfdisk" }
	defer func() { haveTool = func(string) bool { return false } }()
	steps, _ := linuxDiskOps{}.steps(DiskGrowRequest{}, *f.Path)
	got := []string{}
	for _, s := range steps {
		got = append(got, s.String())
	}
	want := "sfdisk --relocate gpt-bak-std /dev/vda; echo , + | sfdisk --no-reread --force -N 1 /dev/vda; " +
		"partx -u --nr 1 /dev/vda; cryptsetup resize data; resize2fs /dev/dm-0"
	if strings.Join(got, "; ") != want {
		t.Errorf("steps =\n%s\nwant\n%s", strings.Join(got, "; "), want)
	}
}

func TestAFilesystemThatCannotGrowOnlineIsNotOffered(t *testing.T) {
	tp := linuxTopo{devs: map[string]*topoDev{}, tables: map[string]topoTable{}, fsinfo: map[string]topoFS{}}
	tp.devs["sdb"] = &topoDev{Name: "sdb", Kind: "disk", Size: 64 * gib}
	tp.devs["sdb1"] = &topoDev{Name: "sdb1", Kind: "part", Parent: "sdb", PartNum: 1, Start: mib, Size: 32 * gib}
	tp.tables["sdb"] = topoTable{Type: "dos", UsableEnd: 64 * gib}
	tp.mounts = []topoMount{{Mount: "/media/usb", FS: "vfat", Dev: "sdb1", Total: uint64(32 * gib), Used: uint64(gib),
		Avail: uint64(31 * gib)}}
	fss, _ := tp.analyze()
	f := fsByMount(t, fss, "/media/usb")
	if f.Answer != DiskBlocked || !strings.Contains(f.Why, "vfat cannot grow while it is mounted") || f.Path != nil {
		t.Fatalf("f = %q %q", f.Answer, f.Why)
	}
}

func TestAFilesystemSmallerThanItsDeviceGrowsAlone(t *testing.T) {
	tp := linuxTopo{devs: map[string]*topoDev{}, tables: map[string]topoTable{}, fsinfo: map[string]topoFS{}}
	tp.devs["vdb"] = &topoDev{Name: "vdb", Kind: "disk", Size: 500 * gib}
	tp.mounts = []topoMount{{Mount: "/data", FS: "xfs", Dev: "vdb", Total: uint64(250 * gib), Used: uint64(220 * gib),
		Avail: uint64(30 * gib)}}
	tp.fsinfo["vdb"] = topoFS{Size: 250 * gib, Devices: 1}
	fss, _ := tp.analyze()
	f := fsByMount(t, fss, "/data")
	if f.Answer != DiskGrow || f.Room != 250*gib || f.Path.Part != "" || f.Path.FSRoom != 250*gib {
		t.Fatalf("f = %q room %d %+v", f.Answer, f.Room, f.Path)
	}
}

func TestThinVolumesAndRAIDAreNotGrown(t *testing.T) {
	tp := ubuntuLVM(200 * gib)
	tp.lvs[0].Layout = "thin,sparse"
	fss, _ := tp.analyze()
	if root := fsByMount(t, fss, "/"); root.Answer != DiskBlocked || !strings.Contains(root.Why, "thin") {
		t.Errorf("thin volume = %q %q", root.Answer, root.Why)
	}
	tp = ubuntuLVM(200 * gib)
	tp.devs["md0"] = &topoDev{Name: "md0", Kind: "md", Size: 10 * gib, Slaves: []string{"sdb1", "sdc1"}}
	tp.mounts = append(tp.mounts, topoMount{Mount: "/raid", FS: "ext4", Dev: "md0", Total: uint64(10 * gib),
		Used: uint64(gib), Avail: uint64(8 * gib)})
	fss, _ = tp.analyze()
	if f := fsByMount(t, fss, "/raid"); f.Answer == DiskGrow {
		t.Errorf("md raid = %q, want not grown", f.Answer)
	}
}

/* ── Reading tables and superblocks ──────────────────────────────────────── */

type memDev []byte

func (m memDev) ReadAt(b []byte, off int64) (int, error) {
	if off >= int64(len(m)) {
		return 0, errors.New("eof")
	}
	n := copy(b, m[off:])
	if n < len(b) {
		return n, errors.New("eof")
	}
	return n, nil
}

func TestAGPTOnADiskThatGrewReachesTheNewEnd(t *testing.T) {
	const ss = 512
	dev := make(memDev, 64*1024)
	dev[510], dev[511] = 0x55, 0xAA
	dev[446+4] = 0xEE
	g := dev[ss:]
	copy(g, "EFI PART")
	binary.LittleEndian.PutUint64(g[32:], 209715199) // backup still at the old 100 GiB end
	binary.LittleEndian.PutUint64(g[48:], 209715166)
	binary.LittleEndian.PutUint64(g[72:], 2)
	binary.LittleEndian.PutUint32(g[80:], 128)
	binary.LittleEndian.PutUint32(g[84:], 128)
	// One entry: swap at sector 2048.
	e := dev[2*ss:]
	copy(e[0:16], []byte{0x6D, 0xFD, 0x57, 0x06, 0xAB, 0xA4, 0xC4, 0x43, 0x84, 0xE5, 0x09, 0x33, 0xC8, 0x4B, 0x4F, 0x4F})
	binary.LittleEndian.PutUint64(e[32:], 2048)
	head := make([]byte, 2*ss)
	copy(head, dev)
	tb, ok := parseTable(dev, head, ss, 200*gib)
	if !ok || tb.Type != "gpt" {
		t.Fatalf("table = %+v %v", tb, ok)
	}
	sectors := 200 * gib / ss
	if want := (sectors - 1 - 32) * ss; tb.UsableEnd != want {
		t.Errorf("usable end = %d, want %d (the new end, less the backup entries)", tb.UsableEnd, want)
	}
	if tb.Kinds[2048*ss] != "swap" {
		t.Errorf("kinds = %v", tb.Kinds)
	}
}

func TestAnMBRStopsAtTwoTebibytes(t *testing.T) {
	head := make([]byte, 1024)
	head[510], head[511] = 0x55, 0xAA
	head[446+16+4] = 0x82
	binary.LittleEndian.PutUint32(head[446+16+8:], 4096)
	tb, ok := parseTable(memDev(head), head, 512, 3<<40)
	if !ok || tb.Type != "dos" || tb.UsableEnd != 1<<41 || tb.Kinds[4096*512] != "swap" {
		t.Fatalf("table = %+v %v", tb, ok)
	}
}

func TestSuperblocksSayTheFilesystemsOwnSize(t *testing.T) {
	ext := make(memDev, 4096)
	sb := ext[1024:]
	binary.LittleEndian.PutUint32(sb[0x04:], 1000)
	binary.LittleEndian.PutUint32(sb[0x18:], 2) // 4096-byte blocks
	binary.LittleEndian.PutUint16(sb[0x38:], 0xEF53)
	copy(sb[0x68:], []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
	info, ok := readSuperblock(ext, "ext4")
	if !ok || info.Size != 4096000 || info.UUID != "01020304-0506-0708-090a-0b0c0d0e0f10" {
		t.Errorf("ext4 = %+v %v", info, ok)
	}
	xfs := make(memDev, 512)
	copy(xfs, "XFSB")
	binary.BigEndian.PutUint32(xfs[4:], 4096)
	binary.BigEndian.PutUint64(xfs[8:], 2560)
	if info, ok := readSuperblock(xfs, "xfs"); !ok || info.Size != 10<<20 {
		t.Errorf("xfs = %+v %v", info, ok)
	}
	if _, ok := readSuperblock(make(memDev, 4096), "ext4"); ok {
		t.Error("a device with no superblock must not read as one")
	}
}

func TestLVMReportsAreRead(t *testing.T) {
	rows := lvmRows([]byte(`{"report":[{"lv":[{"lv_name":"root","vg_name":"rhel","lv_size":"53687091200"}]}]}`), "lv")
	if len(rows) != 1 || rows[0]["lv_name"] != "root" || atoi64(rows[0]["lv_size"]) != 50*gib {
		t.Errorf("rows = %v", rows)
	}
}

/* ── Windows ─────────────────────────────────────────────────────────────── */

const winJSON = `{"vendor":"Microsoft Corporation","model":"Virtual Machine",
"disks":[{"number":0,"size":214748364800,"allocated":128849018880,"style":"GPT","bus":"SCSI","serial":"","model":"Msft Virtual Disk","ro":false,"offline":false},
         {"number":1,"size":536870912000,"allocated":268435456000,"style":"GPT","bus":"SCSI","serial":"","model":"Msft Virtual Disk","ro":false,"offline":false}],
"parts":[{"disk":0,"number":1,"offset":1048576,"size":104857600,"max":104857600,"letter":"","type":"System","paths":["\\\\?\\Volume{a}\\"],"fs":"FAT32","label":"","vsize":0,"vfree":0,"uuid":""},
         {"disk":0,"number":3,"offset":122683392,"size":127775277056,"max":127775277056,"letter":"C","type":"Basic","paths":["C:\\"],"fs":"NTFS","label":"","vsize":127775277056,"vfree":16106127360,"uuid":"\\\\?\\Volume{c}\\"},
         {"disk":0,"number":4,"offset":127898009600,"size":643825664,"max":643825664,"letter":"","type":"Recovery","paths":[],"fs":"NTFS","label":"","vsize":643825664,"vfree":0,"uuid":""},
         {"disk":1,"number":2,"offset":16777216,"size":268418678784,"max":536853086208,"letter":"D","type":"Basic","paths":["D:\\"],"fs":"NTFS","label":"Data","vsize":268418678784,"vfree":32212254720,"uuid":"\\\\?\\Volume{d}\\"}]}`

func TestWindowsRecoveryPartitionAfterCIsExplained(t *testing.T) {
	r, err := parseWinDisks([]byte(winJSON))
	if err != nil {
		t.Fatal(err)
	}
	if r.Hypervisor != "Hyper-V" {
		t.Errorf("hypervisor = %q", r.Hypervisor)
	}
	c := fsByMount(t, r.Filesystems, "C:")
	if c.Answer != DiskBlocked || !strings.Contains(c.Why, "recovery partition") || c.Path != nil {
		t.Errorf("C: = %q %q", c.Answer, c.Why)
	}
	d := fsByMount(t, r.Filesystems, "D:")
	if d.Answer != DiskGrow || d.Path == nil || d.Path.DiskNumber != 1 || d.Path.PartitionNumber != 2 || d.Room < 249*gib {
		t.Fatalf("D: = %q %+v", d.Answer, d.Path)
	}
	steps := winGrowSteps(*d.Path)
	if len(steps) != 1 || steps[0].String() != "Resize-Partition -DiskNumber 1 -PartitionNumber 2 -Size "+
		"(Get-PartitionSupportedSize -DiskNumber 1 -PartitionNumber 2).SizeMax" {
		t.Errorf("steps = %+v", steps)
	}
	if err := validDiskRequest(DiskGrowRequest{ID: "x", Key: d.Key, Path: *d.Path}); err != nil {
		t.Errorf("a Windows path should be a valid request: %v", err)
	}
}

/* ── The grow ────────────────────────────────────────────────────────────── */

type fakeDiskOps struct {
	scans []DiskReport
	ran   []string
	fail  string
}

func (f *fakeDiskOps) scan(context.Context, string) (DiskReport, error) {
	if len(f.scans) == 0 {
		return DiskReport{}, errors.New("no more scans")
	}
	r := f.scans[0]
	if len(f.scans) > 1 {
		f.scans = f.scans[1:]
	}
	return r, nil
}

func (f *fakeDiskOps) run(_ context.Context, c diskCmd) (string, error) {
	f.ran = append(f.ran, c.String())
	if f.fail != "" && strings.Contains(c.String(), f.fail) {
		return "boom", errors.New("exit status 1")
	}
	if strings.Contains(c.String(), "--dump") {
		return "label: gpt\n/dev/sda3 : start=4198400, size=205516800", nil
	}
	return "ok", nil
}

func (f *fakeDiskOps) steps(req DiskGrowRequest, p GrowPath) ([]diskCmd, error) {
	haveTool = func(n string) bool { return n == "growpart" }
	defer func() { haveTool = func(string) bool { return false } }()
	return linuxDiskOps{}.steps(req, p)
}

func (f *fakeDiskOps) table(ctx context.Context, p GrowPath) (diskCmd, bool) {
	return linuxDiskOps{}.table(ctx, p)
}
func (f *fakeDiskOps) rescanDisk(GrowPath) string { return "" }

func growerFor(t *testing.T, ops *fakeDiskOps) (*DiskGrower, DiskFS) {
	t.Helper()
	fss, _ := ubuntuLVM(200 * gib).analyze()
	root := fsByMount(t, fss, "/")
	g := &DiskGrower{Plan: DiskPlan{DiskGrowRequest: DiskGrowRequest{ID: "run-0000001", Key: root.Key, Path: *root.Path},
		StateDir: t.TempDir(), Unit: "argus-diskgrow-x"}, ops: ops, Now: time.Now}
	return g, root
}

func TestAGrowRunsEachLayerBottomFirstAndKeepsTheTable(t *testing.T) {
	before, _ := ubuntuLVM(200 * gib).analyze()
	afterTopo := ubuntuLVM(200 * gib)
	afterTopo.devs["sda3"].Size = 198 * gib
	afterTopo.fsinfo["dm-0"] = topoFS{Size: 198 * gib, UUID: "root-uuid", Devices: 1}
	afterTopo.mounts[0].Total = uint64(196 * gib)
	after, _ := afterTopo.analyze()
	ops := &fakeDiskOps{scans: []DiskReport{{Filesystems: before}, {Filesystems: after}}}
	g, _ := growerFor(t, ops)
	st := g.Grow(context.Background())
	if st.State != RunDone {
		t.Fatalf("state = %s %q", st.State, st.Message)
	}
	want := []string{"sfdisk --dump /dev/sda", "growpart /dev/sda 3", "lvm pvresize /dev/sda3",
		"lvm lvextend -l +100%FREE /dev/ubuntu-vg/ubuntu-lv", "resize2fs /dev/ubuntu-vg/ubuntu-lv"}
	if strings.Join(ops.ran, "|") != strings.Join(want, "|") {
		t.Errorf("ran\n%v\nwant\n%v", ops.ran, want)
	}
	if !strings.Contains(st.TableBefore, "sda3") {
		t.Errorf("the table before was not kept: %q", st.TableBefore)
	}
	if st.After != 198*gib || st.Before != 98*gib-4*mib {
		t.Errorf("before %d after %d", st.Before, st.After)
	}
	if !strings.Contains(st.Message, "grew /") {
		t.Errorf("message = %q", st.Message)
	}
	if len(st.Steps) != 7 || st.Steps[0].Title != "Check the layout" || st.Steps[6].Title != "Read back" {
		t.Errorf("steps = %+v", st.Steps)
	}
}

func TestAGrowRefusesALayoutThatMoved(t *testing.T) {
	moved := ubuntuLVM(200 * gib)
	moved.devs["sda3"].Start += 4 * mib
	now, _ := moved.analyze()
	ops := &fakeDiskOps{scans: []DiskReport{{Filesystems: now}}}
	g, _ := growerFor(t, ops)
	st := g.Grow(context.Background())
	if st.State != RunFailed || !strings.Contains(st.Message, "nothing was changed") ||
		!strings.Contains(st.Message, "partition changed") {
		t.Fatalf("state = %s %q", st.State, st.Message)
	}
	if len(ops.ran) != 0 {
		t.Errorf("a moved layout must stop before any command; ran %v", ops.ran)
	}
}

func TestAGrowStopsAtTheFirstFailedStep(t *testing.T) {
	before, _ := ubuntuLVM(200 * gib).analyze()
	ops := &fakeDiskOps{scans: []DiskReport{{Filesystems: before}}, fail: "pvresize"}
	g, _ := growerFor(t, ops)
	st := g.Grow(context.Background())
	if st.State != RunFailed || !strings.Contains(st.Message, "physical volume failed") {
		t.Fatalf("state = %s %q", st.State, st.Message)
	}
	for _, r := range ops.ran {
		if strings.Contains(r, "lvextend") || strings.Contains(r, "resize2fs") {
			t.Errorf("ran %q after a failed step", r)
		}
	}
	if last := st.Steps[len(st.Steps)-1]; last.State != "failed" || !strings.Contains(last.Output, "boom") {
		t.Errorf("last step = %+v", last)
	}
}

func TestARequestNamingStrangeDevicesIsRefused(t *testing.T) {
	ok := DiskGrowRequest{ID: "x", Key: "uuid:abc", Path: GrowPath{Mount: "/", FSType: "ext4", Disk: "sda"}}
	if err := validDiskRequest(ok); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []GrowPath{
		{Mount: "/", FSType: "ext4", Disk: "sda; rm -rf /"},
		{Mount: "/", FSType: "ext4", LV: "/dev/../etc/passwd"},
		{Mount: "/", FSType: "ext4", Crypt: "a b"},
	} {
		if err := validDiskRequest(DiskGrowRequest{ID: "x", Key: "uuid:a", Path: bad}); err == nil {
			t.Errorf("%+v was accepted", bad)
		}
	}
	if err := validDiskRequest(DiskGrowRequest{ID: "x", Key: "uuid:a", Bytes: 10, Path: GrowPath{Mount: "/", FSType: "ext4",
		Part: "sda1"}}); err == nil {
		t.Error("a partition cannot be grown by part of its room")
	}
}

/* ── The daemon ──────────────────────────────────────────────────────────── */

type fakeDiskPlatform struct {
	report   DiskReport
	launched []string
	rescans  int
}

func (f *fakeDiskPlatform) supported() string { return "" }
func (f *fakeDiskPlatform) scan(_ context.Context, req DiskScanRequest) (DiskReport, error) {
	if req.Rescan {
		f.rescans++
	}
	r := f.report
	r.ScannedAt = time.Now().UTC()
	return r, nil
}
func (f *fakeDiskPlatform) launch(_ context.Context, plan, unit string) error {
	f.launched = append(f.launched, unit)
	return nil
}
func (f *fakeDiskPlatform) active(string) bool { return true }
func (f *fakeDiskPlatform) bootID() string     { return "boot" }

func TestTheDaemonScansWhenAskedAndStartsAGrowOnce(t *testing.T) {
	fss, _ := ubuntuLVM(200 * gib).analyze()
	p := &fakeDiskPlatform{report: DiskReport{OS: "linux", Supported: true, Filesystems: fss}}
	d := &Disks{StateDir: t.TempDir(), platform: p, now: time.Now, Log: nil}
	d.Log = NewDisks(t.TempDir(), "", nil).Log
	d.Handle(context.Background(), &DiskOrders{Scan: true, ScanID: "scan-1", Rescan: true})
	d.maybeScan(context.Background())
	b := d.Beat()
	if b.Report == nil || b.ScanID != "scan-1" || p.rescans != 1 {
		t.Fatalf("beat = %+v, rescans %d", b, p.rescans)
	}
	d.Delivered(b)
	if b2 := d.Beat(); b2.Report != nil || b2.ScanID != "" {
		t.Error("a delivered report and scan id should not be sent again")
	}
	root := fsByMount(t, fss, "/")
	req := &DiskGrowRequest{ID: "grow-00000001", Key: root.Key, Path: *root.Path}
	d.Handle(context.Background(), &DiskOrders{Scan: true, Run: req})
	d.Handle(context.Background(), &DiskOrders{Scan: true, Run: req})
	if len(p.launched) != 1 || !strings.HasPrefix(p.launched[0], "argus-diskgrow-") {
		t.Fatalf("launched = %v", p.launched)
	}
	if b := d.Beat(); b.Run == nil || b.Run.State != RunStarting {
		t.Errorf("run = %+v", b.Run)
	}
	if _, err := filepath.Glob(filepath.Join(d.StateDir, disksDir, "plan-*.json")); err != nil {
		t.Error(err)
	}
}
