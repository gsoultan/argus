# Growing disks

The agent reads each filesystem's whole stack and says whether it can grow,
and grows it online when the console asks or a tag's rule says so. Agent:
`internal/agent/disks*.go`. Control plane: `internal/control/disk_grow.go`
(types, store, planning, rules) and `disk_grow_routes.go` (heartbeat part,
routes, audit, alerts). Migration 064. Console: the server's Disks tab
(`web/src/components/server/DisksTab.tsx`), Settings › Disk growing
(`DiskGrowSettings.tsx`), calls in `web/src/lib/diskApi.ts`, queries in
`diskQueries.ts`. Live only: nothing here has a fixture.

## The stack and the answer

- Layers, disk first: disk → partition → crypt (LUKS) → pv → vg → lv → fs.
  Linux reads /sys, the partition table itself (GPT and MBR), `lvm ...
  --reportformat json`, and the superblock (ext, xfs, btrfs; a mounted XFS
  or btrfs is sized through its ioctl, the on-disk superblock is stale).
  Windows: Get-Disk/Get-Partition/Get-PartitionSupportedSize as JSON.
- Each filesystem gets one answer: `grow` (with `room` and a `path`),
  `full` (nothing to grow into), or `blocked` (room exists but something is
  in the way: a partition after it, a Windows recovery partition after C:,
  a filesystem that cannot grow mounted, MBR past 2 TiB, a read-only disk).
  Room under 64 MiB is not room.
- NVMe, virtio, SCSI and Xen disks all read the same way. `rescan` marks a
  SCSI `sd*` disk, which only sees a hypervisor's new size after
  `echo 1 > /sys/class/block/sdX/device/rescan`; NVMe and virtio see it on
  their own. "Look for more space" in the console asks for that rescan
  (Windows: Update-HostStorageCache).

## How a grow travels

- Same shape as log cleanup: everything rides the heartbeat (`disks` both
  ways). A run is on every reply until the agent reports how it ended;
  `sent_at` set means it cannot be taken back. No news for an hour: the
  control plane calls it interrupted (`step = 'stale'`).
- Linux: the scan and the grow run as root through `systemd-run` (scan with
  `--pipe --wait`, grow in a unit `argus-diskgrow-<id>`). Windows: in
  process, the grow in a goroutine.
- The console names a filesystem by the scan's key, never a device. The
  control plane plans only from the report it holds (`planDiskGrow`), and
  the helper scans again and refuses to write if anything moved
  (`diskSameLayout`): partition start/size, LVM UUIDs, filesystem size
  within 1%.
- Steps, bottom first: save the table (`sfdisk --dump`), growpart (or
  sfdisk `--relocate gpt-bak-std` + `-N n` + `partx -u`), `cryptsetup
  resize`, `lvm pvresize`, `lvm lvextend -l +100%FREE` (or `-L +Nb` for
  part of a group), then resize2fs / xfs_growfs / btrfs resize max. Every
  step only adds space, so a failure part way is safe to leave and a second
  grow carries on. Windows: Resize-Partition to SizeMax.
- The console's grow dialog shows the same commands
  (`plannedCommands` in DisksTab.tsx); keep it in step with
  `linuxDiskOps.steps` and `winGrowSteps`.

## Rules

- `disk_grow_rules`, per tag, first match wins, exactly one rule for every
  other server (tag ''). `needs_approval`: a grow by hand waits for a second
  admin (never the asker). `auto_percent` (0, or 50–99): grow the fullest
  filesystem past it that has room, at most once a day per server
  (`auto_day`), never into space the server does not already have.
- Seeded: 'production' needs approval; every other server by hand, no
  approval. Nothing grows on its own out of the box.

## Not here yet

- Making a cloud volume bigger (EBS ModifyVolume and the like) belongs to
  the cloud side; it would call this grow afterwards, matching the disk by
  `disk_serial` (an EBS volume's NVMe serial is its volume id without the
  dash).
