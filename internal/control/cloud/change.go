package cloud

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
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
)

// Level is how much a change can do, which decides who may ask for it and
// whether it needs a second person.
type Level string

const (
	LevelOperate Level = "operate" // start, stop, reboot, tags
	LevelChange  Level = "change"  // launch, resize, termination protection
	LevelRemove  Level = "remove"  // terminate
)

// LevelOf is the level an action needs, or "" for an unknown action.
func LevelOf(a Action) Level {
	switch a {
	case ActionStart, ActionStop, ActionReboot, ActionTags:
		return LevelOperate
	case ActionLaunch, ActionResize, ActionProtect, ActionUnprotect:
		return LevelChange
	case ActionRemove:
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
}

// LaunchSpec is one new instance.
type LaunchSpec struct {
	Region    string            `json:"region"`
	Name      string            `json:"name"`
	Image     string            `json:"image"` // an Image's ID
	Size      string            `json:"size"`  // instance type
	DiskGB    int32             `json:"diskGb"`
	Subnet    string            `json:"subnet"`
	Firewalls []string          `json:"firewalls"`
	Protect   bool              `json:"protect"`
	PublicIP  bool              `json:"publicIp"`
	Tags      map[string]string `json:"tags,omitempty"`
	// UserData runs at first boot. It holds the agent's one-time enrollment
	// token, so it is never stored or logged.
	UserData string `json:"-"`
}

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
