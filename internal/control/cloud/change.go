package cloud

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
)

/*
Changes: what a provider does when Argus asks it to change something.

The control plane decides whether a change may run, who approves it and what
is recorded; a Manager only carries it out, as the person it is told, and
says what happened in terms the audit and the console can show. Every call
takes the account's manage role, never the read role.
*/

// Action is one kind of change.
type Action string

const (
	ActionLaunch    Action = "launch"
	ActionStart     Action = "start"
	ActionStop      Action = "stop"
	ActionReboot    Action = "reboot"
	ActionResize    Action = "resize"
	ActionTags      Action = "tags"
	ActionProtect   Action = "protect"
	ActionUnprotect Action = "unprotect"
	ActionRemove    Action = "remove"
	// ActionHibernate stops an instance keeping what is in its memory, so
	// it starts where it left off. Only an instance launched with
	// hibernation on can.
	ActionHibernate Action = "hibernate"
	// ActionSettings changes an instance's settings (InstanceSettings).
	ActionSettings Action = "settings"
)

// Level is how much a change can do, which decides who may ask for it and
// whether it needs a second person.
type Level string

const (
	LevelOperate Level = "operate" // start, stop, reboot, tags, snapshot a disk
	LevelChange  Level = "change"  // launch, resize, protection, disks, images, addresses, firewalls, targets
	LevelRemove  Level = "remove"  // terminate, delete a disk, snapshot, image or firewall, release an address
)

// LevelOf is the level an action needs, or "" for an unknown action.
func LevelOf(a Action) Level {
	switch a {
	case ActionStart, ActionStop, ActionHibernate, ActionReboot, ActionTags, ActionSnapshotCreate:
		return LevelOperate
	case ActionLaunch, ActionResize, ActionProtect, ActionUnprotect, ActionSettings,
		ActionVolumeCreate, ActionVolumeGrow, ActionVolumeAttach, ActionVolumeDetach, ActionImageCreate,
		ActionAddressAllocate, ActionAddressAssociate, ActionAddressDisassociate,
		ActionFirewallCreate, ActionFirewallRules, ActionInstanceFirewalls, ActionTargetRegister, ActionTargetDeregister,
		ActionCapacityCreate, ActionCapacityCancel,
		ActionTargetGroupCreate, ActionHealthCheck, ActionBalancerCreate, ActionGroupSize,
		ActionScaleGroupCreate, ActionScaleGroupChange, ActionScaleAdd, ActionScaleRemove, ActionReplicaCreate, ActionVPNCreate,
		ActionReadEndpoint:
		return LevelChange
	case ActionRemove, ActionVolumeDelete, ActionSnapshotDelete, ActionImageDelete, ActionAddressRelease,
		ActionFirewallDelete, ActionRIBuy, ActionSPBuy, ActionReplicaRemove:
		return LevelRemove
	}
	return ""
}

// Image is a machine image a launch can start from.
type Image struct {
	// Family is the stable name the console offers: "ubuntu-24.04".
	Family string `json:"family"`
	Label  string `json:"label"`
	ID     string `json:"id"`
	Name   string `json:"name"`
	// Linux is whether the agent can enroll on it at first boot.
	Linux bool `json:"linux"`
	// MinDiskGB is the smallest disk it fits on, when more than the
	// provider's least.
	MinDiskGB int `json:"minDiskGb,omitempty"`
}

// LaunchSpec is one new instance.
type LaunchSpec struct {
	Region string `json:"region"`
	Name   string `json:"name"`
	Image  string `json:"image"` // an Image's ID
	Size   string `json:"size"`  // instance type
	DiskGB int32  `json:"diskGb"`
	Subnet string `json:"subnet"`
	// Zone is where the instance goes, where the subnet does not say: a
	// Google Cloud subnet spans its region.
	Zone string `json:"zone,omitempty"`
	// Group is the resource group an Azure VM is made in.
	Group     string            `json:"group,omitempty"`
	Firewalls []string          `json:"firewalls"`
	Protect   bool              `json:"protect"`
	PublicIP  bool              `json:"publicIp"`
	Tags      map[string]string `json:"tags,omitempty"`
	// ExtraDisks are more encrypted gp3 disks, in GB, attached as /dev/sdf
	// onwards and deleted with the instance.
	ExtraDisks []int32 `json:"extraDisks,omitempty"`
	// CapacityReservation launches into that Capacity Reservation, which
	// must have room in the subnet's zone for the size.
	CapacityReservation string `json:"capacityReservation,omitempty"`
	// Spot asks for spare capacity at the Spot price: one-time, and AWS
	// terminates it when it needs the capacity back. AWS does not allow
	// termination protection on it.
	Spot bool `json:"spot,omitempty"`
	// UserData runs at first boot. It holds the agent's one-time enrollment
	// token, so it is never stored or logged.
	UserData string `json:"-"`
}

// MaxExtraDisks is the most extra disks a launch adds.
const MaxExtraDisks = 4

// ExtraDiskDevice is the device name of the i-th extra disk.
func ExtraDiskDevice(i int) string { return "/dev/sd" + string(rune('f'+i)) }

// Result is what a change did.
type Result struct {
	// ResourceID is the resource the change made or acted on.
	ResourceID string `json:"resourceId,omitempty"`
	// RequestID is the provider's own id for the call, to find it in
	// CloudTrail.
	RequestID string `json:"requestId,omitempty"`
	// Snapshots are any taken first, by volume.
	Snapshots map[string]string `json:"snapshots,omitempty"`
	// Detail says what happened, in words.
	Detail string `json:"detail,omitempty"`
}

// Volume is a disk in a remove plan.
type Volume struct {
	ID          string `json:"id"`
	Device      string `json:"device"`
	SizeGB      int32  `json:"sizeGb"`
	Deleted     bool   `json:"deleted"` // deleted with the instance
	LastSnapAt  string `json:"lastSnapshotAt,omitempty"`
	LastSnapID  string `json:"lastSnapshotId,omitempty"`
	Description string `json:"description,omitempty"`
}

// RemovePlan is everything that goes when an instance is terminated, read
// fresh before asking and again before running.
type RemovePlan struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	State     string   `json:"state"`
	Size      string   `json:"size"`
	Protected bool     `json:"protected"`
	ManagedBy string   `json:"managedBy,omitempty"` // e.g. "CloudFormation stack web"
	Volumes   []Volume `json:"volumes"`
	// PublicIP is released with it; ElasticIP stays allocated to the account.
	PublicIP  string `json:"publicIp,omitempty"`
	ElasticIP string `json:"elasticIp,omitempty"`
	// Stays names what else the provider leaves behind at no charge: in
	// Azure, a network interface not marked to be deleted with its VM.
	Stays []string `json:"stays,omitempty"`
}

// managers are the tags AWS puts on an instance something else owns, and
// what to call that owner. Terminating such an instance fights its owner:
// a stack or group puts it back, or breaks.
var managers = []struct{ tag, what string }{
	{"aws:cloudformation:stack-name", "CloudFormation stack "},
	{"aws:autoscaling:groupName", "Auto Scaling group "},
	{"eks:nodegroup-name", "EKS node group "},
	{"karpenter.sh/nodepool", "Karpenter node pool "},
	{"aws:ec2spot:fleet-request-id", "Spot Fleet request "},
	{"aws:ec2:fleet-id", "EC2 Fleet "},
	{"elasticbeanstalk:environment-name", "Elastic Beanstalk environment "},
}

// ManagedBy names what owns an instance with these tags, or "".
func ManagedBy(tags map[string]string) string {
	for _, m := range managers {
		if v := tags[m.tag]; v != "" {
			return m.what + v
		}
	}
	return ""
}

// Hash is a digest of the plan's facts, so an approval runs only what the
// approver saw.
func (p RemovePlan) Hash() string { return Digest(p) }

// Digest is a stable digest of v as JSON.
func Digest(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Manager is a provider that can change things. Every call runs as actor,
// with the account's manage role.
type Manager interface {
	Images(ctx context.Context, a Account, actor Actor, region string) ([]Image, error)
	Launch(ctx context.Context, a Account, actor Actor, spec LaunchSpec) (Result, error)
	Power(ctx context.Context, a Account, actor Actor, region, id string, act Action) (Result, error)
	Resize(ctx context.Context, a Account, actor Actor, region, id, size string) (Result, error)
	SetTags(ctx context.Context, a Account, actor Actor, region, id string, set map[string]string, remove []string) (Result, error)
	Protect(ctx context.Context, a Account, actor Actor, region, id string, on bool) (Result, error)
	PlanRemove(ctx context.Context, a Account, actor Actor, region, id string) (RemovePlan, error)
	Remove(ctx context.Context, a Account, actor Actor, region, id string, finalSnapshots bool) (Result, error)
}

// BulkPower is a Manager that starts or stops many instances of one region
// in one call, as schedules do. The call succeeds or fails as a whole;
// callers that need to know which instance failed try them one by one.
type BulkPower interface {
	PowerMany(ctx context.Context, a Account, actor Actor, region string, ids []string, act Action) (Result, error)
}

// MaxBulkPower is the most instances one PowerMany call names.
const MaxBulkPower = 50

// InstanceSettings is what one settings change sets on an instance; a nil
// or empty field is left as it is.
type InstanceSettings struct {
	// StopProtect refuses a stop or hibernate from anyone, Argus's
	// schedules included, until it is turned off.
	StopProtect *bool `json:"stopProtect,omitempty"`
	// RequireTokens makes the metadata service answer only requests with a
	// session token (IMDSv2), which a server-side request forgery cannot get.
	RequireTokens *bool `json:"requireTokens,omitempty"`
	// CreditMode is a burstable instance's: "standard" slows it when its
	// credits run out, "unlimited" keeps it fast and bills the extra.
	CreditMode string `json:"creditMode,omitempty"`
}

// Empty is whether the settings change nothing.
func (s InstanceSettings) Empty() bool {
	return s.StopProtect == nil && s.RequireTokens == nil && s.CreditMode == ""
}

// InstanceConfig is an instance's settings as the provider reads them now.
type InstanceConfig struct {
	TerminationProtect bool `json:"terminationProtect"`
	StopProtect        bool `json:"stopProtect"`
	RequireTokens      bool `json:"requireTokens"`
	// Hibernation is whether the instance was launched able to hibernate.
	Hibernation bool `json:"hibernation"`
	// CreditMode is "" for an instance that is not burstable.
	CreditMode string `json:"creditMode"`
}

// Configurer is a Manager that reads and changes instance settings.
type Configurer interface {
	InstanceConfig(ctx context.Context, a Account, actor Actor, region, id string) (InstanceConfig, error)
	Configure(ctx context.Context, a Account, actor Actor, region, id string, s InstanceSettings) (Result, error)
}

// Consoler is a Manager that reads what an instance wrote to its serial
// console: its boot log, for a server that does not answer.
type Consoler interface {
	ConsoleOutput(ctx context.Context, a Account, actor Actor, region, id string) (ConsoleText, error)
}

// ConsoleText is an instance's latest console output and when AWS took it.
type ConsoleText struct {
	Text string     `json:"text"`
	At   *time.Time `json:"at"`
}

// Burstable is whether a size earns and spends processor credits.
func Burstable(size string) bool {
	for _, f := range []string{"t2.", "t3.", "t3a.", "t4g."} {
		if strings.HasPrefix(size, f) {
			return true
		}
	}
	return false
}

// ErrStopProtected is a stop refused because stop protection is on.
var ErrStopProtected = errors.New("stop protection is on; turn it off first, which is its own change")

// ErrProtected is an instance with termination protection on.
var ErrProtected = errors.New("termination protection is on; turn it off first, which is its own change")

var (
	reHostname = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	reTagKey   = regexp.MustCompile(`^[\w .:/=+\-@]{1,128}$`)
	reSize     = regexp.MustCompile(`^[a-z][a-z0-9-]*\.[a-z0-9]+$`)
)

// ValidHostname reports whether s can be a new server's name: what the
// instance is tagged and what its agent enrolls as.
func ValidHostname(s string) bool { return reHostname.MatchString(s) }

// ValidTagKey reports whether k can be a tag key Argus sets. Keys starting
// with "aws:" are the provider's own.
func ValidTagKey(k string) bool {
	return reTagKey.MatchString(k) && len(k) >= 1 && !(len(k) >= 4 && k[:4] == "aws:")
}

// ValidSize reports whether s looks like an instance type.
func ValidSize(s string) bool { return reSize.MatchString(s) && len(s) <= 32 }

// KindTagger is a Manager that must be told what kind a resource is to
// change its tags, because its ids are only unique within a kind.
type KindTagger interface {
	SetTagsOf(ctx context.Context, a Account, actor Actor, region string, kind Kind, id string, set map[string]string, remove []string) (Result, error)
}

// SizeLister is a Manager that lists the sizes one instance can change to,
// where they depend on where it runs.
type SizeLister interface {
	Sizes(ctx context.Context, a Account, actor Actor, region, id string) ([]string, error)
}

// ErrUnsupported is a change a provider does not make yet.
var ErrUnsupported = errors.New("not supported")

// Supporter is a Manager that makes only some changes so far. A Manager
// that is not one makes every change it has a method for.
type Supporter interface {
	Supports(a Action) bool
}

// Supports reports whether m makes change a.
func Supports(m any, a Action) bool {
	if s, ok := m.(Supporter); ok {
		return s.Supports(a)
	}
	return true
}
