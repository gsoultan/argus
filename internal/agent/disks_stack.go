package agent

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

/*
The stack under a Linux filesystem, and whether it can grow.

The Linux scan (disks_linux.go) reads the parts: block devices from /sys,
partition tables, the LVM report, the mount table and each filesystem's own
size from its superblock. This file puts them together without touching
the host, so every layout can be tested from a description of it.

Space reaches a filesystem from the bottom up: a disk made bigger leaves room
after its last partition; the partition grows into it; the LVM physical
volume on it grows to the partition; its volume group gains free space; the
logical volume takes it; the filesystem grows to the volume. Room can already
be waiting at any of those layers (a partition never grown after the disk
was, a volume group with free space, a filesystem smaller than its device),
and a grow takes it from every layer at once.
*/

// topoDev is a block device, as /sys describes it.
type topoDev struct {
	Name string
	// Kind is disk, part, dm or md.
	Kind string
	Size int64
	// Parent is a partition's disk, PartNum its number and Start where it
	// begins, in bytes.
	Parent  string
	PartNum int
	Start   int64
	// Slaves are what a device-mapper or md device is built on.
	Slaves         []string
	DMName, DMUUID string
	ReadOnly       bool
	Removable      bool
	Bus            string
	Model, Serial  string
}

// topoTable is a disk's partition table.
type topoTable struct {
	// Type is gpt or dos.
	Type string
	// UsableEnd is where the last partition may end, in bytes, once a GPT
	// backup header is moved to the end of a disk that grew.
	UsableEnd int64
	// Kinds names partitions by what they are, where the table says: swap,
	// recovery, efi, extended. Keyed by start in bytes.
	Kinds map[int64]string
}

type topoPV struct {
	Name          string
	UUID          string
	Size, DevSize int64
	VG            string
}

type topoVG struct {
	Name, UUID         string
	Size, Free, Extent int64
	PVs                int
}

type topoLV struct {
	Name, VG, UUID, Path, DMPath, Layout string
	Size                                 int64
}

type topoMount struct {
	Mount, FS, Dev     string
	Total, Used, Avail uint64
}

// topoFS is what a filesystem's superblock says about it.
type topoFS struct {
	Size    int64
	UUID    string
	Devices int
}

type linuxTopo struct {
	devs   map[string]*topoDev
	tables map[string]topoTable
	pvs    []topoPV
	vgs    []topoVG
	lvs    []topoLV
	mounts []topoMount
	fsinfo map[string]topoFS
}

// onlineGrow are the filesystems that grow while mounted.
var onlineGrow = map[string]string{
	"ext3": "resize2fs", "ext4": "resize2fs", "xfs": "xfs_growfs", "btrfs": "btrfs",
}

const mib = 1 << 20

func (t linuxTopo) dev(name string) *topoDev {
	if d, ok := t.devs[name]; ok {
		return d
	}
	return nil
}

// byDevPath finds the device a /dev path names: /dev/sda3, /dev/dm-2,
// /dev/mapper/<name>.
func (t linuxTopo) byDevPath(p string) *topoDev {
	if strings.HasPrefix(p, "/dev/mapper/") {
		name := strings.TrimPrefix(p, "/dev/mapper/")
		for _, d := range t.devs {
			if d.Kind == "dm" && d.DMName == name {
				return d
			}
		}
		return nil
	}
	return t.dev(path.Base(p))
}

func (t linuxTopo) lvFor(d *topoDev) *topoLV {
	for i := range t.lvs {
		lv := &t.lvs[i]
		if (lv.DMPath != "" && path.Base(lv.DMPath) == d.DMName) || strings.HasSuffix(d.DMUUID, lv.UUID) && lv.UUID != "" {
			return lv
		}
	}
	return nil
}

func (t linuxTopo) vg(name string) *topoVG {
	for i := range t.vgs {
		if t.vgs[i].Name == name {
			return &t.vgs[i]
		}
	}
	return nil
}

func (t linuxTopo) pvsOf(vg string) []topoPV {
	var out []topoPV
	for _, p := range t.pvs {
		if p.VG == vg {
			out = append(out, p)
		}
	}
	return out
}

// partsOf lists a disk's partitions by start.
func (t linuxTopo) partsOf(disk string) []*topoDev {
	var out []*topoDev
	for _, d := range t.devs {
		if d.Kind == "part" && d.Parent == disk {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}

// unallocated is what no partition holds on a disk with a table.
func (t linuxTopo) unallocated(disk *topoDev) int64 {
	tb, ok := t.tables[disk.Name]
	if !ok {
		return 0
	}
	end := tb.UsableEnd
	if end <= 0 || end > disk.Size {
		end = disk.Size
	}
	var free int64
	at := int64(mib) // the first mebibyte is the table's, by convention
	for _, p := range t.partsOf(disk.Name) {
		if tb.Kinds[p.Start] == "extended" {
			continue // its logical partitions are listed on their own
		}
		if p.Start > at {
			free += p.Start - at
		}
		if e := p.Start + p.Size; e > at {
			at = e
		}
	}
	if end > at {
		free += end - at
	}
	if free < diskMinRoom {
		return 0
	}
	return free
}

// partRoom is how far a partition can grow: to the next partition, or the
// end of what the table can hold. next names what is in the way when room
// on the disk lies beyond it.
func (t linuxTopo) partRoom(p *topoDev) (room int64, next *topoDev, why string) {
	disk := t.dev(p.Parent)
	tb, ok := t.tables[p.Parent]
	if disk == nil || !ok {
		return 0, nil, "its partition table could not be read"
	}
	if tb.Type == "dos" && p.PartNum >= 5 {
		return 0, nil, fmt.Sprintf("%s is a logical partition inside an extended one, which Argus does not grow", p.Name)
	}
	limit := tb.UsableEnd
	if limit <= 0 || limit > disk.Size {
		limit = disk.Size
	}
	for _, o := range t.partsOf(p.Parent) {
		if o.Start > p.Start && o.Start < limit {
			limit, next = o.Start, o
		}
	}
	end := p.Start + p.Size
	if limit <= end {
		return 0, next, ""
	}
	return (limit - end) / mib * mib, next, ""
}

func busOf(d *topoDev) string {
	switch {
	case d.Bus != "":
		return d.Bus
	case strings.HasPrefix(d.Name, "nvme"):
		return "nvme"
	case strings.HasPrefix(d.Name, "vd"):
		return "virtio"
	case strings.HasPrefix(d.Name, "xvd"):
		return "xen"
	case strings.HasPrefix(d.Name, "sd"):
		return "scsi"
	case strings.HasPrefix(d.Name, "mmcblk"):
		return "mmc"
	}
	return ""
}

func diskDetail(d *topoDev, tb topoTable) string {
	var parts []string
	if b := busOf(d); b != "" {
		parts = append(parts, map[string]string{"nvme": "NVMe", "virtio": "virtio", "xen": "Xen", "scsi": "SCSI",
			"mmc": "MMC"}[b])
	}
	if tb.Type != "" {
		parts = append(parts, strings.ToUpper(map[string]string{"gpt": "gpt", "dos": "mbr"}[tb.Type]))
	}
	return strings.Join(parts, " · ")
}

// analyze puts each mounted filesystem's stack together and answers it.
func (t linuxTopo) analyze() ([]DiskFS, []BlockDisk) {
	out := []DiskFS{}
	seen := map[string]bool{}
	for _, m := range t.mounts {
		if seen[m.Dev] {
			continue
		}
		d := t.dev(m.Dev)
		if d == nil {
			continue
		}
		seen[m.Dev] = true
		out = append(out, t.stack(m, d))
	}
	sort.SliceStable(out, func(i, j int) bool {
		pi, pj := diskPercentOf(out[i]), diskPercentOf(out[j])
		if pi != pj {
			return pi > pj
		}
		return out[i].Mount < out[j].Mount
	})
	disks := []BlockDisk{}
	for _, d := range t.devs {
		if d.Kind != "disk" {
			continue
		}
		tb := t.tables[d.Name]
		disks = append(disks, BlockDisk{Name: d.Name, Size: d.Size, Bus: busOf(d), Model: d.Model, Serial: d.Serial,
			Table: tb.Type, Unallocated: t.unallocated(d), ReadOnly: d.ReadOnly})
	}
	sort.Slice(disks, func(i, j int) bool { return disks[i].Name < disks[j].Name })
	return out, disks
}

func diskPercentOf(f DiskFS) int {
	usable := f.Used + f.Avail
	if usable == 0 {
		return 0
	}
	return int((f.Used*100 + usable - 1) / usable)
}

// stack walks down from a mounted filesystem to its disk.
func (t linuxTopo) stack(m topoMount, top *topoDev) DiskFS {
	f := DiskFS{Mount: m.Mount, FS: m.FS, Total: m.Total, Used: m.Used, Avail: m.Avail, Layers: []DiskLayer{}}
	info, known := t.fsinfo[top.Name]
	f.Key = "mount:" + m.Mount
	if info.UUID != "" {
		f.Key = "uuid:" + info.UUID
	}
	p := &GrowPath{FSDevice: "/dev/" + top.Name, FSType: m.FS, Mount: m.Mount}
	fsSize := info.Size
	if !known || fsSize <= 0 {
		fsSize = int64(m.Total)
	}
	p.FSSize = fsSize
	var (
		layers   []DiskLayer // top first; reversed at the end
		blockers []string
	)
	fsLayer := DiskLayer{Kind: "fs", Name: m.FS, Size: fsSize, Used: int64(m.Used)}
	if known && top.Size-fsSize >= diskMinRoom {
		p.FSRoom = top.Size - fsSize
		fsLayer.Room = p.FSRoom
	}
	layers = append(layers, fsLayer)
	if _, ok := onlineGrow[m.FS]; !ok {
		blockers = append(blockers, fmt.Sprintf("%s cannot grow while it is mounted", m.FS))
	}
	if m.FS == "btrfs" && info.Devices > 1 {
		blockers = append(blockers, "this btrfs filesystem spans several devices, which Argus does not grow")
	}

	cur := top
	var disk *topoDev
walk:
	for depth := 0; cur != nil && depth < 8; depth++ {
		switch {
		case cur.Kind == "dm" && strings.HasPrefix(cur.DMUUID, "LVM-"):
			lv := t.lvFor(cur)
			if lv == nil {
				blockers = append(blockers, "the LVM report does not name "+cur.DMName)
				break walk
			}
			vg := t.vg(lv.VG)
			layers = append(layers, DiskLayer{Kind: "lv", Name: lv.Name, Size: lv.Size, Detail: lv.Layout})
			p.LV, p.LVUUID, p.LVSize = lv.Path, lv.UUID, lv.Size
			f.LVM = true
			if !lvGrowable(lv.Layout) {
				blockers = append(blockers, fmt.Sprintf("%s is a %s volume, which Argus does not grow", lv.Name, lv.Layout))
			}
			if vg == nil {
				blockers = append(blockers, "the LVM report does not name the group "+lv.VG)
				break walk
			}
			layers = append(layers, DiskLayer{Kind: "vg", Name: vg.Name, Size: vg.Size, Used: vg.Size - vg.Free,
				Room: roomOrZero(vg.Free)})
			p.VG, p.VGUUID, p.VGFree = vg.Name, vg.UUID, roomOrZero(vg.Free)
			pvs := t.pvsOf(vg.Name)
			if len(pvs) != 1 {
				// Its free space still counts; what is under several
				// physical volumes is not followed.
				disk = nil
				cur = nil
				break walk
			}
			pv := pvs[0]
			p.PV = pv.Name
			layers = append(layers, DiskLayer{Kind: "pv", Name: pv.Name, Size: pv.Size, Room: roomOrZero(pv.DevSize - pv.Size)})
			p.PVRoom = roomOrZero(pv.DevSize - pv.Size)
			cur = t.byDevPath(pv.Name)
			if cur == nil {
				blockers = append(blockers, "the device under "+pv.Name+" could not be found")
				break walk
			}
		case cur.Kind == "dm" && strings.HasPrefix(cur.DMUUID, "CRYPT-"):
			if len(cur.Slaves) != 1 {
				blockers = append(blockers, "the encrypted volume "+cur.DMName+" is on more than one device")
				break walk
			}
			below := t.dev(cur.Slaves[0])
			l := DiskLayer{Kind: "crypt", Name: cur.DMName, Size: cur.Size, Detail: "LUKS"}
			if below != nil {
				// The LUKS header takes at most 16 MiB at the start.
				l.Room = roomOrZero(below.Size - cur.Size - 16*mib)
			}
			p.Crypt, p.CryptRoom = cur.DMName, l.Room
			layers = append(layers, l)
			cur = below
		case cur.Kind == "dm":
			blockers = append(blockers, fmt.Sprintf("%s is a device-mapper volume Argus does not grow", cur.DMName))
			break walk
		case cur.Kind == "md":
			blockers = append(blockers, cur.Name+" is a software RAID, which Argus does not grow")
			break walk
		case cur.Kind == "part":
			disk = t.dev(cur.Parent)
			room, next, why := t.partRoom(cur)
			if why != "" {
				blockers = append(blockers, why)
			}
			tb := t.tables[cur.Parent]
			layers = append(layers, DiskLayer{Kind: "partition", Name: cur.Name, Size: cur.Size, Room: roomOrZero(room),
				Offset: cur.Start})
			p.Part, p.PartNum, p.PartStart, p.PartSize, p.PartRoom, p.Table = cur.Name, cur.PartNum, cur.Start, cur.Size,
				roomOrZero(room), tb.Type
			if disk != nil && p.PartRoom == 0 && next != nil && t.unallocated(disk) >= diskMinRoom {
				what := next.Name
				if k := tb.Kinds[next.Start]; k != "" {
					what = fmt.Sprintf("the %s partition %s", k, next.Name)
				}
				blockers = append(blockers, fmt.Sprintf("%s comes right after it, between it and the free space; "+
					"a partition can only grow into the space right after it", what))
			}
			if disk != nil && tb.Type == "dos" && disk.Size > 1<<41 && cur.Start+cur.Size+room >= 1<<41 {
				blockers = append(blockers, "an MBR partition table cannot hold more than 2 TiB")
			}
			cur = nil
		case cur.Kind == "disk":
			disk = cur
			cur = nil
		default:
			blockers = append(blockers, cur.Name+" is not a disk Argus grows")
			break walk
		}
	}
	if disk != nil {
		tb := t.tables[disk.Name]
		layers = append(layers, DiskLayer{Kind: "disk", Name: disk.Name, Size: disk.Size, Detail: diskDetail(disk, tb),
			Room: t.unallocated(disk)})
		p.Disk, p.DiskSize, p.DiskSerial = disk.Name, disk.Size, disk.Serial
		f.Unallocated = t.unallocated(disk)
		f.Rescan = busOf(disk) == "scsi" && !disk.Removable
		if disk.ReadOnly {
			blockers = append(blockers, disk.Name+" is read-only")
		}
	}
	for i, j := 0, len(layers)-1; i < j; i, j = i+1, j-1 {
		layers[i], layers[j] = layers[j], layers[i]
	}
	f.Layers = layers
	f.Room = p.PartRoom + p.CryptRoom + p.PVRoom + p.VGFree + p.FSRoom
	switch {
	case len(blockers) > 0 && (f.Room >= diskMinRoom || f.Unallocated >= diskMinRoom):
		f.Answer, f.Why = DiskBlocked, sentence(blockers[0])
	case len(blockers) > 0:
		// Nothing to grow into anyway; say why it could not, quietly.
		f.Answer, f.Why = DiskFull, "Uses all of its disk. "+sentence(blockers[0])
	case f.Room >= diskMinRoom:
		f.Answer, f.Path = DiskGrow, p
		var steps []string
		if p.PartRoom > 0 {
			steps = append(steps, p.Part)
		}
		if p.CryptRoom > 0 || (p.Crypt != "" && p.PartRoom > 0) {
			steps = append(steps, "the encryption")
		}
		if p.LV != "" {
			steps = append(steps, "the LVM volume")
		}
		steps = append(steps, "the filesystem")
		f.Why = "Growing extends " + joinAnd(steps) + ", online."
	default:
		f.Answer, f.Why = DiskFull, "Uses all of its disk."
	}
	return f
}

func lvGrowable(layout string) bool {
	if layout == "" {
		return true
	}
	for _, l := range strings.Split(layout, ",") {
		switch strings.TrimSpace(l) {
		case "linear", "striped":
		default:
			return false
		}
	}
	return true
}

func roomOrZero(n int64) int64 {
	if n < diskMinRoom {
		return 0
	}
	return n
}

func sentence(s string) string {
	if s == "" {
		return s
	}
	// Capitalise a sentence that starts with a word, never a device name.
	first, _, _ := strings.Cut(s, " ")
	switch first {
	case "the", "this", "its", "an", "a", "neither", "nothing", "there":
		s = strings.ToUpper(s[:1]) + s[1:]
	}
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

func joinAnd(xs []string) string {
	switch len(xs) {
	case 0:
		return ""
	case 1:
		return xs[0]
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " and " + xs[len(xs)-1]
}
