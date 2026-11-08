package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

/*
Windows disks, as Storage's own cmdlets describe them (disks_windows.go
prints them as JSON). This half only reads that JSON, so it is tested on any
host.

Windows does the hard part itself: Get-PartitionSupportedSize says how far a
partition can grow, and Resize-Partition grows the partition and its NTFS or
ReFS volume together, online. What Argus adds is saying why when it cannot,
above all the recovery partition Windows Setup puts right after C:.
*/

// winScanScript prints the disks and partitions as JSON.
const winScanScript = `$ErrorActionPreference = 'Stop'
$cs = Get-CimInstance Win32_ComputerSystem
$out = [ordered]@{ vendor = "$($cs.Manufacturer)"; model = "$($cs.Model)"; disks = @(); parts = @() }
foreach ($d in Get-Disk) {
  $out.disks += [pscustomobject]@{ number = [int]$d.Number; size = [int64]$d.Size; allocated = [int64]$d.AllocatedSize;
    style = "$($d.PartitionStyle)"; bus = "$($d.BusType)"; serial = "$($d.SerialNumber)".Trim(); model = "$($d.FriendlyName)";
    ro = [bool]$d.IsReadOnly; offline = [bool]$d.IsOffline }
}
foreach ($p in Get-Partition) {
  $max = [int64]0
  try { $max = [int64](Get-PartitionSupportedSize -DiskNumber $p.DiskNumber -PartitionNumber $p.PartitionNumber -ErrorAction Stop).SizeMax } catch {}
  $v = $null
  try { $v = $p | Get-Volume -ErrorAction Stop } catch {}
  $letter = "$($p.DriveLetter)".Trim([char]0).Trim()
  $o = [pscustomobject]@{ disk = [int]$p.DiskNumber; number = [int]$p.PartitionNumber; offset = [int64]$p.Offset;
    size = [int64]$p.Size; max = $max; letter = $letter; type = "$($p.Type)"; paths = @($p.AccessPaths);
    fs = ''; label = ''; vsize = [int64]0; vfree = [int64]0; uuid = '' }
  if ($v) { $o.fs = "$($v.FileSystem)"; $o.label = "$($v.FileSystemLabel)"; $o.vsize = [int64]$v.Size;
    $o.vfree = [int64]$v.SizeRemaining; $o.uuid = "$($v.UniqueId)" }
  $out.parts += $o
}
$out | ConvertTo-Json -Depth 4 -Compress`

type winDiskJSON struct {
	Vendor string `json:"vendor"`
	Model  string `json:"model"`
	Disks  []struct {
		Number    int    `json:"number"`
		Size      int64  `json:"size"`
		Allocated int64  `json:"allocated"`
		Style     string `json:"style"`
		Bus       string `json:"bus"`
		Serial    string `json:"serial"`
		Model     string `json:"model"`
		RO        bool   `json:"ro"`
		Offline   bool   `json:"offline"`
	} `json:"disks"`
	Parts []winPart `json:"parts"`
}

type winPart struct {
	Disk   int      `json:"disk"`
	Number int      `json:"number"`
	Offset int64    `json:"offset"`
	Size   int64    `json:"size"`
	Max    int64    `json:"max"`
	Letter string   `json:"letter"`
	Type   string   `json:"type"`
	Paths  []string `json:"paths"`
	FS     string   `json:"fs"`
	Label  string   `json:"label"`
	VSize  int64    `json:"vsize"`
	VFree  int64    `json:"vfree"`
	UUID   string   `json:"uuid"`
}

// mount is how the console names the volume: its letter, or a folder it
// is mounted at.
func (p winPart) mount() string {
	if p.Letter != "" {
		return strings.ToUpper(p.Letter) + ":"
	}
	for _, a := range p.Paths {
		if !strings.HasPrefix(a, `\\?\`) {
			return strings.TrimSuffix(a, `\`)
		}
	}
	return ""
}

var winGrowFS = map[string]bool{"NTFS": true, "ReFS": true}

// parseWinDisks answers each mounted Windows volume.
func parseWinDisks(b []byte) (DiskReport, error) {
	var j winDiskJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return DiskReport{}, fmt.Errorf("cannot read what Windows said about its disks: %w", err)
	}
	r := DiskReport{OS: "windows", Supported: true, Filesystems: []DiskFS{}, Disks: []BlockDisk{},
		Hypervisor: hypervisorFromDMI(j.Vendor, j.Model)}
	byDisk := map[int][]winPart{}
	for _, p := range j.Parts {
		byDisk[p.Disk] = append(byDisk[p.Disk], p)
	}
	for _, d := range j.Disks {
		parts := byDisk[d.Number]
		sort.Slice(parts, func(a, b int) bool { return parts[a].Offset < parts[b].Offset })
		unalloc := roomOrZero(d.Size - d.Allocated)
		name := fmt.Sprintf("disk%d", d.Number)
		r.Disks = append(r.Disks, BlockDisk{Name: name, Size: d.Size, Bus: strings.ToLower(d.Bus), Model: d.Model,
			Serial: d.Serial, Table: strings.ToLower(d.Style), Unallocated: unalloc, ReadOnly: d.RO})
		detail := strings.TrimSpace(strings.Join(nonEmpty(d.Bus, strings.ToUpper(d.Style)), " · "))
		for i, p := range parts {
			m := p.mount()
			if m == "" || p.FS == "" {
				continue
			}
			f := DiskFS{Key: "mount:" + m, Mount: m, FS: p.FS, Total: uint64(p.VSize), Used: uint64(p.VSize - p.VFree),
				Avail: uint64(p.VFree), Unallocated: unalloc, Rescan: !strings.EqualFold(d.Bus, "USB")}
			if p.UUID != "" {
				f.Key = "uuid:" + p.UUID
			}
			f.Layers = []DiskLayer{
				{Kind: "disk", Name: fmt.Sprintf("Disk %d", d.Number), Size: d.Size, Detail: detail, Room: unalloc},
				{Kind: "partition", Name: partName(p), Size: p.Size, Offset: p.Offset, Room: roomOrZero(p.Max - p.Size)},
				{Kind: "fs", Name: p.FS, Size: p.VSize, Used: p.VSize - p.VFree},
			}
			room := roomOrZero(p.Max - p.Size)
			var blocker string
			switch {
			case !winGrowFS[p.FS]:
				blocker = p.FS + " cannot grow while it is in use"
			case d.RO:
				blocker = "this disk is read-only"
			case d.Offline:
				blocker = "this disk is offline"
			case strings.EqualFold(d.Bus, "Spaces"):
				blocker = "this is a Storage Spaces disk; grow its virtual disk in Storage Spaces first"
			case room == 0 && unalloc > 0 && i+1 < len(parts):
				n := parts[i+1]
				if strings.EqualFold(n.Type, "Recovery") {
					blocker = "the Windows recovery partition comes right after it, between it and the free space. " +
						"Microsoft's way out: turn WinRE off (reagentc /disable), remove that partition, grow " + m +
						", and create it again at the end of the disk"
				} else {
					blocker = partName(n) + " comes right after it, between it and the free space; a partition can " +
						"only grow into the space right after it"
				}
			}
			switch {
			case blocker != "" && (room > 0 || unalloc > 0):
				f.Answer, f.Why = DiskBlocked, sentence(blocker)
			case blocker != "":
				f.Answer, f.Why = DiskFull, "Uses all of its disk. "+sentence(blocker)
			case room > 0:
				f.Answer, f.Room = DiskGrow, room
				f.Why = "Growing extends the partition and " + p.FS + " together, online."
				f.Path = &GrowPath{Disk: name, DiskSize: d.Size, DiskSerial: d.Serial, DiskNumber: d.Number,
					PartitionNumber: p.Number, PartStart: p.Offset, PartSize: p.Size, PartRoom: room, SizeMax: p.Max,
					Table: strings.ToLower(d.Style), FSType: p.FS, FSSize: p.Size, Mount: m}
			default:
				f.Answer, f.Why = DiskFull, "Uses all of its disk."
			}
			r.Filesystems = append(r.Filesystems, f)
		}
	}
	sort.SliceStable(r.Filesystems, func(a, b int) bool {
		pa, pb := diskPercentOf(r.Filesystems[a]), diskPercentOf(r.Filesystems[b])
		if pa != pb {
			return pa > pb
		}
		return r.Filesystems[a].Mount < r.Filesystems[b].Mount
	})
	return r, nil
}

func partName(p winPart) string {
	if m := p.mount(); m != "" {
		return m
	}
	if strings.EqualFold(p.Type, "Recovery") {
		return "Recovery"
	}
	return fmt.Sprintf("Partition %d", p.Number)
}

func nonEmpty(xs ...string) []string {
	var out []string
	for _, x := range xs {
		if strings.TrimSpace(x) != "" && x != "Unknown" {
			out = append(out, x)
		}
	}
	return out
}

// winGrowSteps are the commands that grow a Windows volume.
func winGrowSteps(p GrowPath) []diskCmd {
	sel := fmt.Sprintf("-DiskNumber %d -PartitionNumber %d", p.DiskNumber, p.PartitionNumber)
	script := fmt.Sprintf("$ErrorActionPreference = 'Stop'; $m = (Get-PartitionSupportedSize %s).SizeMax; "+
		"Resize-Partition %s -Size $m; \"now $m bytes\"", sel, sel)
	return []diskCmd{{Title: "Grow " + p.Mount, Argv: powershellArgv(script),
		Show: fmt.Sprintf("Resize-Partition %s -Size (Get-PartitionSupportedSize %s).SizeMax", sel, sel)}}
}

func winTableCmd(p GrowPath) diskCmd {
	script := fmt.Sprintf("Get-Partition -DiskNumber %d | Format-Table PartitionNumber,DriveLetter,Offset,Size,Type,GptType "+
		"-AutoSize | Out-String -Width 220", p.DiskNumber)
	return diskCmd{Title: "Save the partition table", Argv: powershellArgv(script),
		Show: fmt.Sprintf("Get-Partition -DiskNumber %d", p.DiskNumber)}
}

func powershellArgv(script string) []string {
	return []string{"powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-Command", script}
}
