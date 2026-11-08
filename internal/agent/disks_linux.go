//go:build linux

package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

/* ── The daemon's platform ───────────────────────────────────────────────── */

type linuxDisks struct {
	executable string
	stateDir   string
}

func hostDiskPlatform(executable, stateDir string) diskPlatform {
	return &linuxDisks{executable: executable, stateDir: stateDir}
}

func (p *linuxDisks) supported() string {
	switch {
	case os.Geteuid() != 0:
		return "the agent is not running as root, so it cannot read or grow disks"
	case !systemdRunning():
		return "this server does not run systemd, which growing disks from the console needs"
	}
	return ""
}

// scan runs `argus-agent disks-scan` outside the agent's sandbox, which
// cannot read a disk's partition table, and reads the report it prints.
func (p *linuxDisks) scan(ctx context.Context, req DiskScanRequest) (DiskReport, error) {
	dir := filepath.Join(p.stateDir, disksDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return DiskReport{}, err
	}
	reqPath := filepath.Join(dir, "scan.json")
	if err := writeFileAtomic(reqPath, mustJSON(req), 0o600); err != nil {
		return DiskReport{}, err
	}
	cmd := exec.CommandContext(ctx, "systemd-run", "--pipe", "--wait", "--quiet", "--collect",
		"--service-type=exec", "--description=Argus disk scan", p.executable, "disks-scan", "--req", reqPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return DiskReport{}, fmt.Errorf("scanning the disks failed: %s", tail([]byte(msg), 400))
	}
	var r DiskReport
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		return DiskReport{}, fmt.Errorf("cannot read the scan's report: %w", err)
	}
	return r, nil
}

func (p *linuxDisks) launch(ctx context.Context, planPath, unit string) error {
	cmd := exec.CommandContext(ctx, "systemd-run", "--quiet", "--collect",
		"--unit="+unit, "--service-type=exec", "--description=Argus disk grow",
		p.executable, "disks-grow", "--plan", planPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemd-run: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (p *linuxDisks) active(unit string) bool {
	return exec.Command("systemctl", "is-active", "--quiet", unit+".service").Run() == nil
}

func (p *linuxDisks) bootID() string { return readBootID() }

/* ── The scan ────────────────────────────────────────────────────────────── */

// diskHost is where the scan reads: "" on a server, a fixture tree in
// tests, with the LVM report and filesystem sizes from functions.
type diskHost struct {
	root   string
	statfs func(path string) (total, free, avail uint64, ok bool)
	lvm    func(ctx context.Context, what string, fields string) ([]byte, error)
	// loops counts loop devices as disks: only the tests that grow a real
	// filesystem on one.
	loops bool
}

var liveDiskHost = diskHost{statfs: statfsLive, lvm: runLVMReport}

func (h diskHost) p(s string) string { return h.root + s }

func (h diskHost) line(s string) string {
	b, err := os.ReadFile(h.p(s))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (h diskHost) num(s string) int64 {
	n, _ := strconv.ParseInt(h.line(s), 10, 64)
	return n
}

// scanHostDisks is the helper's scan of this server.
func scanHostDisks(ctx context.Context, req DiskScanRequest) (DiskReport, error) {
	return liveDiskHost.scan(ctx, req)
}

func (h diskHost) scan(ctx context.Context, req DiskScanRequest) (DiskReport, error) {
	r := DiskReport{ScannedAt: time.Now().UTC(), OS: "linux", Supported: true, Filesystems: []DiskFS{},
		Disks: []BlockDisk{}}
	r.Hypervisor = hypervisorFromDMI(h.line("/sys/class/dmi/id/sys_vendor"), h.line("/sys/class/dmi/id/product_name"))
	t := h.topology(ctx)
	if req.Rescan {
		any := false
		for _, d := range t.devs {
			if d.Kind == "disk" && busOf(d) == "scsi" && !d.Removable {
				if err := h.rescan(d.Name); err == nil {
					any = true
				}
			}
		}
		if any {
			r.Rescanned = true
			time.Sleep(time.Second) // the kernel reads the new size in the background
			t = h.topology(ctx)
		}
	}
	r.Filesystems, r.Disks = t.analyze()
	return r, nil
}

// rescan asks a SCSI disk for its size again. It changes nothing on it.
func (h diskHost) rescan(disk string) error {
	return os.WriteFile(h.p("/sys/class/block/"+disk+"/device/rescan"), []byte("1"), 0o200)
}

// topology reads everything the analysis needs.
func (h diskHost) topology(ctx context.Context) linuxTopo {
	t := linuxTopo{devs: map[string]*topoDev{}, tables: map[string]topoTable{}, fsinfo: map[string]topoFS{}}
	ents, _ := os.ReadDir(h.p("/sys/block"))
	for _, e := range ents {
		name := e.Name()
		if skipBlock(name) && !(h.loops && strings.HasPrefix(name, "loop")) {
			continue
		}
		base := "/sys/block/" + name
		d := &topoDev{Name: name, Size: h.num(base+"/size") * 512, ReadOnly: h.line(base+"/ro") == "1",
			Removable: h.line(base+"/removable") == "1"}
		switch {
		case strings.HasPrefix(name, "dm-"):
			d.Kind, d.DMName, d.DMUUID = "dm", h.line(base+"/dm/name"), h.line(base+"/dm/uuid")
			d.Slaves = h.slaves(base)
		case strings.HasPrefix(name, "md"):
			d.Kind, d.Slaves = "md", h.slaves(base)
		default:
			d.Kind = "disk"
			d.Model = h.line(base + "/device/model")
			d.Serial = h.line(base + "/device/serial")
			if d.Serial == "" {
				d.Serial = h.line(base + "/serial")
			}
			if link, err := os.Readlink(h.p(base)); err == nil && strings.Contains(link, "/usb") {
				d.Bus = "usb"
			}
			h.partitions(d, t.devs)
			if tb, ok := h.table(d); ok {
				t.tables[name] = tb
			}
		}
		t.devs[name] = d
	}
	t.mounts = h.mounts()
	for _, m := range t.mounts {
		if _, ok := t.fsinfo[m.Dev]; !ok {
			if info, ok := h.superblock(m.Dev, m.FS, m.Mount); ok {
				t.fsinfo[m.Dev] = info
			}
		}
	}
	h.readLVM(ctx, &t)
	return t
}

func skipBlock(name string) bool {
	for _, p := range []string{"loop", "ram", "zram", "sr", "fd", "nbd", "zd"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func (h diskHost) slaves(base string) []string {
	ents, _ := os.ReadDir(h.p(base + "/slaves"))
	out := []string{}
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func (h diskHost) partitions(d *topoDev, devs map[string]*topoDev) {
	base := "/sys/block/" + d.Name
	ents, _ := os.ReadDir(h.p(base))
	for _, e := range ents {
		pb := base + "/" + e.Name()
		if !strings.HasPrefix(e.Name(), d.Name) || h.line(pb+"/partition") == "" {
			continue
		}
		num, _ := strconv.Atoi(h.line(pb + "/partition"))
		devs[e.Name()] = &topoDev{Name: e.Name(), Kind: "part", Parent: d.Name, PartNum: num,
			Start: h.num(pb+"/start") * 512, Size: h.num(pb+"/size") * 512, ReadOnly: h.line(pb+"/ro") == "1"}
	}
}

// Partition type GUIDs and MBR type bytes worth naming.
var (
	gptKinds = map[string]string{
		"0657FD6D-A4AB-43C4-84E5-0933C84B4F4F": "swap",
		"DE94BBA4-06D1-4D40-A16A-BFD50179D6AC": "Windows recovery",
		"C12A7328-F81F-11D2-BA4B-00A0C93EC93B": "EFI",
		"21686148-6449-6E6F-744E-656564454649": "BIOS boot",
	}
	mbrKinds = map[byte]string{0x82: "swap", 0x05: "extended", 0x0f: "extended", 0x85: "extended", 0x27: "Windows recovery",
		0xef: "EFI"}
)

// table reads a disk's partition table from the disk itself: the scan
// runs as root. It reads; it never writes.
func (h diskHost) table(d *topoDev) (topoTable, bool) {
	ss := h.num("/sys/block/" + d.Name + "/queue/logical_block_size")
	if ss <= 0 {
		ss = 512
	}
	f, err := os.Open(h.p("/dev/" + d.Name))
	if err != nil {
		return topoTable{}, false
	}
	defer f.Close()
	buf := make([]byte, 2*ss)
	if _, err := f.ReadAt(buf, 0); err != nil {
		return topoTable{}, false
	}
	return parseTable(f, buf, ss, d.Size)
}

// parseTable reads a GPT or MBR table. head is the first two sectors.
func parseTable(f interface {
	ReadAt([]byte, int64) (int, error)
}, head []byte, ss, diskSize int64) (topoTable, bool) {
	if len(head) < int(2*ss) || head[510] != 0x55 || head[511] != 0xAA {
		if len(head) >= int(2*ss) && string(head[ss:ss+8]) == "EFI PART" {
			// A GPT without a protective MBR still reads.
		} else {
			return topoTable{}, false
		}
	}
	tb := topoTable{Kinds: map[int64]string{}}
	if string(head[ss:ss+8]) == "EFI PART" {
		g := head[ss:]
		entLBA := int64(binary.LittleEndian.Uint64(g[72:80]))
		n := int64(binary.LittleEndian.Uint32(g[80:84]))
		sz := int64(binary.LittleEndian.Uint32(g[84:88]))
		if n <= 0 || n > 1024 || sz < 128 || sz > 1024 {
			return topoTable{}, false
		}
		entSectors := (n*sz + ss - 1) / ss
		sectors := diskSize / ss
		// Once the backup header is at the end, partitions may reach up to
		// the backup entries before it.
		tb.Type, tb.UsableEnd = "gpt", (sectors-1-entSectors)*ss
		ents := make([]byte, n*sz)
		if _, err := f.ReadAt(ents, entLBA*ss); err == nil {
			for i := int64(0); i < n; i++ {
				e := ents[i*sz : (i+1)*sz]
				first := int64(binary.LittleEndian.Uint64(e[32:40]))
				if first == 0 {
					continue
				}
				if k := gptKinds[guidString(e[0:16])]; k != "" {
					tb.Kinds[first*ss] = k
				}
			}
		}
		return tb, true
	}
	tb.Type = "dos"
	limit := int64(1<<32) * ss
	tb.UsableEnd = diskSize
	if tb.UsableEnd > limit {
		tb.UsableEnd = limit
	}
	for i := 0; i < 4; i++ {
		e := head[446+16*i : 446+16*(i+1)]
		first := int64(binary.LittleEndian.Uint32(e[8:12]))
		if e[4] == 0xEE {
			return topoTable{}, false // a protective MBR whose GPT did not read
		}
		if first > 0 {
			if k := mbrKinds[e[4]]; k != "" {
				tb.Kinds[first*ss] = k
			}
		}
	}
	return tb, true
}

// guidString formats a GPT GUID, whose first three fields are little-endian.
func guidString(b []byte) string {
	return fmt.Sprintf("%08X-%04X-%04X-%X-%X", binary.LittleEndian.Uint32(b[0:4]), binary.LittleEndian.Uint16(b[4:6]),
		binary.LittleEndian.Uint16(b[6:8]), b[8:10], b[10:16])
}

// mounts lists real filesystems mounted from a block device, one line
// each, with the device's name in /sys/block.
func (h diskHost) mounts() []topoMount {
	f, err := os.Open(h.p("/proc/self/mountinfo"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []topoMount
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		left, right, ok := strings.Cut(sc.Text(), " - ")
		if !ok {
			continue
		}
		lf, rf := strings.Fields(left), strings.Fields(right)
		if len(lf) < 5 || len(rf) < 1 {
			continue
		}
		majmin, mount, fs := lf[2], unescapeMount(lf[4]), rf[0]
		if !realFS[fs] || skipMount(mount) || lf[3] != "/" && fs != "btrfs" {
			continue
		}
		link, err := os.Readlink(h.p("/sys/dev/block/" + majmin))
		if err != nil {
			continue
		}
		total, free, avail, ok := h.statfs(mount)
		if !ok || total == 0 {
			continue
		}
		out = append(out, topoMount{Mount: mount, FS: fs, Dev: filepath.Base(link), Total: total, Used: total - free,
			Avail: avail})
	}
	return out
}

// superblock reads a filesystem's own size and id from its device, and
// for XFS and btrfs asks the mounted filesystem itself: they write their
// superblock lazily, so the copy on the device can be from before a grow.
func (h diskHost) superblock(dev, fs, mount string) (topoFS, bool) {
	f, err := os.Open(h.p("/dev/" + dev))
	if err != nil {
		return topoFS{}, false
	}
	defer f.Close()
	info, ok := readSuperblock(f, fs)
	if !ok || h.root != "" {
		return info, ok
	}
	if size, devs, ok := mountedSize(mount, fs); ok {
		info.Size = size
		if devs > 0 {
			info.Devices = devs
		}
	}
	return info, true
}

// mountedSize asks a mounted XFS or btrfs filesystem for its size.
func mountedSize(mount, fs string) (size int64, devices int, ok bool) {
	d, err := os.Open(mount)
	if err != nil {
		return 0, 0, false
	}
	defer d.Close()
	switch fs {
	case "xfs":
		// XFS_IOC_FSGEOMETRY_V1: blocksize at 0, datablocks at 32.
		var geo [112]byte
		const ioc = 0x80705864
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, d.Fd(), ioc, uintptr(unsafe.Pointer(&geo[0]))); e != 0 {
			return 0, 0, false
		}
		bs := int64(binary.NativeEndian.Uint32(geo[0:4]))
		return bs * int64(binary.NativeEndian.Uint64(geo[32:40])), 0, true
	case "btrfs":
		// BTRFS_IOC_FS_INFO, then BTRFS_IOC_DEV_INFO for device 1.
		var fsInfo [1024]byte
		const iocFS = 0x8400941f // _IOR(0x94, 31, 1024)
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, d.Fd(), iocFS, uintptr(unsafe.Pointer(&fsInfo[0]))); e != 0 {
			return 0, 0, false
		}
		n := int(binary.NativeEndian.Uint64(fsInfo[8:16]))
		var dev [4096]byte
		binary.NativeEndian.PutUint64(dev[0:8], 1)
		const iocDev = 0xd000941e // _IOWR(0x94, 30, 4096)
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, d.Fd(), iocDev, uintptr(unsafe.Pointer(&dev[0]))); e != 0 {
			return 0, n, n > 0
		}
		return int64(binary.NativeEndian.Uint64(dev[32:40])), n, true
	}
	return 0, 0, false
}

func readSuperblock(f interface {
	ReadAt([]byte, int64) (int, error)
}, fs string) (topoFS, bool) {
	switch fs {
	case "ext2", "ext3", "ext4":
		b := make([]byte, 1024)
		if _, err := f.ReadAt(b, 1024); err != nil || binary.LittleEndian.Uint16(b[0x38:]) != 0xEF53 {
			return topoFS{}, false
		}
		blocks := uint64(binary.LittleEndian.Uint32(b[0x04:]))
		if binary.LittleEndian.Uint32(b[0x60:])&0x80 != 0 { // 64bit
			blocks |= uint64(binary.LittleEndian.Uint32(b[0x150:])) << 32
		}
		bs := uint64(1024) << binary.LittleEndian.Uint32(b[0x18:])
		return topoFS{Size: int64(blocks * bs), UUID: uuidString(b[0x68:0x78]), Devices: 1}, true
	case "xfs":
		b := make([]byte, 64)
		if _, err := f.ReadAt(b, 0); err != nil || string(b[0:4]) != "XFSB" {
			return topoFS{}, false
		}
		bs := uint64(binary.BigEndian.Uint32(b[4:]))
		blocks := binary.BigEndian.Uint64(b[8:])
		return topoFS{Size: int64(blocks * bs), UUID: uuidString(b[32:48]), Devices: 1}, true
	case "btrfs":
		b := make([]byte, 0x200)
		if _, err := f.ReadAt(b, 0x10000); err != nil || string(b[0x40:0x48]) != "_BHRfS_M" {
			return topoFS{}, false
		}
		devs := int(binary.LittleEndian.Uint64(b[0x88:]))
		// The device item says what of this device the filesystem uses.
		size := int64(binary.LittleEndian.Uint64(b[0xC9+8:]))
		return topoFS{Size: size, UUID: uuidString(b[0x20:0x30]), Devices: devs}, true
	}
	return topoFS{}, false
}

func uuidString(b []byte) string {
	if bytes.Equal(b, make([]byte, 16)) {
		return ""
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

/* ── LVM ─────────────────────────────────────────────────────────────────── */

// lvmPaths are where the lvm tool lives.
var lvmPaths = []string{"/usr/sbin/lvm", "/sbin/lvm", "/usr/bin/lvm"}

func lvmBinary() string {
	for _, p := range lvmPaths {
		if fileExists(p) {
			return p
		}
	}
	return ""
}

// runLVMReport runs one LVM report as JSON, in bytes. It only reads.
func runLVMReport(ctx context.Context, what, fields string) ([]byte, error) {
	bin := lvmBinary()
	if bin == "" {
		return nil, errNoLVM
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, what, "--reportformat", "json", "--units", "b", "--nosuffix",
		"--readonly", "-o", fields)
	cmd.Env = append(os.Environ(), "LVM_SUPPRESS_FD_WARNINGS=1")
	return cmd.Output()
}

var errNoLVM = errors.New("lvm is not installed")

type lvmJSON struct {
	Report []map[string][]map[string]string `json:"report"`
}

func lvmRows(b []byte, key string) []map[string]string {
	var j lvmJSON
	if json.Unmarshal(b, &j) != nil {
		return nil
	}
	var out []map[string]string
	for _, r := range j.Report {
		out = append(out, r[key]...)
	}
	return out
}

func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

func (h diskHost) readLVM(ctx context.Context, t *linuxTopo) {
	hasLVM := false
	for _, d := range t.devs {
		if d.Kind == "dm" && strings.HasPrefix(d.DMUUID, "LVM-") {
			hasLVM = true
		}
	}
	if !hasLVM || h.lvm == nil {
		return
	}
	if b, err := h.lvm(ctx, "pvs", "pv_name,pv_uuid,pv_size,dev_size,vg_name"); err == nil {
		for _, r := range lvmRows(b, "pv") {
			t.pvs = append(t.pvs, topoPV{Name: r["pv_name"], UUID: r["pv_uuid"], Size: atoi64(r["pv_size"]),
				DevSize: atoi64(r["dev_size"]), VG: r["vg_name"]})
		}
	}
	if b, err := h.lvm(ctx, "vgs", "vg_name,vg_uuid,vg_size,vg_free,vg_extent_size,pv_count"); err == nil {
		for _, r := range lvmRows(b, "vg") {
			t.vgs = append(t.vgs, topoVG{Name: r["vg_name"], UUID: r["vg_uuid"], Size: atoi64(r["vg_size"]),
				Free: atoi64(r["vg_free"]), Extent: atoi64(r["vg_extent_size"]), PVs: int(atoi64(r["pv_count"]))})
		}
	}
	if b, err := h.lvm(ctx, "lvs", "lv_name,vg_name,lv_uuid,lv_path,lv_dm_path,lv_size,lv_layout"); err == nil {
		for _, r := range lvmRows(b, "lv") {
			t.lvs = append(t.lvs, topoLV{Name: r["lv_name"], VG: r["vg_name"], UUID: strings.ReplaceAll(r["lv_uuid"], "-", ""),
				Path: r["lv_path"], DMPath: r["lv_dm_path"], Size: atoi64(r["lv_size"]), Layout: r["lv_layout"]})
		}
	}
}

/* ── Growing ─────────────────────────────────────────────────────────────── */

func hostDiskOps() diskOps { return linuxDiskOps{h: liveDiskHost} }

// rescanLoop is how a test makes a loop device see its file grew; on a
// server it is never called.
var rescanLoop func(dev string) error

type linuxDiskOps struct{ h diskHost }

func (o linuxDiskOps) scan(ctx context.Context, rescanDisk string) (DiskReport, error) {
	if rescanDisk != "" {
		if err := o.h.rescan(rescanDisk); err == nil {
			time.Sleep(time.Second)
		}
	}
	return o.h.scan(ctx, DiskScanRequest{})
}

// sbinPath is a PATH with the system tools on it, whatever the unit had.
const sbinPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

func (o linuxDiskOps) run(ctx context.Context, c diskCmd) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, c.Argv[0], c.Argv[1:]...)
	cmd.Env = append(os.Environ(), "PATH="+sbinPath, "LC_ALL=C", "LVM_SUPPRESS_FD_WARNINGS=1")
	if c.Stdin != "" {
		cmd.Stdin = strings.NewReader(c.Stdin)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// haveTool says whether a system tool is installed; tests replace it.
var haveTool = func(name string) bool {
	for _, dir := range strings.Split(sbinPath, ":") {
		if fileExists(filepath.Join(dir, name)) {
			return true
		}
	}
	return false
}

// steps are the commands that grow p's filesystem, bottom first.
func (o linuxDiskOps) steps(req DiskGrowRequest, p GrowPath) ([]diskCmd, error) {
	var out []diskCmd
	below := false
	if p.PartRoom > 0 {
		disk := "/dev/" + p.Disk
		n := strconv.Itoa(p.PartNum)
		if haveTool("growpart") {
			out = append(out, diskCmd{Title: "Grow " + p.Part, Argv: []string{"growpart", disk, n}})
		} else if haveTool("sfdisk") {
			if p.Table == "gpt" {
				out = append(out, diskCmd{Title: "Move the GPT backup header to the end of " + p.Disk,
					Argv: []string{"sfdisk", "--relocate", "gpt-bak-std", disk}})
			}
			out = append(out, diskCmd{Title: "Grow " + p.Part, Argv: []string{"sfdisk", "--no-reread", "--force", "-N", n, disk},
				Stdin: ", +\n"})
			out = append(out, diskCmd{Title: "Tell the kernel " + p.Part + " is bigger",
				Argv: []string{"partx", "-u", "--nr", n, disk}})
		} else {
			return nil, errors.New("neither growpart nor sfdisk is installed; install cloud-guest-utils or util-linux")
		}
		below = true
	}
	if p.Crypt != "" && (below || p.CryptRoom > 0) {
		out = append(out, diskCmd{Title: "Grow the encrypted volume " + p.Crypt, Argv: []string{"cryptsetup", "resize", p.Crypt}})
		below = true
	}
	if p.PV != "" && (below || p.PVRoom > 0) {
		out = append(out, diskCmd{Title: "Grow the LVM physical volume", Argv: []string{"lvm", "pvresize", p.PV}})
		below = true
	}
	if p.LV != "" && (below || p.VGFree > 0) {
		size := []string{"-l", "+100%FREE"}
		if req.Bytes > 0 {
			size = []string{"-L", "+" + strconv.FormatInt(req.Bytes, 10) + "b"}
		}
		out = append(out, diskCmd{Title: "Grow the LVM volume", Argv: append(append([]string{"lvm", "lvextend"}, size...), p.LV)})
	}
	switch p.FSType {
	case "ext3", "ext4":
		dev := p.FSDevice
		if p.LV != "" {
			dev = p.LV
		}
		out = append(out, diskCmd{Title: "Grow the filesystem", Argv: []string{"resize2fs", dev}})
	case "xfs":
		out = append(out, diskCmd{Title: "Grow the filesystem", Argv: []string{"xfs_growfs", p.Mount}})
	case "btrfs":
		out = append(out, diskCmd{Title: "Grow the filesystem", Argv: []string{"btrfs", "filesystem", "resize", "max", p.Mount}})
	default:
		return nil, fmt.Errorf("%s cannot grow while it is mounted", p.FSType)
	}
	return out, nil
}

func (o linuxDiskOps) table(ctx context.Context, p GrowPath) (diskCmd, bool) {
	if p.Disk == "" || p.Part == "" {
		return diskCmd{}, false
	}
	return diskCmd{Title: "Save the partition table", Argv: []string{"sfdisk", "--dump", "/dev/" + p.Disk}}, true
}

func (o linuxDiskOps) rescanDisk(p GrowPath) string {
	if p.Disk != "" && strings.HasPrefix(p.Disk, "sd") {
		return p.Disk
	}
	return ""
}
