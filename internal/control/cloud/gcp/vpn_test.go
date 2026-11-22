package gcp

import (
	"testing"

	"github.com/gsoultan/argus/internal/control/cloud"
)

func TestVPNTunnelsGroupHAPair(t *testing.T) {
	gw := "https://www.googleapis.com/compute/v1/projects/p/regions/asia-southeast2/vpnGateways/ha-gw"
	peer := "https://www.googleapis.com/compute/v1/projects/p/global/externalVpnGateways/office"
	region := "https://www.googleapis.com/compute/v1/projects/p/regions/asia-southeast2"
	got := vpnTunnels([]raw{
		{Name: "t0", Region: region, Status: "ESTABLISHED", VPNGateway: gw, PeerExternalGateway: peer, PeerIP: "203.0.113.9", Router: "r"},
		{Name: "t1", Region: region, Status: "NO_INCOMING_PACKETS", VPNGateway: gw, PeerExternalGateway: peer, PeerIP: "203.0.113.10", Router: "r"},
		{Name: "old", Region: region, Status: "ESTABLISHED", TargetVPNGateway: region + "/targetVpnGateways/classic", PeerIP: "198.51.100.1",
			RemoteSelector: []string{"10.9.0.0/16"}},
	})
	if len(got) != 2 {
		t.Fatalf("%d connections", len(got))
	}
	ha, classic := got[0], got[1]
	if ha.ID != "ha-gw-to-office" || ha.State != cloud.VPNPartial || ha.Region != "asia-southeast2" ||
		ha.Details[cloud.DetailRouting] != "bgp" || ha.Details[cloud.DetailVPNType] != "HA VPN" {
		t.Fatalf("ha %+v", ha)
	}
	if classic.State != cloud.VPNUp || classic.Details[cloud.DetailRemoteNetworks] != "10.9.0.0/16" || classic.Details[cloud.DetailRouting] != "static" {
		t.Fatalf("classic %+v", classic)
	}
}
