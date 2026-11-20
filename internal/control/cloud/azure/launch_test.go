package azure

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/control/cloud"
)

const (
	webSubnet = "web-rg/main/web"
	webNSG    = "ffffffff-0000-4000-8000-000000000001"
)

func launchable(t *testing.T) (*Provider, *fakeMicrosoft, cloud.Account, cloud.LaunchSpec) {
	t.Helper()
	p, m, a := managed(t)
	old := newKey
	newKey = func() (string, error) { return "ssh-rsa AAAA argus-no-one-holds-this-key", nil }
	t.Cleanup(func() { newKey = old })
	return p, m, a, cloud.LaunchSpec{Region: "eastus", Group: "Web-RG", Name: "api-1", Image: "Canonical:ubuntu-24_04-lts:server",
		Size: "Standard_B2s", DiskGB: 30, Subnet: webSubnet, Zone: "eastus-2", Firewalls: []string{webNSG}, PublicIP: true,
		ExtraDisks: []int32{64}, Tags: map[string]string{"argus:launched-by": "ada@example.com"}, UserData: "#!/bin/sh\necho hi\n"}
}

func TestLaunchMakesAVMAndEverythingItNeedsInOneCall(t *testing.T) {
	p, m, a, s := launchable(t)
	if got := pathID("/subscriptions/x/resourceGroups/Web-RG/providers/Microsoft.Network/virtualNetworks/main/subnets/web"); got != webSubnet {
		t.Fatalf("the subnet's id in the inventory is %q", got)
	}
	res, err := p.Launch(context.Background(), a, "ada@example.com", s)
	if err != nil {
		t.Fatal(err)
	}
	if res.ResourceID != "aaaaaaaa-0000-4000-8000-0000000000ff" || res.RequestID != "corr-1234" {
		t.Errorf("result = %+v", res)
	}
	got := m.changed()
	if len(got) != 1 || !strings.HasPrefix(got[0], "PUT vm api-1 ") {
		t.Fatalf("calls = %v", got)
	}
	var body struct {
		Location   string            `json:"location"`
		Zones      []string          `json:"zones"`
		Tags       map[string]string `json:"tags"`
		Identity   any               `json:"identity"`
		Properties struct {
			HardwareProfile struct {
				VMSize string `json:"vmSize"`
			} `json:"hardwareProfile"`
			StorageProfile struct {
				ImageReference map[string]string `json:"imageReference"`
				OSDisk         struct {
					DiskSizeGB   int    `json:"diskSizeGB"`
					DeleteOption string `json:"deleteOption"`
				} `json:"osDisk"`
				DataDisks []struct {
					Name         string `json:"name"`
					Lun          int    `json:"lun"`
					DiskSizeGB   int    `json:"diskSizeGB"`
					DeleteOption string `json:"deleteOption"`
					CreateOption string `json:"createOption"`
				} `json:"dataDisks"`
			} `json:"storageProfile"`
			OSProfile struct {
				ComputerName  string `json:"computerName"`
				AdminUsername string `json:"adminUsername"`
				AdminPassword string `json:"adminPassword"`
				CustomData    string `json:"customData"`
				Linux         struct {
					NoPassword bool `json:"disablePasswordAuthentication"`
					SSH        struct {
						PublicKeys []struct{ Path, KeyData string } `json:"publicKeys"`
					} `json:"ssh"`
				} `json:"linuxConfiguration"`
			} `json:"osProfile"`
			NetworkProfile struct {
				NetworkAPIVersion string `json:"networkApiVersion"`
				NICs              []struct {
					Name       string `json:"name"`
					Properties struct {
						DeleteOption string `json:"deleteOption"`
						NSG          idRef  `json:"networkSecurityGroup"`
						IPConfigs    []struct {
							Properties struct {
								Subnet idRef `json:"subnet"`
								Public *struct {
									SKU struct {
										Name string `json:"name"`
									} `json:"sku"`
									Properties struct {
										DeleteOption string `json:"deleteOption"`
										Method       string `json:"publicIPAllocationMethod"`
									} `json:"properties"`
								} `json:"publicIPAddressConfiguration"`
							} `json:"properties"`
						} `json:"ipConfigurations"`
					} `json:"properties"`
				} `json:"networkInterfaceConfigurations"`
			} `json:"networkProfile"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(got[0], "PUT vm api-1 ")), &body); err != nil {
		t.Fatal(err)
	}
	pr := body.Properties
	if body.Location != "eastus" || len(body.Zones) != 1 || body.Zones[0] != "2" || body.Tags["argus:launched-by"] != "ada@example.com" || body.Identity != nil {
		t.Errorf("where and whose: %+v", body)
	}
	if pr.HardwareProfile.VMSize != "Standard_B2s" || pr.StorageProfile.ImageReference["version"] != "latest" || pr.StorageProfile.ImageReference["offer"] != "ubuntu-24_04-lts" {
		t.Errorf("size and image: %+v", pr.StorageProfile.ImageReference)
	}
	// Every disk goes with the VM.
	if pr.StorageProfile.OSDisk.DiskSizeGB != 30 || pr.StorageProfile.OSDisk.DeleteOption != "Delete" || len(pr.StorageProfile.DataDisks) != 1 {
		t.Fatalf("disks: %+v", pr.StorageProfile)
	}
	if d := pr.StorageProfile.DataDisks[0]; d.Name != "api-1-data-1" || d.DiskSizeGB != 64 || d.DeleteOption != "Delete" || d.CreateOption != "Empty" || d.Lun != 0 {
		t.Errorf("the extra disk: %+v", d)
	}
	// Nobody signs in with a password, and the key is the throwaway one.
	os := pr.OSProfile
	if os.ComputerName != "api-1" || os.AdminUsername != AdminUser || os.AdminPassword != "" || !os.Linux.NoPassword || len(os.Linux.SSH.PublicKeys) != 1 ||
		!strings.Contains(os.Linux.SSH.PublicKeys[0].KeyData, "no-one-holds-this-key") {
		t.Errorf("the account: %+v", os)
	}
	if script, _ := base64.StdEncoding.DecodeString(os.CustomData); string(script) != s.UserData {
		t.Errorf("the first-boot script: %q", script)
	}
	if len(pr.NetworkProfile.NICs) != 1 || pr.NetworkProfile.NetworkAPIVersion == "" {
		t.Fatalf("network: %+v", pr.NetworkProfile)
	}
	nic := pr.NetworkProfile.NICs[0]
	// The paths are Argus's own, built from names it checked.
	sub := "/subscriptions/" + testSub + "/resourceGroups/Web-RG/providers/Microsoft.Network/"
	if nic.Name != "api-1-nic" || nic.Properties.DeleteOption != "Delete" || nic.Properties.NSG.ID != sub+"networkSecurityGroups/web-nsg" ||
		len(nic.Properties.IPConfigs) != 1 || nic.Properties.IPConfigs[0].Properties.Subnet.ID != sub+"virtualNetworks/main/subnets/web" {
		t.Errorf("the interface: %+v", nic)
	}
	if pub := nic.Properties.IPConfigs[0].Properties.Public; pub == nil || pub.SKU.Name != "Standard" || pub.Properties.DeleteOption != "Delete" || pub.Properties.Method != "Static" {
		t.Errorf("the public address: %+v", pub)
	}
}

func TestLaunchWithoutAnAddressAZoneOrAScriptAsksForNone(t *testing.T) {
	p, m, a, s := launchable(t)
	s.PublicIP, s.Zone, s.UserData, s.Firewalls, s.ExtraDisks = false, "", "", nil, nil
	if _, err := p.Launch(context.Background(), a, "ada@example.com", s); err != nil {
		t.Fatal(err)
	}
	got := m.changed()[0]
	for _, absent := range []string{"publicIPAddressConfiguration", `"zones"`, "customData", "networkSecurityGroup"} {
		if strings.Contains(got, absent) {
			t.Errorf("the call holds %s: %s", absent, got)
		}
	}
}

func TestLaunchRefusesWhatItWouldGetWrong(t *testing.T) {
	ctx := context.Background()
	for why, change := range map[string]func(*cloud.LaunchSpec){
		"a name in use":                func(s *cloud.LaunchSpec) { s.Name = "web-1" },
		"a name Azure cannot take":     func(s *cloud.LaunchSpec) { s.Name = "Bad_Name" },
		"no resource group":            func(s *cloud.LaunchSpec) { s.Group = "" },
		"a group that is a path":       func(s *cloud.LaunchSpec) { s.Group = "a/b" },
		"an image not offered":         func(s *cloud.LaunchSpec) { s.Image = "Evil:offer:sku" },
		"an own image that is a path":  func(s *cloud.LaunchSpec) { s.Image = "image/Web-RG/../x" },
		"a size that is not one":       func(s *cloud.LaunchSpec) { s.Size = "t3.medium" },
		"another region's zone":        func(s *cloud.LaunchSpec) { s.Zone = "westeurope-1" },
		"a boot disk too small":        func(s *cloud.LaunchSpec) { s.DiskGB = 8 },
		"a subnet it has not read":     func(s *cloud.LaunchSpec) { s.Subnet = "web-rg/main/nope" },
		"a subnet in another region":   func(s *cloud.LaunchSpec) { s.Region, s.Zone = "westeurope", "" },
		"a group in another region":    func(s *cloud.LaunchSpec) { s.Firewalls = []string{"ffffffff-0000-4000-8000-000000000003"} },
		"two security groups":          func(s *cloud.LaunchSpec) { s.Firewalls = append(s.Firewalls, "ffffffff-0000-4000-8000-000000000002") },
		"a Spot VM of a B-series size": func(s *cloud.LaunchSpec) { s.Spot = true },
		"a lock":                       func(s *cloud.LaunchSpec) { s.Protect = true },
		"a tag Azure keeps":            func(s *cloud.LaunchSpec) { s.Tags = map[string]string{"microsoft-x": "y"} },
	} {
		p, m, a, s := launchable(t)
		change(&s)
		if _, err := p.Launch(ctx, a, "ada@example.com", s); err == nil {
			t.Errorf("%s was launched", why)
		}
		if got := m.changed(); len(got) != 0 {
			t.Errorf("%s reached Azure: %v", why, got)
		}
	}
	p, _, a, s := launchable(t)
	s.Protect = true
	if _, err := p.Launch(ctx, a, "ada@example.com", s); !errors.Is(err, cloud.ErrUnsupported) {
		t.Errorf("a lock: %v", err)
	}
}

// A Spot VM is asked for with no price cap, and is deallocated, not
// deleted, when Azure takes the capacity back; an ordinary launch says
// nothing of either.
func TestLaunchAsSpotKeepsTheVMWhenAzureTakesTheCapacityBack(t *testing.T) {
	p, m, a, s := launchable(t)
	if _, err := p.Launch(context.Background(), a, "ada@example.com", s); err != nil {
		t.Fatal(err)
	}
	if got := m.changed()[0]; strings.Contains(got, "priority") || strings.Contains(got, "evictionPolicy") || strings.Contains(got, "billingProfile") {
		t.Fatalf("an ordinary launch asked for Spot: %s", got)
	}
	p, m, a, s = launchable(t)
	s.Spot, s.Size = true, "Standard_D2s_v5"
	if _, err := p.Launch(context.Background(), a, "ada@example.com", s); err != nil {
		t.Fatal(err)
	}
	var body struct {
		Properties struct {
			Priority string `json:"priority"`
			Eviction string `json:"evictionPolicy"`
			Billing  struct {
				MaxPrice float64 `json:"maxPrice"`
			} `json:"billingProfile"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(m.changed()[0], "PUT vm api-1 ")), &body); err != nil {
		t.Fatal(err)
	}
	if pr := body.Properties; pr.Priority != "Spot" || pr.Eviction != "Deallocate" || pr.Billing.MaxPrice != -1 {
		t.Errorf("spot: %+v", pr)
	}
	for size, want := range map[string]bool{"Standard_B2s": false, "standard_b4ms": false, "Standard_D2s_v5": true, "Standard_F2s_v2": true} {
		if SpotSize(size) != want {
			t.Errorf("SpotSize(%s) = %v", size, !want)
		}
	}
}

func TestImagesAreTheOnesOfferedAndTheSubscriptionsOwn(t *testing.T) {
	p, _, a, s := launchable(t)
	images, err := p.Images(context.Background(), a, "ada@example.com", "eastus")
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != len(marketImages)+1 {
		t.Fatalf("images = %+v", images)
	}
	own := images[len(images)-1]
	if own.ID != "image/Web-RG/Base-Image" || !own.Linux || own.MinDiskGB != 30 || own.Family != "own" {
		t.Errorf("the subscription's image = %+v", own)
	}
	for _, im := range images {
		if _, ok := imageReference(testSub, im.ID); !ok {
			t.Errorf("%s is offered but cannot be launched", im.ID)
		}
	}
	if west, _ := p.Images(context.Background(), a, "ada@example.com", "westeurope"); len(west) != len(marketImages) {
		t.Errorf("another region's images = %+v", west)
	}
	s.Image = own.ID
	m2 := s
	if _, err := p.Launch(context.Background(), a, "ada@example.com", m2); err != nil {
		t.Fatal(err)
	}
}

func TestTheThrowawayKeyIsAnOpenSSHPublicKey(t *testing.T) {
	k, err := throwawayKey()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Fields(k)
	if len(parts) != 3 || parts[0] != "ssh-rsa" {
		t.Fatalf("key = %q", k)
	}
	blob, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil || !strings.HasPrefix(string(blob), "\x00\x00\x00\x07ssh-rsa\x00\x00\x00\x03\x01\x00\x01\x00\x00\x01\x01\x00") {
		t.Fatalf("not an RSA 2048 key in OpenSSH's form: %v", err)
	}
	if again, _ := throwawayKey(); again == k {
		t.Fatal("two launches got the same key")
	}
}
