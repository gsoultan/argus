package cloud

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

/*
The AWS side of a scale group: a launch template and an Auto Scaling group
under it, both named argus-<group>.

  - Argus decides (ask each time, or automatic): the Auto Scaling group is
    the floor. Its minimum, desired and maximum are all the group's minimum
    less the server it was made from, so AWS replaces a failed copy by
    itself even when argus-control is down. Growing above the minimum is
    Argus's, as before.
  - AWS decides: the same Auto Scaling group runs between the group's
    minimum and maximum (less the source server) with a target-tracking
    policy on CloudWatch's free 5-minute processor metric, or the load
    balancer's requests per target. It keeps working when Argus is down,
    and sees nothing CloudWatch does not, so no memory.

Either way the Auto Scaling group registers its servers in the target group
and drains them from it, and their agents enroll with the group's key (see
scale_floor.go in control).
*/

// The AWS calls a floor takes, for the permissions they need. They are
// part of making or changing a scale group, not changes of their own.
const (
	ActionFloorCreate Action = "floor-create"
	ActionFloorPolicy Action = "floor-policy"
)

// ScaleAWS is the third mode: AWS Auto Scaling decides.
const ScaleAWS = "aws"

// FloorName is the launch template and Auto Scaling group under a scale
// group.
func FloorName(group string) string { return "argus-" + group }

// FloorRetry is how long Argus waits before trying again to make a floor
// AWS refused.
const FloorRetry = 30 * time.Minute

// FloorEnrollWindow is how long after AWS starts a server its agent may
// enroll with the group's key.
const FloorEnrollWindow = 30 * time.Minute

// Target-tracking metrics.
const (
	TrackCPU      = "cpu"
	TrackRequests = "requests"
)

// AWSTrack is what AWS keeps a group near when it decides.
type AWSTrack struct {
	// Metric is cpu (the group's average processor) or requests (the load
	// balancer's requests per server each minute).
	Metric string  `json:"metric"`
	Target float64 `json:"target"`
}

// DefaultAWSTrack is the processor at 60%.
func DefaultAWSTrack() AWSTrack { return AWSTrack{Metric: TrackCPU, Target: 60} }

// Check says what is wrong with t, or nil.
func (t AWSTrack) Check() error {
	switch t.Metric {
	case TrackCPU:
		if t.Target < 10 || t.Target > 90 {
			return errors.New("keep the processor near 10% to 90%")
		}
	case TrackRequests:
		if t.Target < 1 || t.Target > 1_000_000 {
			return errors.New("keep requests per server at 1 to 1,000,000 a minute")
		}
	default:
		return errors.New("AWS tracks the processor (cpu) or requests per server (requests)")
	}
	return nil
}

// Words is the policy as people read it.
func (t AWSTrack) Words() string {
	if t.Metric == TrackRequests {
		return fmt.Sprintf("about %g requests a minute per server", t.Target)
	}
	return fmt.Sprintf("the group's processor near %g%%", t.Target)
}

// FloorSpec is the launch template and Auto Scaling group to make.
type FloorSpec struct {
	// Name is the Auto Scaling group's, and Template the launch
	// template's: a new one each try, since a try AWS half refused may
	// have left one behind.
	Name      string
	Template  string
	Image     string
	Size      string
	DiskGB    int32
	Subnets   []string
	Firewalls []string
	Tags      map[string]string
	// UserData installs the agent with the group's key at first boot.
	UserData    string
	TargetGroup string
	Count       GroupSize
}

// Check says what is wrong with s, or nil.
func (s FloorSpec) Check() error {
	if s.Name == "" || s.Template == "" || s.Image == "" || !ValidSize(s.Size) {
		return errors.New("a floor needs a name, an image and a size")
	}
	if len(s.Subnets) == 0 || s.TargetGroup == "" {
		return errors.New("a floor needs subnets and a target group")
	}
	if strings.TrimSpace(s.UserData) == "" {
		return errors.New("a floor's servers need the agent's first boot")
	}
	return s.Count.Check()
}

// FloorPolicy is the target-tracking policy on a floor. Off keeps it but
// stops it acting, for a paused group or one Argus decides again.
type FloorPolicy struct {
	Track AWSTrack
	// Label is the load balancer and target group, as CloudWatch names
	// them, for requests.
	Label string
	On    bool
}

// FloorInstance is what AWS says of one server: the Auto Scaling group it
// is in ("" for none) and when it started.
type FloorInstance struct {
	Group      string
	State      string
	LaunchedAt time.Time
}

// Floors is a provider that makes launch templates and Auto Scaling groups
// for scale groups. It is optional.
type Floors interface {
	// CreateFloor makes the launch template and the Auto Scaling group.
	// The result's id is the group's name, and its detail the template's
	// id.
	CreateFloor(ctx context.Context, a Account, actor Actor, region string, s FloorSpec) (Result, error)
	// SetFloorPolicy puts the target-tracking policy on a group.
	SetFloorPolicy(ctx context.Context, a Account, actor Actor, region, group string, p FloorPolicy) (Result, error)
	// FloorInstance reads one server, to check a key's enrollment.
	FloorInstance(ctx context.Context, a Account, actor Actor, region, instance string) (FloorInstance, error)
}

// RequestLabel is CloudWatch's name for a load balancer and target group
// pair, from their ARNs: app/web-lb/50dc6c495c0c9188/targetgroup/web-tg/943f017f100becff.
// "" when either is not an application load balancer's.
func RequestLabel(balancerARN, targetGroupARN string) string {
	_, lb, ok := strings.Cut(balancerARN, ":loadbalancer/")
	_, tg, ok2 := strings.Cut(targetGroupARN, ":targetgroup/")
	if !ok || !ok2 || !strings.HasPrefix(lb, "app/") {
		return ""
	}
	return lb + "/targetgroup/" + tg
}
