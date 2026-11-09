package cloud

import (
	"context"
	"regexp"
	"strings"
)

/*
Disks, snapshots, images and addresses: the storage half of a cloud
account, changed through the same requested -> (approved) -> applied path
as instances. A Storage provider only carries a change out; the control
plane decides whether it may run.
*/

// Kinds read for this phase.
const (
	KindSnapshot Kind = "snapshot"
	KindImage    Kind = "image" // an image this account owns
)

// Storage actions.
const (
	ActionVolumeCreate        Action = "volume-create"
	ActionVolumeGrow          Action = "volume-grow"
	ActionVolumeAttach        Action = "volume-attach"
	ActionVolumeDetach        Action = "volume-detach"
	ActionVolumeDelete        Action = "volume-delete"
	ActionSnapshotCreate      Action = "snapshot-create"
	ActionSnapshotDelete      Action = "snapshot-delete"
	ActionImageCreate         Action = "image-create"
	ActionImageDelete         Action = "image-delete"
	ActionAddressAllocate     Action = "address-allocate"
	ActionAddressAssociate    Action = "address-associate"
	ActionAddressDisassociate Action = "address-disassociate"
	ActionAddressRelease      Action = "address-release"
)

// StorageActions are the actions a Storage provider carries out.
var StorageActions = []Action{
	ActionVolumeCreate, ActionVolumeGrow, ActionVolumeAttach, ActionVolumeDetach, ActionVolumeDelete,
	ActionSnapshotCreate, ActionSnapshotDelete, ActionImageCreate, ActionImageDelete,
	ActionAddressAllocate, ActionAddressAssociate, ActionAddressDisassociate, ActionAddressRelease,
}

// IsStorage reports whether a is one of StorageActions.
func IsStorage(a Action) bool {
	for _, x := range StorageActions {
		if x == a {
			return true
		}
	}
	return false
}

// KindOf is the kind of resource a storage action works on: the volume a
// grow changes, the instance an image is made from. A create works on
// nothing yet and answers the kind it makes.
func KindOf(a Action) Kind {
	switch a {
	case ActionVolumeCreate, ActionVolumeGrow, ActionVolumeAttach, ActionVolumeDetach, ActionVolumeDelete, ActionSnapshotCreate:
		return KindVolume
	case ActionSnapshotDelete:
		return KindSnapshot
	case ActionImageCreate:
		return KindInstance
	case ActionImageDelete:
		return KindImage
	case ActionAddressAllocate, ActionAddressAssociate, ActionAddressDisassociate, ActionAddressRelease:
		return KindAddress
	}
	return ""
}

// VolumeTypes are the disk types Argus offers, cheapest general purpose
// first.
var VolumeTypes = []string{"gp3", "gp2", "io2", "io1", "st1", "sc1"}

// ValidVolumeType reports whether t is one of VolumeTypes.
func ValidVolumeType(t string) bool {
	for _, x := range VolumeTypes {
		if x == t {
			return true
		}
	}
	return false
}

// VolumeLimits is the size range of a disk type, in GiB.
func VolumeLimits(t string) (min, max int32) {
	switch t {
	case "io1", "io2":
		return 4, 16384
	case "st1", "sc1":
		return 125, 16384
	}
	return 1, 16384
}

// VolumeSpec is a new disk.
type VolumeSpec struct {
	Zone   string `json:"zone"`
	SizeGB int32  `json:"sizeGb"`
	Type   string `json:"type"`
	// IOPS and Throughput apply to gp3 (and IOPS to io1 and io2); zero
	// takes the type's baseline.
	IOPS       int32  `json:"iops,omitempty"`
	Throughput int32  `json:"throughput,omitempty"`
	Snapshot   string `json:"snapshot,omitempty"`
	Name       string `json:"name"`
	// AttachTo, with Device, attaches it to an instance once it is ready.
	AttachTo string            `json:"attachTo,omitempty"`
	Device   string            `json:"device,omitempty"`
	Tags     map[string]string `json:"tags,omitempty"`
}

// VolumeChange grows a disk or changes its type or performance. Zero
// fields stay as they are.
type VolumeChange struct {
	SizeGB     int32  `json:"sizeGb,omitempty"`
	Type       string `json:"type,omitempty"`
	IOPS       int32  `json:"iops,omitempty"`
	Throughput int32  `json:"throughput,omitempty"`
}

// Storage is a provider that changes disks, snapshots, images and
// addresses. Every call runs as actor, with the account's manage role.
type Storage interface {
	CreateVolume(ctx context.Context, a Account, actor Actor, region string, s VolumeSpec) (Result, error)
	ModifyVolume(ctx context.Context, a Account, actor Actor, region, id string, c VolumeChange) (Result, error)
	AttachVolume(ctx context.Context, a Account, actor Actor, region, volume, instance, device string) (Result, error)
	DetachVolume(ctx context.Context, a Account, actor Actor, region, volume string) (Result, error)
	DeleteVolume(ctx context.Context, a Account, actor Actor, region, volume string, finalSnapshot bool) (Result, error)
	CreateSnapshot(ctx context.Context, a Account, actor Actor, region, volume, name string) (Result, error)
	DeleteSnapshot(ctx context.Context, a Account, actor Actor, region, snapshot string) (Result, error)
	CreateImage(ctx context.Context, a Account, actor Actor, region, instance, name string, reboot bool) (Result, error)
	DeleteImage(ctx context.Context, a Account, actor Actor, region, image string, withSnapshots bool) (Result, error)
	AllocateAddress(ctx context.Context, a Account, actor Actor, region, name string) (Result, error)
	AssociateAddress(ctx context.Context, a Account, actor Actor, region, address, instance string) (Result, error)
	DisassociateAddress(ctx context.Context, a Account, actor Actor, region, address string) (Result, error)
	ReleaseAddress(ctx context.Context, a Account, actor Actor, region, address string) (Result, error)
}

// PolicyChecker is a provider that can say which manage permissions an
// account's manage role lacks, reading the role's policy with the read
// role. An account connected before a phase added permissions lacks that
// phase's, until its stack is updated.
type PolicyChecker interface {
	ManageMissing(ctx context.Context, read Account, manageRoleARN string) ([]string, error)
	// Needs is the permissions an action takes.
	Needs(a Action) []string
}

var (
	reDevice    = regexp.MustCompile(`^/dev/(sd[f-p]|xvd[f-p])$`)
	reImageName = regexp.MustCompile(`^[A-Za-z0-9()\[\] ./'@_-]{3,128}$`)
)

// Devices are the device names Argus attaches disks as, in the order it
// picks them.
var Devices = []string{"/dev/sdf", "/dev/sdg", "/dev/sdh", "/dev/sdi", "/dev/sdj", "/dev/sdk", "/dev/sdl",
	"/dev/sdm", "/dev/sdn", "/dev/sdo", "/dev/sdp"}

// ValidDevice reports whether d is a device name Argus attaches disks as.
func ValidDevice(d string) bool { return reDevice.MatchString(d) }

// ValidImageName reports whether n can be an image's name.
func ValidImageName(n string) bool { return reImageName.MatchString(n) }

// FreeDevice is the first of Devices an instance does not use, given the
// device names it has, or "".
func FreeDevice(used []string) string {
	taken := map[string]bool{}
	for _, u := range used {
		taken[u] = true
		// /dev/xvdf and /dev/sdf are the same slot.
		if strings.HasPrefix(u, "/dev/xvd") {
			taken["/dev/sd"+strings.TrimPrefix(u, "/dev/xvd")] = true
		}
	}
	for _, d := range Devices {
		if !taken[d] {
			return d
		}
	}
	return ""
}
