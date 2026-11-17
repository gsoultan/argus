package tunnel

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

/*
Which machines can connect to a Postgres. Three things decide it, and the
agent writes all three:

  - listen_addresses: which of the server's addresses Postgres accepts
    connections on (ALTER SYSTEM; a change takes a restart);
  - pg_hba.conf: which addresses may sign in, and how. Argus appends its own
    lines, each ending "# argus", after the server's, and removes only those;
    a password is always asked (scram-sha-256, or md5 where passwords are
    still stored that way, which also takes SCRAM);
  - the server's firewall, as for every database (firewall.go).

pg_hba.conf is read top down and the first line that matches decides, so
Argus's lines, last, never override one the server's administrator wrote.
*/

// PGHBAMarker ends every pg_hba.conf line Argus writes.
const PGHBAMarker = "# argus"

// PGListenFor is listen_addresses for a change: localhost alone, every
// address, or localhost and the addresses named.
func PGListenFor(op PGAdminOp) string {
	switch op.Reach {
	case ReachAny:
		return "*"
	case ReachSelected, ReachNetwork:
		return strings.Join(append([]string{"localhost"}, op.Addresses...), ",")
	}
	return "localhost"
}

// PGHBACIDR is a source as pg_hba.conf takes it: always with a mask.
func PGHBACIDR(src string) string {
	p, err := ParseSource(src)
	if err != nil {
		return src
	}
	return p.String()
}

// PGHBALines are the lines Argus appends for a change, before the method
// is filled in.
func PGHBALines(reach string, sources []string, method string) []string {
	var out []string
	add := func(cidr string) {
		out = append(out, fmt.Sprintf("host all all %s %s %s", cidr, method, PGHBAMarker))
	}
	switch reach {
	case ReachSelected, ReachNetwork:
		for _, s := range sources {
			add(PGHBACIDR(s))
		}
	case ReachAny:
		add("0.0.0.0/0")
		add("::/0")
	}
	return out
}

// ValidatePGNetwork refuses a network.set the agent would not run, and puts
// its sources in their one written form.
func ValidatePGNetwork(op *PGAdminOp) error {
	if !ValidReach(op.Reach) {
		return fmt.Errorf("%q is not who may connect: local, selected, network or any", op.Reach)
	}
	src, err := NormalSources(op.Sources)
	if err != nil {
		return err
	}
	op.Sources = src
	switch op.Reach {
	case ReachLocal, ReachAny:
		if len(op.Addresses) > 0 || len(src) > 0 {
			return errors.New("this server only and anywhere take no addresses")
		}
	case ReachSelected, ReachNetwork:
		if len(src) == 0 {
			return errors.New("name at least one machine or network")
		}
		if len(op.Addresses) == 0 {
			return errors.New("name the server's addresses Postgres listens on")
		}
		for _, a := range op.Addresses {
			ip, err := netip.ParseAddr(a)
			if err != nil || ip.Zone() != "" || ip.IsLoopback() || ip.IsUnspecified() {
				return fmt.Errorf("%s is not one of this server's addresses", a)
			}
		}
		if op.Reach == ReachNetwork {
			for _, s := range src {
				p, _ := ParseSource(s)
				if !p.Addr().IsPrivate() {
					return fmt.Errorf("%s is not a private network", s)
				}
			}
		}
	}
	return nil
}

// PGNetworkText is a network.set as the lines an approval is given for. The
// method a pg_hba.conf line asks for is chosen on the server, so it reads
// PASSWORD here.
func PGNetworkText(op PGAdminOp) string {
	lines := []string{"ALTER SYSTEM SET listen_addresses = '" + PGListenFor(op) + "'"}
	hba := PGHBALines(op.Reach, op.Sources, "PASSWORD")
	if len(hba) == 0 {
		lines = append(lines, "PG_HBA REMOVE ARGUS LINES")
	}
	for _, l := range hba {
		lines = append(lines, "PG_HBA "+strings.TrimSuffix(l, " "+PGHBAMarker))
	}
	lines = append(lines, FirewallText(op.Reach, op.Sources))
	if op.Restart {
		lines = append(lines, "RESTART POSTGRES")
	}
	return strings.Join(lines, "\n")
}

// PGAlwaysApproval says a change waits for a second person whatever the
// database's own rules: opening it to anywhere.
func PGAlwaysApproval(op *PGAdminOp) bool {
	return op != nil && op.Kind == AdminNetworkSet && op.Reach == ReachAny && !op.Preview
}
