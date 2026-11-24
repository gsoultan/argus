package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Which machines can connect to the NATS server on this server. Argus leaves
nats-server's own listen address as its config says (a cluster's routes and
gateways depend on it, and a reload from here could cut clients off): it
shows it, from the monitoring port, and changes the server's firewall for
the client port: selected machines, the private network, or anywhere, where
the server already listens past the loopback.
*/

// NATSNetwork is which machines can connect to this NATS server, and why.
type NATSNetwork struct {
	// Bind is what the client port listens on: "*" for every address.
	Bind    string `json:"bind"`
	Port    int    `json:"port"`
	TLS     bool   `json:"tls"`
	Cluster string `json:"cluster,omitempty"`

	Addresses []RedisAddress `json:"addresses"`
	Reach     string         `json:"reach"`
	Host      string         `json:"host"`

	Firewall          string   `json:"firewall"`
	FirewallActive    bool     `json:"firewallActive"`
	FirewallSources   []string `json:"firewallSources,omitempty"`
	FirewallAllowAll  bool     `json:"firewallAllowAll,omitempty"`
	FirewallOthers    []string `json:"firewallOthers,omitempty"`
	FirewallOpenToAll bool     `json:"firewallOpenToAll,omitempty"`
	FirewallError     string   `json:"firewallError,omitempty"`
}

// NATSNetworkResult answers network_set.
type NATSNetworkResult struct {
	Network     NATSNetwork `json:"network"`
	FirewallRan []string    `json:"firewallRan,omitempty"`
}

// natsBind is what a varz host means: "*" for every address.
func natsBind(host string) string {
	switch a, err := netip.ParseAddr(host); {
	case host == "" || host == "*":
		return "*"
	case host == "localhost":
		return "127.0.0.1"
	case err != nil:
		return host
	case a.IsUnspecified():
		return "*"
	default:
		return a.String()
	}
}

// natsListen reads where the client port listens, from the monitoring port.
func (s *natsSession) natsListen(ctx context.Context) (NATSNetwork, error) {
	n := NATSNetwork{Host: s.g.Host, Port: s.db.Port}
	if n.Port == 0 {
		n.Port = tunnel.NATSDefaultPort
	}
	b, err := s.monitorGet(ctx, "/varz", nil)
	if err != nil {
		return n, err
	}
	var v struct {
		Host       string `json:"host"`
		Port       int    `json:"port"`
		TLSRequire bool   `json:"tls_required"`
		Cluster    struct {
			Name string `json:"name"`
		} `json:"cluster"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return n, fmt.Errorf("NATS's monitoring port answered something else: %w", err)
	}
	if v.Port != 0 && v.Port != n.Port {
		return n, fmt.Errorf("the NATS server whose monitoring port answers listens on port %d, not %d", v.Port, n.Port)
	}
	n.Bind, n.TLS, n.Cluster = natsBind(v.Host), v.TLSRequire, v.Cluster.Name
	return n, nil
}

func (s *natsSession) networkView(ctx context.Context, n NATSNetwork) NATSNetwork {
	n.Addresses = hostAddresses()
	n.Firewall = firewallFound()
	for i := range n.Addresses {
		n.Addresses[i].State, n.Addresses[i].Detail = probePG(ctx, n.Addresses[i].IP, n.Port)
	}
	fw := s.f.cfg.firewall()(ctx, tunnel.FirewallRequest{Port: n.Port, Check: true})
	if fw.Error != "" {
		n.FirewallError = fw.Error
	} else {
		n.Firewall, n.FirewallActive = fw.Firewall, fw.Active
		n.FirewallSources, n.FirewallAllowAll = fw.Sources, fw.AllowAll
		n.FirewallOthers, n.FirewallOpenToAll = fw.Others, fw.OpenToAll
	}
	any, lan := listenReach(n.Bind)
	switch {
	case !any && !lan:
		n.Reach = tunnel.ReachLocal
	case n.FirewallActive && len(n.FirewallSources) > 0 && !n.FirewallAllowAll && !n.FirewallOpenToAll:
		n.Reach = tunnel.ReachSelected
	case any:
		n.Reach = tunnel.ReachAny
	default:
		n.Reach = tunnel.ReachNetwork
	}
	return n
}

// network answers network and network_set.
func (s *natsSession) network(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if s.db.Level != tunnel.LevelDBA {
		return errAnswer("changing which machines can connect is Admin work; this session is at %s", s.db.Level)
	}
	actx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	st, err := s.natsListen(actx)
	if err != nil {
		return errAnswer("%s", err)
	}
	n := s.networkView(actx, st)
	if req.Op == tunnel.RabbitOpNetwork {
		return tunnel.StoreAnswer{Type: "result", Data: n}, nil
	}

	op := tunnel.MongoNetOp(req)
	if err := tunnel.ValidatePGNetwork(&op); err != nil {
		return errAnswer("%s", err)
	}
	req.Sources = op.Sources
	text := tunnel.NATSNetworkText(req)
	if (s.db.ChangesNeedApproval || req.Reach == tunnel.ReachAny) && req.Approval == "" {
		why := "on " + s.g.Host + " this change runs once someone else approves it"
		if req.Reach == tunnel.ReachAny {
			why = "opening NATS to anywhere runs once someone else approves it"
		}
		if err := rec.refused(req.ID, text, why); err != nil {
			return tunnel.StoreAnswer{}, err
		}
		return tunnel.StoreAnswer{Type: "error", Error: why, NeedsApproval: &tunnel.RedisApprovalNeed{Text: text}}, nil
	}
	if req.Reach == tunnel.ReachAny && !strings.EqualFold(strings.TrimSpace(req.Confirm), s.g.Host) {
		return errAnswer("type the server's name, %s, to open NATS to anywhere", s.g.Host)
	}
	if n.FirewallError != "" {
		return errAnswer("the firewall could not be read: %s", n.FirewallError)
	}
	any, lan := listenReach(n.Bind)
	switch {
	case req.Reach != tunnel.ReachLocal && !any && !lan:
		return errAnswer("NATS's client port %d listens on %s only, and Argus does not change nats-server's config; set listen (or host) in nats-server.conf on the server first", n.Port, n.Bind)
	case req.Reach == tunnel.ReachLocal && (any || lan):
		return errAnswer("NATS's client port %d listens past this server, and Argus does not change nats-server's config; keep it to selected machines, or set listen in nats-server.conf on the server", n.Port)
	}
	if !n.FirewallActive && req.Reach == tunnel.ReachSelected {
		return errAnswer("no firewall is on here, so NATS cannot be limited to selected machines; turn a firewall on first")
	}
	if req.Reach == tunnel.ReachSelected && n.FirewallOpenToAll {
		return errAnswer("a firewall rule Argus did not add already lets everyone reach port %d; remove it on the server first", n.Port)
	}

	// Recorded before anything runs. No record, no change.
	if err := rec.write(tunnel.PGRecord{Kind: "stmt", Request: req.ID, SQL: text, Approval: req.Approval}); err != nil {
		return tunnel.StoreAnswer{}, fmt.Errorf("the recording failed: %w", err)
	}
	start := time.Now()
	var out NATSNetworkResult
	if n.FirewallActive {
		fw := s.f.cfg.firewall()(actx, tunnel.FirewallRequest{Port: n.Port, Reach: req.Reach, Sources: req.Sources})
		out.FirewallRan = fw.Ran
		if fw.Error != "" {
			msg := "the firewall change failed: " + fw.Error
			if rerr := rec.fail(req.ID, msg, time.Since(start).Milliseconds()); rerr != nil {
				return tunnel.StoreAnswer{}, rerr
			}
			return errAnswer("%s", msg)
		}
	}
	out.Network = s.networkView(actx, st)
	if err := rec.done(req.ID, "NETWORK", 1, time.Since(start).Milliseconds(), out, false); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	return tunnel.StoreAnswer{Type: "result", Data: out, MS: time.Since(start).Milliseconds(), Tag: "NETWORK"}, nil
}
