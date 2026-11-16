package cloud

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

/*
Load balancers, target groups and Auto Scaling groups, made and changed from
Argus: the pieces that put more than one server behind one address. Every
change here is Change level and goes through the usual plan, approval and
apply path. Nothing is deleted from here yet; an unused load balancer is
already a finding in Risks & waste, removed in AWS.

  - A target group, with its health check: where a load balancer sends
    traffic, and how it tells a server is well.
  - A load balancer (application for HTTP, network for TCP) in two or more
    zones, with one listener that forwards to a target group. HTTPS needs a
    certificate, which Argus does not manage, so listeners are HTTP or TCP;
    add HTTPS in AWS.
  - An Auto Scaling group's size: minimum, maximum and how many it wants now.
    Argus does not make groups yet; scale groups (later) will.
*/

// KindScalingGroup is an AWS Auto Scaling group, read with each region.
const KindScalingGroup Kind = "scaling-group"

// Balancing actions.
const (
	ActionTargetGroupCreate Action = "target-group-create"
	ActionHealthCheck       Action = "health-check"
	ActionBalancerCreate    Action = "balancer-create"
	ActionGroupSize         Action = "group-size"
)

// IsBalancing is whether a is one of the balancing actions.
func IsBalancing(a Action) bool {
	return a == ActionTargetGroupCreate || a == ActionHealthCheck || a == ActionBalancerCreate || a == ActionGroupSize
}

// HealthCheck is how a target group tells a target is well.
type HealthCheck struct {
	// Protocol is HTTP, HTTPS or TCP.
	Protocol string `json:"protocol"`
	// Path is asked for over HTTP or HTTPS, like /health.
	Path string `json:"path,omitempty"`
	// Port is "traffic-port" (the target's own) or a number.
	Port string `json:"port,omitempty"`
	// IntervalSeconds between checks, 5 to 300.
	IntervalSeconds int32 `json:"intervalSeconds"`
	// Healthy and Unhealthy are checks in a row that change a target's
	// state, 2 to 10.
	Healthy   int32 `json:"healthy"`
	Unhealthy int32 `json:"unhealthy"`
	// Matcher is the HTTP codes that mean well, like 200 or 200-399.
	Matcher string `json:"matcher,omitempty"`
}

var (
	reHealthPath = regexp.MustCompile(`^/[A-Za-z0-9._~!$&'()*+,;=:@%/?-]*$`)
	reMatcher    = regexp.MustCompile(`^[1-5][0-9]{2}(-[1-5][0-9]{2})?(,[1-5][0-9]{2}(-[1-5][0-9]{2})?)*$`)
	reLBName     = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,30}[A-Za-z0-9])?$`)
)

// DefaultHealthCheck is AWS's own defaults, for the protocol.
func DefaultHealthCheck(protocol string) HealthCheck {
	h := HealthCheck{Protocol: protocol, Port: "traffic-port", IntervalSeconds: 30, Healthy: 5, Unhealthy: 2}
	if protocol != "TCP" {
		h.Path, h.Matcher = "/", "200"
	}
	return h
}

// Check says what is wrong with a health check, or nil. The target group's
// protocol limits what it may use: TCP groups check over TCP or HTTP(S).
func (h *HealthCheck) Check() error {
	switch h.Protocol {
	case "HTTP", "HTTPS":
		if !reHealthPath.MatchString(h.Path) || len(h.Path) > 1024 {
			return errors.New("a health check path starts with /, like /health")
		}
		if h.Matcher == "" {
			h.Matcher = "200"
		}
		if !reMatcher.MatchString(h.Matcher) {
			return errors.New("healthy codes are like 200, 200-299 or 200,301")
		}
	case "TCP":
		h.Path, h.Matcher = "", ""
	default:
		return errors.New("a health check is HTTP, HTTPS or TCP")
	}
	if h.Port == "" {
		h.Port = "traffic-port"
	}
	if h.Port != "traffic-port" {
		if p, ok := portOf(h.Port); !ok || p < 1 || p > 65535 {
			return errors.New("a health check port is traffic-port or 1 to 65535")
		}
	}
	if h.IntervalSeconds < 5 || h.IntervalSeconds > 300 {
		return errors.New("check every 5 to 300 seconds")
	}
	if h.Healthy < 2 || h.Healthy > 10 || h.Unhealthy < 2 || h.Unhealthy > 10 {
		return errors.New("2 to 10 checks in a row make a target healthy or unhealthy")
	}
	return nil
}

// Words is the health check in a sentence.
func (h HealthCheck) Words() string {
	what := h.Protocol
	if h.Path != "" {
		what += " " + h.Path
	}
	if h.Port != "" && h.Port != "traffic-port" {
		what += " on port " + h.Port
	}
	if h.Matcher != "" {
		what += " answering " + h.Matcher
	}
	return fmt.Sprintf("%s, every %ds, healthy after %d, unhealthy after %d", what, h.IntervalSeconds, h.Healthy, h.Unhealthy)
}

func portOf(s string) (int, bool) {
	n := 0
	if s == "" || len(s) > 5 {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// TargetGroupSpec is a new target group of instances.
type TargetGroupSpec struct {
	Network  string            `json:"network"`
	Name     string            `json:"name"`
	Protocol string            `json:"protocol"`
	Port     int32             `json:"port"`
	Health   HealthCheck       `json:"health"`
	Tags     map[string]string `json:"tags,omitempty"`
}

// Check says what is wrong with s, or nil.
func (s *TargetGroupSpec) Check() error {
	if !reLBName.MatchString(s.Name) {
		return errors.New("a name is 1 to 32 letters, digits and hyphens, not starting or ending with a hyphen")
	}
	if s.Network == "" {
		return errors.New("pick the network its servers are in")
	}
	switch s.Protocol {
	case "HTTP", "HTTPS", "TCP":
	default:
		return errors.New("a target group takes HTTP, HTTPS or TCP")
	}
	if s.Port < 1 || s.Port > 65535 {
		return errors.New("a port is 1 to 65535")
	}
	if s.Protocol != "TCP" && s.Health.Protocol == "TCP" {
		return errors.New("an HTTP target group checks over HTTP or HTTPS")
	}
	return s.Health.Check()
}

// BalancerSpec is a new load balancer with one listener.
type BalancerSpec struct {
	Name string `json:"name"`
	// Type is application (HTTP) or network (TCP).
	Type string `json:"type"`
	// Internal keeps it on private addresses only.
	Internal bool `json:"internal"`
	// Subnets in at least two zones.
	Subnets   []string `json:"subnets"`
	Firewalls []string `json:"firewalls,omitempty"`
	// The listener: HTTP on an application load balancer, TCP on a network
	// one, forwarding to TargetGroup.
	Port        int32             `json:"port"`
	TargetGroup string            `json:"targetGroup"`
	Tags        map[string]string `json:"tags,omitempty"`
}

// ListenerProtocol is what the listener speaks, by the balancer's type.
func (s BalancerSpec) ListenerProtocol() string {
	if s.Type == "network" {
		return "TCP"
	}
	return "HTTP"
}

// Check says what is wrong with s, or nil.
func (s *BalancerSpec) Check() error {
	if !reLBName.MatchString(s.Name) || strings.HasPrefix(strings.ToLower(s.Name), "internal-") {
		return errors.New("a name is 1 to 32 letters, digits and hyphens, not starting with internal-")
	}
	switch s.Type {
	case "application":
		if len(s.Firewalls) == 0 || len(s.Firewalls) > 5 {
			return errors.New("an application load balancer takes 1 to 5 security groups")
		}
	case "network":
	default:
		return errors.New("a load balancer is application (HTTP) or network (TCP)")
	}
	if len(s.Subnets) < 2 || len(s.Subnets) > 20 {
		return errors.New("pick subnets in at least two zones")
	}
	if s.Port < 1 || s.Port > 65535 {
		return errors.New("a listener port is 1 to 65535")
	}
	if s.TargetGroup == "" {
		return errors.New("pick the target group it forwards to")
	}
	return nil
}

// GroupSize is an Auto Scaling group's size.
type GroupSize struct {
	Min     int32 `json:"min"`
	Max     int32 `json:"max"`
	Desired int32 `json:"desired"`
}

// MaxGroupSize is the most instances Argus sets a group to.
const MaxGroupSize = 100

// Check says what is wrong with g, or nil.
func (g GroupSize) Check() error {
	if g.Min < 0 || g.Max < 1 || g.Max > MaxGroupSize || g.Min > g.Max {
		return fmt.Errorf("the minimum is 0 or more, the maximum 1 to %d and at least the minimum", MaxGroupSize)
	}
	if g.Desired < g.Min || g.Desired > g.Max {
		return errors.New("how many it wants is between the minimum and the maximum")
	}
	return nil
}

// Balancing is a provider that makes load balancers and target groups and
// sizes Auto Scaling groups. It is optional.
type Balancing interface {
	CreateTargetGroup(ctx context.Context, a Account, actor Actor, region string, s TargetGroupSpec) (Result, error)
	SetHealthCheck(ctx context.Context, a Account, actor Actor, region, targetGroup string, h HealthCheck) (Result, error)
	CreateBalancer(ctx context.Context, a Account, actor Actor, region string, s BalancerSpec) (Result, error)
	SetGroupSize(ctx context.Context, a Account, actor Actor, region, group string, g GroupSize) (Result, error)
}
