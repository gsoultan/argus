package azure

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/control/cloud"
)

const (
	web1IP   = "dddddddd-0000-4000-8000-000000000001"
	spareIP  = "dddddddd-0000-4000-8000-000000000002"
	freeEast = "dddddddd-0000-4000-8000-000000000003"
	lbIP     = "dddddddd-0000-4000-8000-000000000004"
	netPath  = "/subscriptions/" + testSub + "/resourceGroups/Web-RG/providers/Microsoft.Network/"
)

// written is the network interface as the last change wrote it.
func written(t *testing.T, m *fakeMicrosoft) map[string]any {
	t.Helper()
	got := m.changed()
	body, ok := strings.CutPrefix(got[len(got)-1], "PUT nic ")
	if !ok {
		t.Fatalf("the last change was not the interface: %q", got[len(got)-1])
	}
	var n map[string]any
	if err := json.Unmarshal([]byte(body), &n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAnAddressGoesOnAndOffAVMWithoutTouchingTheRestOfItsInterface(t *testing.T) {
	p, m, acct := managed(t)
	ctx := context.Background()
	const who = "dana@northwind.id"

	// web-1 has an address already: a second is refused before any write.
	n := len(m.changed())
	if _, err := p.AssociateAddress(ctx, acct, who, "eastus", freeEast, web1); err == nil || !strings.Contains(err.Error(), "already has the public address web-1-ip") {
		t.Fatalf("a VM with an address: %v", err)
	}
	if got := since(m, n); len(got) != 0 {
		t.Fatalf("nothing may be written: %q", got)
	}

	res, err := p.DisassociateAddress(ctx, acct, who, "eastus", web1IP)
	if err != nil || res.RequestID != "corr-1234" {
		t.Fatalf("take off: %+v %v", res, err)
	}
	nic := written(t, m)
	props := nic["properties"].(map[string]any)
	cfg := props["ipConfigurations"].([]any)[0].(map[string]any)["properties"].(map[string]any)
	if _, still := cfg["publicIPAddress"]; still {
		t.Fatalf("the address must be gone from the interface: %v", cfg)
	}
	// Everything else is written back as it was read.
	if cfg["privateIPAddress"] != "10.0.1.5" || cfg["subnet"] == nil || props["networkSecurityGroup"] == nil ||
		props["enableAcceleratedNetworking"] != true || nic["tags"].(map[string]any)["keep"] != "me" {
		t.Fatalf("the rest of the interface must be untouched: %v", nic)
	}

	if _, err := p.AssociateAddress(ctx, acct, who, "eastus", freeEast, web1); err != nil {
		t.Fatal(err)
	}
	cfg = written(t, m)["properties"].(map[string]any)["ipConfigurations"].([]any)[0].(map[string]any)["properties"].(map[string]any)
	if got := cfg["publicIPAddress"].(map[string]any)["id"]; got != netPath+"publicIPAddresses/free-east" {
		t.Fatalf("the interface must point at the address by a path Argus built: %v", got)
	}
}

func TestAnInterfaceSomeoneElseChangedIsNotOverwritten(t *testing.T) {
	p, m, acct := managed(t)
	m.mu.Lock()
	m.raceNIC = true
	m.mu.Unlock()
	_, err := p.DisassociateAddress(context.Background(), acct, "dana@northwind.id", "eastus", web1IP)
	if err == nil || !strings.Contains(err.Error(), "someone else changed it") {
		t.Fatalf("a changed interface: %v", err)
	}
	if got := m.changed(); len(got) != 0 {
		t.Fatalf("Azure must have refused the write: %q", got)
	}
}

func TestAddressesAreMadeAndDeletedAndRefusals(t *testing.T) {
	p, m, acct := managed(t)
	ctx := context.Background()
	const who = "dana@northwind.id"

	n := len(m.changed())
	res, err := p.AllocateAddress(ctx, acct, who, "eastus", AddressRef("Web-RG", "api-ip"))
	if err != nil || res.ResourceID == "" || !strings.Contains(res.Detail, "Made api-ip in Web-RG") {
		t.Fatalf("make: %+v %v", res, err)
	}
	want := `PUT Microsoft.Network/publicIPAddresses/api-ip {"location":"eastus","properties":{"publicIPAddressVersion":"IPv4","publicIPAllocationMethod":"Static"},"sku":{"name":"Standard"}}`
	if got := since(m, n); !slices.Equal(got, []string{want}) {
		t.Fatalf("make sent\n%q\nwant\n%q", got, want)
	}

	n = len(m.changed())
	for what, err := range map[string]error{
		"a name in use":                      second(p.AllocateAddress(ctx, acct, who, "eastus", AddressRef("Web-RG", "api-ip"))),
		"no resource group":                  second(p.AllocateAddress(ctx, acct, who, "eastus", "api-2")),
		"a group with a path in it":          second(p.AllocateAddress(ctx, acct, who, "eastus", "a/b/c")),
		"a region that is a path":            second(p.AllocateAddress(ctx, acct, who, "east/us", AddressRef("Web-RG", "api-2"))),
		"deleting an address in use":         second(p.ReleaseAddress(ctx, acct, who, "eastus", web1IP)),
		"deleting a load balancer's address": second(p.ReleaseAddress(ctx, acct, who, "eastus", lbIP)),
		"attaching an address in use":        second(p.AssociateAddress(ctx, acct, who, "eastus", web1IP, web1)),
		"taking off a free address":          second(p.DisassociateAddress(ctx, acct, who, "eastus", freeEast)),
		"an address in another region":       second(p.ReleaseAddress(ctx, acct, who, "eastus", spareIP)),
		"a disk's id for an address":         second(p.ReleaseAddress(ctx, acct, who, "eastus", leftOver)),
		"a path for an id":                   second(p.ReleaseAddress(ctx, acct, who, "eastus", "../networkInterfaces/web-1-nic")),
	} {
		if err == nil {
			t.Errorf("%s must be refused", what)
		}
	}
	// A load balancer's address is not Argus's to take off.
	if _, err := p.DisassociateAddress(ctx, acct, who, "eastus", lbIP); !errors.Is(err, cloud.ErrUnsupported) {
		t.Fatalf("a load balancer's address: %v", err)
	}
	if got := since(m, n); len(got) != 0 {
		t.Fatalf("a refused change must ask Azure for nothing: %q", got)
	}

	if _, err := p.ReleaseAddress(ctx, acct, who, "eastus", freeEast); err != nil {
		t.Fatal(err)
	}
	if _, err := p.SetTagsOf(ctx, acct, who, "westeurope", cloud.KindAddress, spareIP, map[string]string{"Owner": "dana"}, nil); err != nil {
		t.Fatal(err)
	}
	wantAll := []string{
		"DELETE Microsoft.Network/publicIPAddresses/free-east ",
		`PATCH Microsoft.Network/publicIPAddresses/spare/providers/Microsoft.Resources/tags/default {"operation":"Merge","properties":{"tags":{"Owner":"dana"}}}`,
	}
	if got := since(m, n); !slices.Equal(got, wantAll) {
		t.Fatalf("delete and tag sent\n%q\nwant\n%q", got, wantAll)
	}

	// With only Reader, nothing.
	p, m, acct = newFake(t, "argus-0123abcd", false)
	acct.RoleARN = RoleID(testSub, ManageRoleID(testSub))
	if _, err := p.DisassociateAddress(ctx, acct, who, "eastus", web1IP); err == nil {
		t.Fatal("a change without the manage assignment must be refused")
	}
	if got := m.changed(); len(got) != 0 {
		t.Fatalf("nothing may be asked of Azure: %q", got)
	}
}

func TestOnlyAnInterfaceInThisSubscriptionIsAddressed(t *testing.T) {
	ok := "/subscriptions/" + testSub + "/resourceGroups/Web-RG/providers/Microsoft.Network/networkInterfaces/web-1-nic"
	for arm, want := range map[string]string{
		ok:                                 ok,
		ok + "/ipConfigurations/ipconfig1": ok,
		strings.ToUpper(ok[:15]) + ok[15:]: ok,
		"/subscriptions/" + otherSub + "/resourceGroups/Web-RG/providers/Microsoft.Network/networkInterfaces/web-1-nic":                "",
		"/subscriptions/" + testSub + "/resourceGroups/Web-RG/providers/Microsoft.Network/loadBalancers/lb/frontendIPConfigurations/f": "",
		"/subscriptions/" + testSub + "/resourceGroups/a b/providers/Microsoft.Network/networkInterfaces/n":                            "",
		"/subscriptions/" + testSub + "/resourceGroups/Web-RG/providers/Microsoft.Network/networkInterfaces/..":                        "",
		"": "",
	} {
		got, valid := nicPath(testSub, arm)
		if got != want || valid != (want != "") {
			t.Errorf("nicPath(%q) = %q, %v; want %q", arm, got, valid, want)
		}
	}
}
