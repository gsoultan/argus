package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Which machines can connect to a Kafka broker on this server. Argus leaves
the broker's listeners alone: they are tied to advertised.listeners and to
the other brokers of a cluster, and changing them from here can cut clients
off. It shows them, and changes the server's firewall for the entry's
listener port: selected machines, the private network, or anywhere, where
the listener already binds past the loopback.
*/

// KafkaNetwork is which machines can connect to this Kafka broker, and why.
type KafkaNetwork struct {
	// Listeners and Advertised are the broker's, as it reports them.
	Listeners  string `json:"listeners"`
	Advertised string `json:"advertised,omitempty"`
	// Bind is what the entry's listener binds: "*" for every address.
	Bind     string `json:"bind"`
	Protocol string `json:"protocol,omitempty"`
	Port     int    `json:"port"`
	BrokerID int32  `json:"brokerId"`
	Brokers  int    `json:"brokers"`
	TLS      bool   `json:"tls"`

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

// KafkaNetworkResult answers network_set.
type KafkaNetworkResult struct {
	Network     KafkaNetwork `json:"network"`
	FirewallRan []string     `json:"firewallRan,omitempty"`
}

// kafkaListenerFor is the protocol and bind address of the listener on
// port in a listeners value (PROTO://host:port,...): "*" for every address.
func kafkaListenerFor(listeners string, port int) (proto, bind string, ok bool) {
	for _, l := range strings.Split(listeners, ",") {
		l = strings.TrimSpace(l)
		name, hp, found := strings.Cut(l, "://")
		if !found {
			continue
		}
		h, p, err := net.SplitHostPort(hp)
		if err != nil || p != strconv.Itoa(port) {
			continue
		}
		switch a, err := netip.ParseAddr(h); {
		case h == "":
			bind = "*"
		case h == "localhost":
			bind = "127.0.0.1"
		case err != nil:
			// A host name: whatever it resolves to here.
			bind = h
		case a.IsUnspecified():
			bind = "*"
		default:
			bind = a.String()
		}
		return name, bind, true
	}
	return "", "", false
}

// kafkaBrokerState reads the broker on this server's listeners as Kafka's
// administrator; the factory's netState stands in for it in tests.
func (s *kafkaSession) kafkaBrokerState(ctx context.Context) (KafkaNetwork, error) {
	if s.f.netState != nil {
		return s.f.netState(ctx)
	}
	n := KafkaNetwork{Host: s.g.Host}
	_, p, _ := net.SplitHostPort(s.addr)
	n.Port, _ = strconv.Atoi(p)
	cl, err := s.adminClient(ctx)
	if err != nil {
		return n, err
	}
	defer cl.Close()
	adm := kadm.NewClient(cl)
	md, err := adm.BrokerMetadata(ctx)
	if err != nil {
		return n, fmt.Errorf("could not read the cluster's brokers: %w", err)
	}
	n.Brokers = len(md.Brokers)
	local := map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
	if h, err := os.Hostname(); err == nil {
		local[h] = true
	}
	for _, a := range hostAddresses() {
		local[a.IP] = true
	}
	id := int32(-1)
	for _, b := range md.Brokers {
		if int(b.Port) == n.Port && (local[b.Host] || len(md.Brokers) == 1) {
			id = b.NodeID
			break
		}
	}
	if id < 0 {
		return n, errors.New("none of the cluster's brokers answers on this server's port " + strconv.Itoa(n.Port))
	}
	n.BrokerID = id
	rc, err := adm.DescribeBrokerConfigs(ctx, id)
	if err != nil {
		return n, fmt.Errorf("could not read broker %d's listeners: %w", id, err)
	}
	for _, r := range rc {
		for _, c := range r.Configs {
			if c.Value == nil {
				continue
			}
			switch c.Key {
			case "listeners":
				n.Listeners = *c.Value
			case "advertised.listeners":
				n.Advertised = *c.Value
			}
		}
	}
	return n, nil
}

func (s *kafkaSession) networkView(ctx context.Context, n KafkaNetwork) KafkaNetwork {
	proto, bind, ok := kafkaListenerFor(n.Listeners, n.Port)
	if !ok {
		bind = "*"
	}
	n.Protocol, n.Bind = proto, bind
	n.TLS = strings.Contains(proto, "SSL")
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
func (s *kafkaSession) network(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if s.db.Level != tunnel.LevelDBA {
		return errAnswer("changing which machines can connect is DBA work; this session is at %s", s.db.Level)
	}
	actx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	st, err := s.kafkaBrokerState(actx)
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
	text := tunnel.KafkaNetworkText(req)
	if (s.db.ChangesNeedApproval || req.Reach == tunnel.ReachAny) && req.Approval == "" {
		why := "on " + s.g.Host + " this change runs once someone else approves it"
		if req.Reach == tunnel.ReachAny {
			why = "opening Kafka to anywhere runs once someone else approves it"
		}
		if err := rec.refused(req.ID, text, why); err != nil {
			return tunnel.StoreAnswer{}, err
		}
		return tunnel.StoreAnswer{Type: "error", Error: why, NeedsApproval: &tunnel.RedisApprovalNeed{Text: text}}, nil
	}
	if req.Reach == tunnel.ReachAny && !strings.EqualFold(strings.TrimSpace(req.Confirm), s.g.Host) {
		return errAnswer("type the server's name, %s, to open Kafka to anywhere", s.g.Host)
	}
	if n.FirewallError != "" {
		return errAnswer("the firewall could not be read: %s", n.FirewallError)
	}
	any, lan := listenReach(n.Bind)
	switch {
	case req.Reach != tunnel.ReachLocal && !any && !lan:
		return errAnswer("Kafka's listener on port %d binds %s only, and Argus does not change Kafka's listeners; change listeners and advertised.listeners in server.properties on the server first", n.Port, n.Bind)
	case req.Reach == tunnel.ReachLocal && (any || lan):
		return errAnswer("Kafka's listener on port %d binds past this server, and Argus does not change Kafka's listeners; keep it to selected machines, or change listeners in server.properties on the server", n.Port)
	}
	if !n.FirewallActive && req.Reach != tunnel.ReachLocal {
		if req.Reach == tunnel.ReachSelected {
			return errAnswer("no firewall is on here, so Kafka cannot be limited to selected machines; turn a firewall on first")
		}
	}
	if req.Reach == tunnel.ReachSelected && n.FirewallOpenToAll {
		return errAnswer("a firewall rule Argus did not add already lets everyone reach port %d; remove it on the server first", n.Port)
	}

	// Recorded before anything runs. No record, no change.
	if err := rec.write(tunnel.PGRecord{Kind: "stmt", Request: req.ID, SQL: text, Approval: req.Approval}); err != nil {
		return tunnel.StoreAnswer{}, fmt.Errorf("the recording failed: %w", err)
	}
	start := time.Now()
	var out KafkaNetworkResult
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
