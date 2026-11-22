package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
)

/*
One attempt of a Monitoring check, run by an agent near what it checks
(plans/general-layout/design.md, C6). argus-control sends one generic request,
"run this attempt, kind X, these settings", on a grant naming them, and the
agent answers with one AttemptResult. A kind the agent does not know is refused
with "unknown check kind", which argus-control turns into "this server's agent
needs updating".

The first kind run this way is snmp: a switch, router, firewall, printer, UPS
or access point, read over SNMP from a server on its network.
*/

// CheckRunService is the tunnel service name.
const CheckRunService = "checkrun"

// FrameAttemptResult carries the one AttemptResult.
const FrameAttemptResult byte = 'g'

// The kinds an agent can run.
const (
	CheckKindSNMP = "snmp"
)

// ErrUnknownCheckKind is what an agent answers for a kind it cannot run.
var ErrUnknownCheckKind = errors.New("unknown check kind")

// GrantCheck is the attempt a checkrun grant allows: its kind and that
// kind's settings.
type GrantCheck struct {
	Kind string     `json:"kind"`
	SNMP *SNMPQuery `json:"snmp,omitempty"`
}

// Validate refuses an attempt that does not name its kind's settings.
func (g GrantCheck) Validate() error {
	switch g.Kind {
	case CheckKindSNMP:
		if g.SNMP == nil {
			return errors.New("the check names no device")
		}
		return g.SNMP.Validate()
	}
	return ErrUnknownCheckKind
}

// AttemptResult is one attempt's answer.
type AttemptResult struct {
	Kind string `json:"kind"`
	// OK is whether it passed. A failure says why in Fail and Detail.
	OK     bool   `json:"ok"`
	Fail   string `json:"fail,omitempty"`
	Detail string `json:"detail,omitempty"`
	MS     int64  `json:"ms"`
	From   string `json:"from,omitempty"`
	// SNMP is what the device said about itself, when it answered.
	SNMP *SNMPAnswer `json:"snmp,omitempty"`
}

/* ── SNMP ───────────────────────────────────────────────────────────────── */

// SNMP versions a check can speak.
const (
	SNMPv2c = "v2c"
	SNMPv3  = "v3"
)

// What went wrong with a device, for argus-control to tell its own failures
// (the agent could not ask) from the device's.
const (
	SNMPFailNoAnswer  = "no_answer" // nothing came back in time
	SNMPFailRefused   = "refused"   // it answered with an error: wrong credentials, or no such view
	SNMPFailInterface = "interface" // it answered, and an interface that must be up is not
	SNMPFailBlocked   = "blocked"   // an address no check is allowed to reach
	SNMPFailAddress   = "address"   // the name did not resolve
)

const (
	// SNMPTimeoutDefault is how long one attempt waits for the device.
	SNMPTimeoutDefault = 10
	// SNMPTimeoutMax bounds what a check may ask for.
	SNMPTimeoutMax = 60
	// SNMPInterfacesMax bounds how many interfaces are read and watched.
	SNMPInterfacesMax = 512
	snmpSecretMax     = 200
)

// SNMPQuery is a device and how to read it.
type SNMPQuery struct {
	Host string `json:"host"`
	Port int    `json:"port,omitempty"`
	// Version is v3 (the default) or v2c.
	Version string `json:"version,omitempty"`
	// Community is v2c's password, sent in the clear.
	Community string `json:"community,omitempty"`
	// v3: the user, and its authentication and privacy protocols and
	// passwords. No auth protocol is noAuthNoPriv; auth without privacy is
	// authNoPriv; both is authPriv.
	User      string `json:"user,omitempty"`
	AuthProto string `json:"authProto,omitempty"` // SHA, SHA256, SHA512, MD5
	AuthPass  string `json:"authPass,omitempty"`
	PrivProto string `json:"privProto,omitempty"` // AES, AES256, DES
	PrivPass  string `json:"privPass,omitempty"`
	// TimeoutSec is how long to wait; 0 is SNMPTimeoutDefault.
	TimeoutSec int `json:"timeoutSec,omitempty"`
	// Watch are the interfaces, by index, that must be up.
	Watch []int `json:"watch,omitempty"`
}

var snmpAuthProtos = map[string]bool{"": true, "MD5": true, "SHA": true, "SHA224": true, "SHA256": true, "SHA384": true, "SHA512": true}
var snmpPrivProtos = map[string]bool{"": true, "DES": true, "AES": true, "AES192": true, "AES256": true}

// Validate refuses a query that cannot be sent.
func (q SNMPQuery) Validate() error {
	h := strings.TrimSpace(q.Host)
	if h == "" || h != q.Host || len(h) > 253 || strings.ContainsAny(h, " /@?#") {
		return errors.New("enter the device's address: an IP address or a name")
	}
	if a, err := netip.ParseAddr(h); err == nil && blockedAddr(a) {
		return errors.New("that address is not a device a check may reach")
	}
	if q.Port < 0 || q.Port > 65535 {
		return errors.New("the port is 1 to 65535")
	}
	if q.TimeoutSec < 0 || q.TimeoutSec > SNMPTimeoutMax {
		return fmt.Errorf("the timeout is at most %d seconds", SNMPTimeoutMax)
	}
	if len(q.Watch) > SNMPInterfacesMax {
		return fmt.Errorf("at most %d interfaces can be watched", SNMPInterfacesMax)
	}
	for _, s := range []string{q.Community, q.User, q.AuthPass, q.PrivPass} {
		if len(s) > snmpSecretMax {
			return errors.New("a credential is too long")
		}
	}
	switch q.Version {
	case SNMPv2c:
		if q.Community == "" {
			return errors.New("enter the community string")
		}
	case "", SNMPv3:
		if q.User == "" {
			return errors.New("enter the SNMPv3 user name")
		}
		if !snmpAuthProtos[q.AuthProto] || !snmpPrivProtos[q.PrivProto] {
			return errors.New("choose an authentication and a privacy protocol the device uses")
		}
		if q.AuthProto != "" && len(q.AuthPass) < 8 {
			return errors.New("the authentication password is at least 8 characters")
		}
		if q.PrivProto != "" && (q.AuthProto == "" || len(q.PrivPass) < 8) {
			return errors.New("privacy needs authentication too, and a password of at least 8 characters")
		}
	default:
		return errors.New("the SNMP version is v3 or v2c")
	}
	return nil
}

// SNMPAnswer is what a device said about itself.
type SNMPAnswer struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Location    string          `json:"location,omitempty"`
	UptimeSec   int64           `json:"uptimeSec"`
	Interfaces  []SNMPInterface `json:"interfaces"`
}

// SNMPInterface is one interface as the device reports it.
type SNMPInterface struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
	Alias string `json:"alias,omitempty"`
	// Up is its operational state; AdminUp whether someone turned it on.
	Up      bool `json:"up"`
	AdminUp bool `json:"adminUp"`
	// SpeedMbps is 0 when the device does not say.
	SpeedMbps uint64 `json:"speedMbps,omitempty"`
	InOctets  uint64 `json:"inOctets"`
	OutOctets uint64 `json:"outOctets"`
	InErrors  uint64 `json:"inErrors"`
	OutErrors uint64 `json:"outErrors"`
}

// SNMPVerdict says whether an answer passes: every watched interface is
// there and up.
func SNMPVerdict(q SNMPQuery, a *SNMPAnswer) (ok bool, detail string) {
	byIndex := map[int]SNMPInterface{}
	for _, i := range a.Interfaces {
		byIndex[i.Index] = i
	}
	var down []string
	for _, w := range q.Watch {
		i, there := byIndex[w]
		switch {
		case !there:
			down = append(down, fmt.Sprintf("interface %d is gone", w))
		case !i.Up:
			name := i.Name
			if i.Alias != "" {
				name += " (" + i.Alias + ")"
			}
			down = append(down, name+" is down")
		}
	}
	name := a.Name
	if name == "" {
		name = "the device"
	}
	if len(down) > 0 {
		return false, name + " answered, but " + strings.Join(down, ", ")
	}
	if len(q.Watch) == 0 {
		return true, name + " answered"
	}
	return true, fmt.Sprintf("%s answered; the %d watched interface%s up", name, len(q.Watch), map[bool]string{true: " is", false: "s are"}[len(q.Watch) == 1])
}

// snmpResolve is the device's address, refusing one no check may reach.
func snmpResolve(ctx context.Context, host string) (string, string, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		if blockedAddr(a) {
			return "", SNMPFailBlocked, errors.New("that address is not a device a check may reach")
		}
		return a.String(), "", nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addrs) == 0 {
		return "", SNMPFailAddress, fmt.Errorf("the name %s did not resolve", host)
	}
	a := addrs[0]
	if blockedAddr(a) {
		return "", SNMPFailBlocked, errors.New("that name points at an address no check may reach")
	}
	return a.Unmap().String(), "", nil
}
