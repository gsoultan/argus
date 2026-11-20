package azure

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

// web1 is web-1 as Azure answers a GET of it. RG/ stands for its resource
// group's providers path.
func web1Body(osOption, dataOption, nicOption string) string {
	return `{"properties":{"hardwareProfile":{"vmSize":"Standard_B2s"},
		"storageProfile":{"osDisk":{"name":"web-1_OsDisk","diskSizeGB":30,"deleteOption":"` + osOption + `","managedDisk":{"id":"RG/Microsoft.Compute/disks/web-1_OsDisk"}},
		 "dataDisks":[{"lun":0,"name":"web-1-data","diskSizeGB":128,"deleteOption":"` + dataOption + `","managedDisk":{"id":"RG/Microsoft.Compute/disks/web-1-data"}}]},
		"networkProfile":{"networkInterfaces":[{"id":"RG/Microsoft.Network/networkInterfaces/web-1-nic","properties":{"deleteOption":"` + nicOption + `"}}]},
		"instanceView":{"statuses":[{"code":"ProvisioningState/succeeded"},{"code":"PowerState/running"}]}}}`
}

func removable(t *testing.T) (*Provider, *fakeMicrosoft, cloud.Account) {
	t.Helper()
	p, m, a := managed(t)
	m.vmBody = web1Body("Delete", "Detach", "Detach")
	old, budget := pollEvery, snapshotBudget
	pollEvery, snapshotBudget = time.Millisecond, time.Second
	t.Cleanup(func() { pollEvery, snapshotBudget = old, budget })
	return p, m, a
}

func TestThePlanSaysWhatGoesWithAVMAndWhatStays(t *testing.T) {
	p, m, a := removable(t)
	plan, err := p.PlanRemove(context.Background(), a, "ada@example.com", "eastus", web1)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Name != "web-1" || plan.State != "running" || plan.Size != "Standard_B2s" || plan.Protected || plan.ManagedBy != "" {
		t.Fatalf("plan = %+v", plan)
	}
	if len(plan.Volumes) != 2 {
		t.Fatalf("volumes = %+v", plan.Volumes)
	}
	os, data := plan.Volumes[0], plan.Volumes[1]
	if os.ID != "bbbbbbbb-0000-4000-8000-000000000001" || !os.Deleted || os.SizeGB != 30 || os.Description != "web-1_OsDisk (boot)" {
		t.Errorf("the boot disk = %+v", os)
	}
	if data.ID != "bbbbbbbb-0000-4000-8000-000000000002" || data.Deleted || data.SizeGB != 128 || data.Device != "lun 0" {
		t.Errorf("the data disk = %+v", data)
	}
	// The address is not marked to go, so it stays and is still billed,
	// and so does the interface.
	if plan.ElasticIP != "20.1.2.3" || plan.PublicIP != "" || !slices.Equal(plan.Stays, []string{"network interface web-1-nic"}) {
		t.Errorf("address %q / %q, stays %v", plan.ElasticIP, plan.PublicIP, plan.Stays)
	}
	if len(m.changed()) != 0 {
		t.Errorf("a plan changed something: %v", m.changed())
	}
	again, _ := p.PlanRemove(context.Background(), a, "ada@example.com", "eastus", web1)
	if again.Hash() != plan.Hash() {
		t.Error("the same VM read twice gives two plans")
	}
	// Marking the data disk to go is a different plan: an approval of
	// the first does not cover it.
	m.mu.Lock()
	m.vmBody = web1Body("Delete", "Delete", "Delete")
	m.mu.Unlock()
	other, err := p.PlanRemove(context.Background(), a, "ada@example.com", "eastus", web1)
	if err != nil {
		t.Fatal(err)
	}
	if other.Hash() == plan.Hash() || !other.Volumes[1].Deleted || len(other.Stays) != 0 {
		t.Errorf("plan = %+v", other)
	}
	// The interface goes, but the address on it is not marked, so it stays.
	if other.ElasticIP != "20.1.2.3" {
		t.Errorf("address = %q / %q", other.ElasticIP, other.PublicIP)
	}
}

func TestALockedVMIsNotRemoved(t *testing.T) {
	p, m, a := removable(t)
	m.locks = []string{"keep"}
	plan, err := p.PlanRemove(context.Background(), a, "ada@example.com", "eastus", web1)
	if err != nil || !plan.Protected {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	if _, err := p.Remove(context.Background(), a, "ada@example.com", "eastus", web1, true); !errors.Is(err, ErrLocked) {
		t.Fatalf("err = %v", err)
	}
	if got := m.changed(); len(got) != 0 {
		t.Errorf("a locked VM was touched: %v", got)
	}
}

func TestAVMOfAScaleSetIsNamedAsItsOwners(t *testing.T) {
	p, m, a := removable(t)
	m.vmBody = strings.Replace(web1Body("Delete", "Detach", "Detach"), `"hardwareProfile"`, `"virtualMachineScaleSet":{"id":"RG/Microsoft.Compute/virtualMachineScaleSets/web-set"},"hardwareProfile"`, 1)
	plan, err := p.PlanRemove(context.Background(), a, "ada@example.com", "eastus", web1)
	if err != nil || plan.ManagedBy != "scale set web-set" {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
}

func TestRemoveSnapshotsTheDisksThatGoThenDeletesTheVM(t *testing.T) {
	p, m, a := removable(t)
	res, err := p.Remove(context.Background(), a, "ada@example.com", "eastus", web1, true)
	if err != nil {
		t.Fatal(err)
	}
	got := m.changed()
	if len(got) != 2 {
		t.Fatalf("calls = %v", got)
	}
	// Only the boot disk goes with the VM, so only it is snapshotted.
	if !strings.HasPrefix(got[0], "PUT Microsoft.Compute/snapshots/web-1_OsDisk-final-") || !strings.Contains(got[0], `"incremental":true`) ||
		!strings.Contains(got[0], `/Microsoft.Compute/disks/web-1_OsDisk"`) || !strings.Contains(got[0], `"argus-final-snapshot-of":"web-1"`) {
		t.Errorf("snapshot call = %s", got[0])
	}
	if got[1] != "DELETE  " {
		t.Errorf("delete call = %q", got[1])
	}
	name := res.Snapshots["bbbbbbbb-0000-4000-8000-000000000001"]
	if !strings.HasPrefix(name, "web-1_OsDisk-final-") || len(res.Snapshots) != 1 || res.RequestID != "corr-1234" {
		t.Errorf("result = %+v", res)
	}
	if !strings.Contains(res.Detail, "network interface web-1-nic") {
		t.Errorf("detail = %q", res.Detail)
	}
}

func TestRemoveWithoutSnapshotsOnlyDeletesTheVM(t *testing.T) {
	p, m, a := removable(t)
	if _, err := p.Remove(context.Background(), a, "ada@example.com", "eastus", web1, false); err != nil {
		t.Fatal(err)
	}
	if got := m.changed(); len(got) != 1 || got[0] != "DELETE  " {
		t.Errorf("calls = %v", got)
	}
}

func TestNothingIsRemovedWhenAFinalSnapshotFails(t *testing.T) {
	p, m, a := removable(t)
	m.refuse = true
	_, err := p.Remove(context.Background(), a, "ada@example.com", "eastus", web1, true)
	if err == nil || !strings.Contains(err.Error(), "nothing was removed") {
		t.Fatalf("err = %v", err)
	}
	for _, c := range m.changed() {
		if strings.HasPrefix(c, "DELETE") {
			t.Errorf("the VM was deleted after its snapshot failed: %s", c)
		}
	}
}

func TestADiskAzureNamesElsewhereStopsThePlan(t *testing.T) {
	p, m, a := removable(t)
	m.vmBody = strings.Replace(web1Body("Delete", "Detach", "Detach"), "RG/Microsoft.Compute/disks/web-1_OsDisk",
		"/subscriptions/99999999-0000-4000-8000-000000000000/resourceGroups/x/providers/Microsoft.Compute/disks/web-1_OsDisk", 1)
	if _, err := p.PlanRemove(context.Background(), a, "ada@example.com", "eastus", web1); err == nil || !strings.Contains(err.Error(), "cannot tell which disk") {
		t.Fatalf("err = %v", err)
	}
}
