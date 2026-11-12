// Package cloud is the provider-neutral layer under Cloud in the console.
//
// A Provider is one cloud (AWS today; GCP, Azure, DigitalOcean and Hetzner
// later). It knows nothing about Argus people, rules or the audit chain: the
// control plane decides whether a call may run, says who it runs for, and
// records it. A provider only turns a neutral request into its own API calls
// and its answers back into neutral resources, so a second cloud needs no
// console change to appear in the lists.
//
// Control-plane only. The agent links none of this.
package cloud

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
)

// Kind is a neutral resource kind.
type Kind string

const (
	KindInstance Kind = "instance"
	KindVolume   Kind = "volume"
	KindFirewall Kind = "firewall" // AWS security group
	KindNetwork  Kind = "network"  // AWS VPC
	KindSubnet   Kind = "subnet"
	KindAddress  Kind = "address" // AWS Elastic IP
)

// Kinds is every kind the inventory reads, in the order the console lists
// them.
var Kinds = []Kind{KindInstance, KindVolume, KindFirewall, KindNetwork, KindSubnet, KindAddress, KindSnapshot, KindImage,
	KindLoadBalancer, KindTargetGroup}

// ValidKind reports whether k is one the inventory knows.
func ValidKind(k Kind) bool {
	for _, x := range Kinds {
		if x == k {
			return true
		}
	}
	return false
}

// Account is a connected cloud account as a provider needs it: where it is
// and how to get in. The external id is derived by the control plane, never
// typed or stored.
type Account struct {
	Provider   string
	AccountID  string
	RoleARN    string
	ExternalID string
}

// Actor is who a call runs for. Every role session is opened with it as the
// source identity, so the cloud's own log names the same person Argus does.
// Background reads use SyncActor.
type Actor string

// SyncActor is the inventory sync's own identity.
const SyncActor Actor = "argus-sync"

var reActor = regexp.MustCompile(`^[\w+=,.@-]{2,64}$`)

// Valid reports whether a can be a source identity (AWS's rule, which is
// the strictest of the providers planned).
func (a Actor) Valid() bool { return reActor.MatchString(string(a)) }

// Identity is what Verify found: who Argus is inside the account, and the
// regions it may read.
type Identity struct {
	ARN       string   `json:"arn"`
	AccountID string   `json:"accountId"`
	Regions   []string `json:"regions"`
}

// Base is the identity this control plane uses to reach any account, before
// assuming the account's role.
type Base struct {
	// Principal is what an account's trust policy must name.
	Principal string `json:"principal"`
	// ARN is what the provider answered, which may be a session of
	// Principal.
	ARN string `json:"arn"`
	// Source says where the credentials came from, in words.
	Source string `json:"source"`
}

// Ref names another resource this one uses or is attached to.
type Ref struct {
	Kind Kind   `json:"kind"`
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// Rule is one firewall rule.
type Rule struct {
	Inbound  bool   `json:"inbound"`
	Protocol string `json:"protocol"` // "tcp", "udp", "icmp", "all"
	FromPort int32  `json:"fromPort"`
	ToPort   int32  `json:"toPort"`
	// Source is a CIDR or another firewall's id.
	Source      string `json:"source"`
	Description string `json:"description,omitempty"`
}

// Open reports whether the rule lets the whole internet in.
func (r Rule) Open() bool {
	return r.Inbound && (r.Source == "0.0.0.0/0" || r.Source == "::/0")
}

// Covers reports whether the rule admits port.
func (r Rule) Covers(port int32) bool {
	if r.Protocol == "all" {
		return true
	}
	return r.Protocol == "tcp" && r.FromPort <= port && port <= r.ToPort
}

// Resource is one thing in a cloud, in neutral terms. Details holds what is
// specific to the provider, shown as-is on a details tab.
type Resource struct {
	Kind      Kind              `json:"kind"`
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Region    string            `json:"region"`
	Zone      string            `json:"zone,omitempty"`
	State     string            `json:"state,omitempty"`
	Size      string            `json:"size,omitempty"`
	CreatedAt *time.Time        `json:"createdAt,omitempty"`
	Addresses []string          `json:"addresses,omitempty"`
	PublicIP  string            `json:"publicIp,omitempty"`
	Network   string            `json:"network,omitempty"`
	Links     []Ref             `json:"links,omitempty"`
	Rules     []Rule            `json:"rules,omitempty"`
	Tags      map[string]string `json:"tags,omitempty"`
	Details   map[string]string `json:"details,omitempty"`
}

// Snapshot is one region's inventory of the kinds asked for, read at one
// time.
type Snapshot struct {
	Region    string
	Kinds     []Kind
	Resources []Resource
	ReadAt    time.Time
}

// Provider is one cloud.
type Provider interface {
	// Kind is the provider's name: "aws".
	Kind() string
	// Base says who this control plane is to the provider, or why it has
	// no identity.
	Base(ctx context.Context) (Base, error)
	// Verify assumes the account's role as actor and reports what it found.
	Verify(ctx context.Context, a Account, actor Actor) (Identity, error)
	// Inventory reads one region.
	Inventory(ctx context.Context, a Account, region string, kinds []Kind) (Snapshot, error)
}

// ErrNoBase is a provider with no identity on this control host.
var ErrNoBase = errors.New("this control plane has no identity for the provider")

// Name picks a display name from a resource's tags, falling back to its id.
func Name(tags map[string]string, id string) string {
	if n := tags["Name"]; n != "" {
		return n
	}
	return id
}

// StackState is where the stack that makes Argus's roles in an account
// stands, as the provider reports it.
type StackState struct {
	// Status is the provider's word, for example CREATE_IN_PROGRESS; empty
	// when there is no stack.
	Status string `json:"status,omitempty"`
	// Reason is why it failed, when it did.
	Reason string `json:"reason,omitempty"`
	// Done is a stack whose roles are made; Failed one that will not make
	// them without someone looking.
	Done   bool `json:"done"`
	Failed bool `json:"failed"`
}

// Deployer makes the roles in an account with the control plane's own
// identity, so nobody has to open the provider's console. It is possible
// only in the account that identity belongs to, and only when someone gave
// that identity the rights to make roles; elsewhere the person makes them.
// It is an optional interface: a provider that cannot does not implement it.
type Deployer interface {
	// DeployAccess creates the access stack from template, or updates it
	// when it is already there, tagged with the person who asked.
	DeployAccess(ctx context.Context, accountID, template string, actor Actor) (StackState, error)
	// AccessStack reports the access stack.
	AccessStack(ctx context.Context, accountID string) (StackState, error)
}

// ErrNotHomeAccount is a deploy asked for in an account the control
// plane's own identity is not in.
var ErrNotHomeAccount = errors.New("argus-control's own identity is in another account")

// HomeAccount is the account an identity ARN belongs to, or "".
func HomeAccount(arn string) string {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" {
		return ""
	}
	return parts[4]
}
