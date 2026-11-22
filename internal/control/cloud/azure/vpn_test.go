package azure

import (
	"encoding/json"
	"testing"

	"github.com/gsoultan/argus/internal/control/cloud"
)

func TestVPNConnectionMapsLocalGateway(t *testing.T) {
	var conn, lng raw
	if err := json.Unmarshal([]byte(`{"id":"/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/connections/office",
		"name":"office","location":"southeastasia","properties":{"provisioningState":"Succeeded","connectionType":"IPsec",
		"connectionStatus":"NotConnected","sharedKey":"secret",
		"virtualNetworkGateway1":{"id":"/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/virtualNetworkGateways/vgw"},
		"localNetworkGateway2":{"id":"/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/localNetworkGateways/Jakarta"}}}`), &conn); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"id":"/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/localNetworkGateways/Jakarta",
		"properties":{"gatewayIpAddress":"203.0.113.9","localNetworkAddressSpace":{"addressPrefixes":["192.168.10.0/24"]}}}`), &lng); err != nil {
		t.Fatal(err)
	}
	r := vpnConnection(conn, map[string]raw{"/subscriptions/s/resourcegroups/rg/providers/microsoft.network/localnetworkgateways/jakarta": lng})
	if r.State != cloud.VPNDown || r.Details[cloud.DetailRemoteAddress] != "203.0.113.9" ||
		r.Details[cloud.DetailRemoteNetworks] != "192.168.10.0/24" || r.Details[cloud.DetailGateway] != "vgw" {
		t.Fatalf("state %q details %v", r.State, r.Details)
	}
	for k, v := range r.Details {
		if v == "secret" {
			t.Fatalf("shared key copied into %s", k)
		}
	}
}

func TestVPNConnectionWithoutStatusIsUnknown(t *testing.T) {
	var conn raw
	_ = json.Unmarshal([]byte(`{"id":"/x/connections/c","name":"c","properties":{"provisioningState":"Succeeded"}}`), &conn)
	if r := vpnConnection(conn, nil); r.State != "unknown" {
		t.Fatalf("state %q", r.State)
	}
}
