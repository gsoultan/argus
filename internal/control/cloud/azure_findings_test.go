package cloud

import (
	"strings"
	"testing"
)

// An Azure disk bills by the tier its size rounds up to, by its type.
func TestAzureDiskMonthlyIsTheTiersPrice(t *testing.T) {
	for _, c := range []struct {
		sku  string
		gib  int
		want float64
		ok   bool
	}{
		{"StandardSSD_LRS", 128, 9.60, true},
		{"StandardSSD_LRS", 100, 9.60, true}, // rounds up to 128
		{"standardssd_lrs", 30, 2.40, true},
		{"Premium_LRS", 64, 10.21, true},
		{"Premium_ZRS", 64, 15.32, true},
		{"Standard_LRS", 8, 1.54, true}, // the smallest standard tier is 32
		{"StandardSSD_LRS", 32767, 2457.60, true},
		{"UltraSSD_LRS", 128, 0, false},
		{"PremiumV2_LRS", 128, 0, false},
		{"StandardSSD_LRS", 0, 0, false},
		{"", 64, 0, false},
	} {
		if got, ok := AzureDiskMonthly(c.sku, c.gib); got != c.want || ok != c.ok {
			t.Errorf("AzureDiskMonthly(%s, %d) = %v, %v, want %v, %v", c.sku, c.gib, got, ok, c.want, c.ok)
		}
	}
}

// What bills for nothing in a subscription is priced and named as
// Azure's, and the same inventory read as AWS's is not.
func TestAzureWasteIsPricedAndNamedAsAzures(t *testing.T) {
	rs := []Resource{
		{Kind: KindVolume, ID: "d1", Name: "old-data", State: "available", Size: "128 GiB", Details: map[string]string{"type": "Premium_LRS"}},
		{Kind: KindVolume, ID: "d2", Name: "ultra", State: "available", Size: "128 GiB", Details: map[string]string{"type": "UltraSSD_LRS"}},
		{Kind: KindAddress, ID: "a1", Name: "old-ip", State: "unattached"},
		{Kind: KindInstance, ID: "v1", Name: "web-1", State: "stopped", Size: "Standard_D2s_v5", Details: map[string]string{"allocated": "yes"}},
		{Kind: KindInstance, ID: "v2", Name: "web-2", State: "stopped", Size: "Standard_D2s_v5"},
		{Kind: KindInstance, ID: "v3", Name: "odd", State: "stopped", Size: "Standard_M128", Details: map[string]string{"allocated": "yes"}},
		{Kind: KindFirewall, ID: "g1", Name: "spare-nsg", Details: map[string]string{"interfaces": "0"}},
	}
	by := map[string]Finding{}
	for _, f := range FindingsIn("azure", rs, nil, now) {
		by[f.ResourceID] = f
	}
	if f := by["d1"]; f.Monthly != 19.71 || f.Fix != ActionVolumeDelete {
		t.Errorf("a premium disk: %+v", f)
	}
	if f := by["d2"]; f.Check != "idle-volume" || f.Monthly != 0 {
		t.Errorf("a disk Argus has no price for is still found, with no figure: %+v", f)
	}
	if f := by["a1"]; f.Title != "Public address attached to nothing" || f.Monthly != round2(AddressMonthly) {
		t.Errorf("an address: %+v", f)
	}
	// Shut down from inside: billed in full, and a stop from Argus ends it.
	if f := by["v1"]; f.Title != "Stopped, but still billed in full" || f.Fix != ActionStop || f.Group != GroupWaste ||
		f.Monthly != round2(0.096*HoursPerMonth) || !strings.Contains(f.Why, "keeps it allocated") {
		t.Errorf("an allocated VM: %+v", f)
	}
	if _, found := by["v2"]; found {
		t.Errorf("a deallocated VM bills nothing for itself: %+v", by["v2"])
	}
	if f := by["v3"]; f.Check != "idle-instance" || f.Monthly != 0 {
		t.Errorf("a size Argus has no price for: %+v", f)
	}
	if f := by["g1"]; f.Check != "group-unused" || f.Why != "No network interface and no subnet uses spare-nsg." {
		t.Errorf("an unused group: %+v", f)
	}

	// Read as AWS's, nothing changes from before.
	by = map[string]Finding{}
	for _, f := range Findings(rs, nil, now) {
		by[f.ResourceID] = f
	}
	if by["a1"].Title != "Elastic IP attached to nothing" || by["d1"].Monthly != round2(128*0.08) {
		t.Errorf("as AWS: %+v / %+v", by["a1"], by["d1"])
	}
	if _, found := by["v1"]; found {
		t.Error("only Azure bills a VM that is stopped")
	}
}

// A rule nothing listens behind names the cloud that keeps no record.
func TestAzureRuleNothingListensNamesAzure(t *testing.T) {
	fresh := ptr(now)
	rs := []Resource{
		{Kind: KindInstance, ID: "v1", Name: "web-1", State: "running", Links: []Ref{{Kind: KindFirewall, ID: "g1"}}},
		{Kind: KindFirewall, ID: "g1", Name: "web-nsg", Details: map[string]string{"interfaces": "1"},
			Rules: []Rule{{Inbound: true, Protocol: "tcp", FromPort: 8080, ToPort: 8080, Source: "10.0.0.0/8"}}},
	}
	facts := map[string]ServerFacts{"v1": {ServerID: "s1", SeenAt: fresh, Listening: []int{22}, ListenAt: fresh}}
	got := byCheck(FindingsIn("azure", rs, facts, now))["rule-nothing-listens"]
	if len(got) != 1 || !strings.Contains(got[0].Matters, "Azure does not record") || strings.Contains(got[0].Matters, "AWS") {
		t.Fatalf("got %+v", got)
	}
	if p, ok := HourlyPrice("Standard_B2s"); !ok || p != 0.0416 {
		t.Errorf("an Azure size has a price: %v %v", p, ok)
	}
	if _, ok := HourlyPrice("t3.small"); !ok {
		t.Error("AWS sizes keep theirs")
	}
}
