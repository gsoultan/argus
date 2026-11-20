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

const (
	osDisk    = "bbbbbbbb-0000-4000-8000-000000000001"
	dataDisk  = "bbbbbbbb-0000-4000-8000-000000000002"
	leftOver  = "bbbbbbbb-0000-4000-8000-000000000003"
	nightly   = "cccccccc-0000-4000-8000-000000000001"
	groupPath = "/subscriptions/" + testSub + "/resourceGroups/Web-RG/providers/Microsoft.Compute/"
)

func since(m *fakeMicrosoft, n int) []string { return m.changed()[n:] }

func TestDisksGrowAttachDetachAndDelete(t *testing.T) {
	p, m, acct := managed(t)
	ctx := context.Background()
	const who = "dana@northwind.id"

	n := len(m.changed())
	res, err := p.ModifyVolume(ctx, acct, who, "eastus", leftOver, cloud.VolumeChange{SizeGB: 256})
	if err != nil || res.RequestID != "corr-1234" || res.ResourceID != leftOver {
		t.Fatalf("grow: %+v %v", res, err)
	}
	if got := since(m, n); !slices.Equal(got, []string{`PATCH Microsoft.Compute/disks/left-over {"properties":{"diskSizeGB":256}}`}) {
		t.Fatalf("grow sent %q", got)
	}
	// A disk only grows, and Argus leaves its type to the portal.
	n = len(m.changed())
	for why, c := range map[string]cloud.VolumeChange{"smaller": {SizeGB: 100}, "the same": {SizeGB: 200}, "a type": {SizeGB: 300, Type: "Premium_LRS"}, "too big": {SizeGB: MaxDiskGB + 1}} {
		if _, err := p.ModifyVolume(ctx, acct, who, "eastus", leftOver, c); err == nil {
			t.Errorf("a grow to %s must be refused", why)
		}
	}
	if got := since(m, n); len(got) != 0 {
		t.Fatalf("a refused grow must ask Azure for nothing: %q", got)
	}

	// Attach and detach name the one disk; Azure adds or removes it.
	n = len(m.changed())
	if _, err := p.AttachVolume(ctx, acct, who, "eastus", leftOver, web1, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DetachVolume(ctx, acct, who, "eastus", dataDisk); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`POST /attachDetachDataDisks {"dataDisksToAttach":[{"diskId":"` + groupPath + `disks/left-over"}]}`,
		`POST /attachDetachDataDisks {"dataDisksToDetach":[{"diskId":"` + groupPath + `disks/web-1-data"}]}`,
	}
	if got := since(m, n); !slices.Equal(got, want) {
		t.Fatalf("attach and detach sent\n%q\nwant\n%q", got, want)
	}

	n = len(m.changed())
	for what, err := range map[string]error{
		"attaching a disk in use":             second(p.AttachVolume(ctx, acct, who, "eastus", dataDisk, web1, "")),
		"detaching a disk nothing uses":       second(p.DetachVolume(ctx, acct, who, "eastus", leftOver)),
		"detaching the disk a VM starts from": second(p.DetachVolume(ctx, acct, who, "eastus", osDisk)),
		"deleting a disk in use":              second(p.DeleteVolume(ctx, acct, who, "eastus", dataDisk, false)),
		"a final snapshot":                    second(p.DeleteVolume(ctx, acct, who, "eastus", leftOver, true)),
		"a disk in another region":            second(p.DeleteVolume(ctx, acct, who, "westeurope", leftOver, false)),
		"a disk that is not there":            second(p.DeleteVolume(ctx, acct, who, "eastus", "bbbbbbbb-0000-4000-8000-0000000000ff", false)),
		"a path for an id":                    second(p.DeleteVolume(ctx, acct, who, "eastus", "../virtualMachines/web-1", false)),
		"a snapshot's id for a disk":          second(p.DeleteVolume(ctx, acct, who, "eastus", nightly, false)),
	} {
		if err == nil {
			t.Errorf("%s must be refused", what)
		}
	}
	if got := since(m, n); len(got) != 0 {
		t.Fatalf("a refused change must ask Azure for nothing: %q", got)
	}

	if _, err := p.DeleteVolume(ctx, acct, who, "eastus", leftOver, false); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DeleteSnapshot(ctx, acct, who, "eastus", nightly); err != nil {
		t.Fatal(err)
	}
	if got := since(m, n); !slices.Equal(got, []string{"DELETE Microsoft.Compute/disks/left-over ", "DELETE Microsoft.Compute/snapshots/nightly "}) {
		t.Fatalf("delete sent %q", got)
	}
}

func second(_ cloud.Result, err error) error { return err }

func TestSnapshotsAndNewDisksNeverOverwrite(t *testing.T) {
	p, m, acct := managed(t)
	ctx := context.Background()
	const who = "dana@northwind.id"
	pollEvery = time.Millisecond
	t.Cleanup(func() { pollEvery = 2 * time.Second })

	n := len(m.changed())
	res, err := p.CreateSnapshot(ctx, acct, who, "eastus", dataDisk, "web-1-data-20261120-0900")
	if err != nil || res.ResourceID != "web-1-data-20261120-0900" {
		t.Fatalf("snapshot: %+v %v", res, err)
	}
	want := `PUT Microsoft.Compute/snapshots/web-1-data-20261120-0900 {"location":"eastus","properties":{"creationData":{"createOption":"Copy","sourceResourceId":"` +
		groupPath + `disks/web-1-data"},"incremental":true}}`
	if got := since(m, n); !slices.Equal(got, []string{want}) {
		t.Fatalf("snapshot sent\n%q\nwant\n%q", got, want)
	}
	// The name is in use now: a second PUT would change what is there.
	n = len(m.changed())
	if _, err := p.CreateSnapshot(ctx, acct, who, "eastus", dataDisk, "web-1-data-20261120-0900"); err == nil || !strings.Contains(err.Error(), "already has a snapshot") {
		t.Fatalf("a name in use: %v", err)
	}
	if _, err := p.CreateSnapshot(ctx, acct, who, "eastus", dataDisk, "bad/name"); err == nil {
		t.Fatal("a name with a slash must be refused")
	}
	if got := since(m, n); len(got) != 0 {
		t.Fatalf("a refused snapshot must ask Azure for nothing: %q", got)
	}

	// A new disk from a snapshot, in the VM's zone, attached once ready.
	n = len(m.changed())
	spec := cloud.VolumeSpec{Name: "restore-1", Group: "Web-RG", Zone: "eastus-2", SizeGB: 64, Type: "StandardSSD_LRS", Snapshot: nightly,
		AttachTo: web1, Tags: map[string]string{"argus:change": "ch_1"}}
	res, err = p.CreateVolume(ctx, acct, who, "eastus", spec)
	if err != nil {
		t.Fatal(err)
	}
	if res.ResourceID != "bbbbbbbb-0000-4000-8000-000000000009" || !strings.Contains(res.Detail, "attached to web-1") {
		t.Fatalf("the new disk is known by Azure's id for it: %+v", res)
	}
	wantAll := []string{
		`PUT Microsoft.Compute/disks/restore-1 {"location":"eastus","properties":{"creationData":{"createOption":"Copy","sourceResourceId":"` + groupPath +
			`snapshots/nightly"},"diskSizeGB":64},"sku":{"name":"StandardSSD_LRS"},"tags":{"argus:change":"ch_1"},"zones":["2"]}`,
		`POST /attachDetachDataDisks {"dataDisksToAttach":[{"diskId":"` + groupPath + `disks/restore-1"}]}`,
	}
	if got := since(m, n); !slices.Equal(got, wantAll) {
		t.Fatalf("new disk sent\n%q\nwant\n%q", got, wantAll)
	}

	n = len(m.changed())
	for why, s := range map[string]cloud.VolumeSpec{
		"a name in use":        spec,
		"no group":             {Name: "d2", SizeGB: 8, Type: "Premium_LRS"},
		"a group with a path":  {Name: "d2", Group: "a/b", SizeGB: 8, Type: "Premium_LRS"},
		"another cloud's type": {Name: "d2", Group: "Web-RG", SizeGB: 8, Type: "gp3"},
		"no size":              {Name: "d2", Group: "Web-RG", Type: "Premium_LRS"},
		"a zone elsewhere":     {Name: "d2", Group: "Web-RG", SizeGB: 8, Type: "Premium_LRS", Zone: "westeurope-1"},
		"a reserved tag":       {Name: "d2", Group: "Web-RG", SizeGB: 8, Type: "Premium_LRS", Tags: map[string]string{"azure-x": "1"}},
		"a snapshot not there": {Name: "d2", Group: "Web-RG", SizeGB: 8, Type: "Premium_LRS", Snapshot: leftOver},
	} {
		if _, err := p.CreateVolume(ctx, acct, who, "eastus", s); err == nil {
			t.Errorf("a disk with %s must be refused", why)
		}
	}
	if got := since(m, n); len(got) != 0 {
		t.Fatalf("a refused disk must ask Azure for nothing: %q", got)
	}

	// An empty disk without a zone, left unattached, is one call.
	n = len(m.changed())
	if res, err = p.CreateVolume(ctx, acct, who, "eastus", cloud.VolumeSpec{Name: "scratch", Group: "Web-RG", SizeGB: 8, Type: "Standard_LRS"}); err != nil || res.ResourceID == "" {
		t.Fatalf("empty disk: %+v %v", res, err)
	}
	if got := since(m, n); !slices.Equal(got, []string{`PUT Microsoft.Compute/disks/scratch {"location":"eastus","properties":{"creationData":{"createOption":"Empty"},"diskSizeGB":8},"sku":{"name":"Standard_LRS"}}`}) {
		t.Fatalf("empty disk sent %q", got)
	}
}

func TestDiskChangesNeedTheManageAssignmentAndPassAzuresRefusalOn(t *testing.T) {
	p, m, acct := managed(t)
	ctx := context.Background()
	m.mu.Lock()
	m.refuse = true
	m.mu.Unlock()
	if _, err := p.DeleteVolume(ctx, acct, "dana@northwind.id", "eastus", leftOver, false); err == nil || !strings.Contains(err.Error(), "authorization") {
		t.Fatalf("Azure's refusal must reach the person: %v", err)
	}
	// With only Reader there is no change at all.
	p, m, acct = newFake(t, "argus-0123abcd", false)
	acct.RoleARN = RoleID(testSub, ManageRoleID(testSub))
	if _, err := p.DeleteVolume(ctx, acct, "dana@northwind.id", "eastus", leftOver, false); err == nil {
		t.Fatal("a delete without the manage assignment must be refused")
	}
	if _, err := p.CreateSnapshot(ctx, acct, "dana@northwind.id", "eastus", dataDisk, "s1"); err == nil {
		t.Fatal("a snapshot without the manage assignment must be refused")
	}
	if got := m.changed(); len(got) != 0 {
		t.Fatalf("nothing may be asked of Azure: %q", got)
	}
	for _, f := range []func() (cloud.Result, error){
		func() (cloud.Result, error) { return p.CreateImage(ctx, acct, "x", "eastus", web1, "img", false) },
		func() (cloud.Result, error) { return p.DeleteImage(ctx, acct, "x", "eastus", "img", false) },
	} {
		if _, err := f(); !errors.Is(err, cloud.ErrUnsupported) {
			t.Fatalf("images are not made in Azure yet: %v", err)
		}
	}
}

func TestTagsOnADiskAndASnapshot(t *testing.T) {
	p, m, acct := managed(t)
	ctx := context.Background()
	n := len(m.changed())
	if _, err := p.SetTagsOf(ctx, acct, "dana@northwind.id", "eastus", cloud.KindVolume, leftOver, map[string]string{"Owner": "dana"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := p.SetTagsOf(ctx, acct, "dana@northwind.id", "eastus", cloud.KindSnapshot, nightly, map[string]string{"keep": "30d"}, nil); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`PATCH Microsoft.Compute/disks/left-over/providers/Microsoft.Resources/tags/default {"operation":"Merge","properties":{"tags":{"Owner":"dana"}}}`,
		`PATCH Microsoft.Compute/snapshots/nightly/providers/Microsoft.Resources/tags/default {"operation":"Merge","properties":{"tags":{"keep":"30d"}}}`,
	}
	if got := since(m, n); !slices.Equal(got, want) {
		t.Fatalf("tags sent\n%q\nwant\n%q", got, want)
	}
	if _, err := p.SetTagsOf(ctx, acct, "dana@northwind.id", "eastus", cloud.KindFirewall, leftOver, map[string]string{"a": "b"}, nil); !errors.Is(err, cloud.ErrUnsupported) {
		t.Fatalf("a security group is not tagged yet: %v", err)
	}
}

func TestAnOlderManageRoleSaysWhatItLacks(t *testing.T) {
	p, m, acct := managed(t)
	ctx := context.Background()
	read := acct
	read.RoleARN = RoleID(testSub, ReaderRoleID)
	manage := RoleID(testSub, ManageRoleID(testSub))

	// The role as Part 2's command made it: VMs and tags only.
	m.mu.Lock()
	m.roleActions = slices.Clone(ManageActions[:9])
	m.mu.Unlock()
	missing, err := p.ManageMissing(ctx, read, manage)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(missing, "Microsoft.Compute/disks/delete") || slices.Contains(missing, "Microsoft.Compute/virtualMachines/write") {
		t.Fatalf("missing: %v", missing)
	}
	for _, a := range []cloud.Action{cloud.ActionVolumeCreate, cloud.ActionVolumeGrow, cloud.ActionVolumeAttach, cloud.ActionVolumeDetach,
		cloud.ActionVolumeDelete, cloud.ActionSnapshotCreate, cloud.ActionSnapshotDelete} {
		needs := p.Needs(a)
		if len(needs) == 0 || !slices.ContainsFunc(needs, func(n string) bool { return slices.Contains(missing, n) }) {
			t.Errorf("%s must be held back on the older role: needs %v", a, needs)
		}
		for _, n := range needs {
			if !slices.Contains(ManageActions, n) {
				t.Errorf("%s needs %s, which the role the command makes does not allow", a, n)
			}
		}
	}
	if len(p.Needs(cloud.ActionStop)) != 0 {
		t.Fatal("what the first role allowed is never held back")
	}
	// Run again, the command brings the role up to date. Azure's own
	// spelling of an action's case does not matter.
	m.mu.Lock()
	m.roleActions = nil
	for _, a := range ManageActions {
		m.roleActions = append(m.roleActions, strings.ToUpper(a))
	}
	m.mu.Unlock()
	if missing, err = p.ManageMissing(ctx, read, manage); err != nil || len(missing) != 0 {
		t.Fatalf("an up to date role lacks nothing: %v %v", missing, err)
	}
	if _, err := p.ManageMissing(ctx, read, RoleID(testSub, ReaderRoleID)); err == nil {
		t.Fatal("only this subscription's manage role is read")
	}
	// The setup command writes every action the role is checked for.
	cmd := SetupCommandFor(testSub, []string{clientID}, "argus-0123abcd", true)
	for _, a := range ManageActions {
		if !strings.Contains(cmd, a) {
			t.Errorf("the command does not give %s", a)
		}
	}
}

func TestDiskNamesGroupsAndZones(t *testing.T) {
	for n, ok := range map[string]bool{"data-1": true, "web-1_OsDisk_1_abc": true, "a": true, "9lives": true, "ends_": true,
		"": false, "-lead": false, "ends-": false, "ends.": false, "a/b": false, "a b": false, strings.Repeat("a", 81): false} {
		if ValidDiskName(n) != ok {
			t.Errorf("ValidDiskName(%q) = %v", n, !ok)
		}
	}
	for g, ok := range map[string]bool{"Web-RG": true, "rg_(prod).1": true, "": false, "a/b": false, "ends.": false, "a b": false} {
		if ValidGroup(g) != ok {
			t.Errorf("ValidGroup(%q) = %v", g, !ok)
		}
	}
	if n, ok := ZoneNumber("eastus", "eastus-2"); !ok || n != "2" {
		t.Fatalf("eastus-2 is zone 2: %q %v", n, ok)
	}
	for _, z := range []string{"", "eastus", "eastus-4", "eastus-0", "westeurope-1", "eastus-12"} {
		if _, ok := ZoneNumber("eastus", z); ok {
			t.Errorf("%q is not a zone of eastus", z)
		}
	}
	at := time.Date(2026, 11, 20, 9, 5, 0, 0, time.UTC)
	if got := SnapshotName("web-1-data", at); got != "web-1-data-20261120-0905" || !ValidDiskName(got) {
		t.Fatalf("snapshot name: %q", got)
	}
	if got := SnapshotName(strings.Repeat("d", 80), at); len(got) != 80 || !ValidDiskName(got) {
		t.Fatalf("a long disk name is cut to fit: %q", got)
	}
}
