package agent

import (
	"context"

	"github.com/gsoultan/argus/internal/tunnel"
)

// fakeRedisFirewall stands in for the server's firewall in every engine's
// network tests, on every platform.
type fakeRedisFirewall struct {
	active  bool
	sources []string
	all     bool
	others  []string
	asked   []tunnel.FirewallRequest
}

func (f *fakeRedisFirewall) do(_ context.Context, r tunnel.FirewallRequest) tunnel.FirewallResult {
	f.asked = append(f.asked, r)
	if !r.Check && f.active {
		f.sources, f.all = nil, false
		switch r.Reach {
		case tunnel.ReachSelected, tunnel.ReachNetwork:
			f.sources = r.Sources
		case tunnel.ReachAny:
			f.all = true
		}
	}
	return tunnel.FirewallResult{Firewall: "ufw", Active: f.active, Sources: f.sources, AllowAll: f.all,
		Others: f.others, OpenToAll: len(f.others) > 0}
}
