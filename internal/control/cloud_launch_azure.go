package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
	"github.com/gsoultan/argus/internal/control/cloud/azure"
)

/*
A launch in Azure differs from the other clouds' in what the plan below
carries: the VM goes into a resource group, which has to be named; a zone
is optional; a network interface takes one network security group, not
several; and there is no deletion protection to turn on, because Azure's
is a resource lock, which Argus holds no right to set.
*/

// azureGroups are the resource groups Argus has read something in, in
// Azure's own spelling.
func (a *API) azureGroups(ctx context.Context, accountID string) []string {
	seen := map[string]string{}
	for _, k := range []cloud.Kind{cloud.KindInstance, cloud.KindVolume, cloud.KindSnapshot, cloud.KindNetwork, cloud.KindAddress, cloud.KindFirewall} {
		items, err := a.store.CloudResources(ctx, k)
		if err != nil {
			continue
		}
		for _, it := range items {
			if g := it.Resource.Details["resourceGroup"]; it.AccountID == accountID && g != "" {
				if _, ok := seen[strings.ToLower(g)]; !ok {
					seen[strings.ToLower(g)] = g
				}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for _, g := range seen {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

// azureZones are the zones a region may have. Azure says which of them a
// size is offered in only when asked for that size, so the three are
// offered and Azure refuses one the region or the size lacks.
func azureZones(region string) []string {
	return []string{region + "-1", region + "-2", region + "-3"}
}

func (a *API) planAzureLaunch(r *http.Request, m cloudManaged, actor string, in *cloudChangeIn, ch CloudChange) (CloudChange, int, error) {
	bad := http.StatusBadRequest
	var s cloudLaunchSpec
	if err := json.Unmarshal(in.Spec, &s); err != nil {
		return ch, bad, errors.New("a launch needs a name, resource group, image, size, disk and subnet")
	}
	ctx := r.Context()
	s.Name = strings.ToLower(strings.TrimSpace(s.Name))
	if !cloud.ValidHostname(s.Name) {
		return ch, bad, errors.New("the name becomes the VM's and the server's: lowercase letters, digits and dashes, up to 63")
	}
	s.Group = strings.TrimSpace(s.Group)
	if !azure.ValidGroup(s.Group) {
		return ch, bad, errors.New("pick the resource group the VM is made in")
	}
	g, ok := a.azureGroup(ctx, m.acct.ID, s.Group)
	if !ok {
		return ch, bad, errors.New("Argus has read nothing in a resource group named " + s.Group + "; pick one it has, or make the group in the Azure portal first")
	}
	s.Group = g
	if !azure.ValidSize(s.Size) {
		return ch, bad, errors.New("pick a VM size, like Standard_B2s")
	}
	if s.DiskGB == 0 {
		s.DiskGB = azure.MinBootGB
	}
	if s.DiskGB < azure.MinBootGB || s.DiskGB > azure.MaxBootGB {
		return ch, bad, fmt.Errorf("the boot disk is %d to %d GB", azure.MinBootGB, azure.MaxBootGB)
	}
	if s.Zone != "" {
		if _, ok := azure.ZoneNumber(in.Region, s.Zone); !ok {
			return ch, bad, errors.New("pick a zone of " + in.Region + ", or none")
		}
	}
	if len(s.Firewalls) > 1 {
		return ch, bad, errors.New("a VM's network interface takes one network security group; pick one, or none to rely on the subnet's")
	}
	if s.Spot {
		return ch, bad, errors.New("Argus does not launch Spot VMs in Azure yet")
	}
	if s.CapacityReservation != "" {
		return ch, bad, errors.New("Argus does not launch into Azure reservations yet")
	}
	// Azure's deletion protection is a resource lock, which Argus cannot set.
	s.Protect = false
	if s.Count == 0 {
		s.Count = 1
	}
	if s.Count < 1 || s.Count > maxLaunchCount {
		return ch, bad, fmt.Errorf("launch 1 to %d at once", maxLaunchCount)
	}
	names, err := launchNames(s.Name, s.Count)
	if err != nil {
		return ch, bad, err
	}
	for _, n := range names {
		if a.azureNamed(ctx, m.acct.ID, cloud.KindInstance, s.Group, n) {
			return ch, http.StatusConflict, errors.New("resource group " + s.Group + " already has a VM named " + n)
		}
		if _, dup := a.assetByHostname(ctx, n); dup {
			return ch, http.StatusConflict, errors.New(n + " is already a server in Argus; pick another name")
		}
	}
	s.Names = names
	if len(s.ExtraDisks) > cloud.MaxExtraDisks {
		return ch, bad, fmt.Errorf("at most %d extra disks", cloud.MaxExtraDisks)
	}
	for _, gb := range s.ExtraDisks {
		if gb < azure.MinDiskGB || gb > azure.MaxDiskGB {
			return ch, bad, fmt.Errorf("an extra disk is %d to %d GB", azure.MinDiskGB, azure.MaxDiskGB)
		}
	}
	for k, v := range s.Tags {
		if p := azure.TagProblem(k, v); p != "" {
			return ch, bad, fmt.Errorf("%s=%s cannot be a tag: %s", k, v, p)
		}
	}
	// Argus adds two of its own: who launched it, and the change.
	if len(s.Tags) > azure.MaxTags-2 {
		return ch, bad, fmt.Errorf("at most %d tags", azure.MaxTags-2)
	}
	if _, err := a.cloudResource(ctx, m.acct.ID, in.Region, cloud.KindSubnet, s.Subnet); err != nil {
		return ch, bad, errors.New("pick a subnet in " + in.Region)
	}
	for _, f := range s.Firewalls {
		if _, err := a.cloudResource(ctx, m.acct.ID, in.Region, cloud.KindFirewall, f); err != nil {
			return ch, bad, errors.New("pick a network security group in " + in.Region)
		}
	}
	ictx, cancel := context.WithTimeout(ctx, 30*time.Second)
	images, err := m.manager.Images(ictx, m.target, cloud.Actor(actor), in.Region)
	cancel()
	if err != nil {
		return ch, http.StatusBadGateway, errors.New("Argus could not list images: " + err.Error())
	}
	i := slices.IndexFunc(images, func(im cloud.Image) bool { return im.ID == s.Image })
	if i < 0 {
		return ch, bad, errors.New("pick one of the images offered")
	}
	img := images[i]
	s.ImageLabel = img.Label
	if !img.Linux {
		return ch, bad, errors.New("Argus launches Linux VMs in Azure for now; launch " + img.Label + " in the Azure portal")
	}
	if int32(img.MinDiskGB) > s.DiskGB {
		return ch, bad, fmt.Errorf("%s needs a disk of at least %d GB", img.Label, img.MinDiskGB)
	}
	if s.Agent {
		if _, problem := a.agentUserData(r, s.Name, "check"); problem != "" {
			return ch, http.StatusConflict, errors.New(problem)
		}
	}
	env := m.acct.Environment
	if e := cloudEnvOf(s.Tags, ""); e != "" {
		env = e
	}
	ch.Environment = env
	ch.ResourceName = s.Name
	ch.Spec, _ = json.Marshal(s)
	what := s.Name
	if len(names) > 1 {
		what = fmt.Sprintf("%d VMs, %s to %s", len(names), names[0], names[len(names)-1])
	}
	ch.Summary = fmt.Sprintf("Launch %s: %s, %s, %d GB in %s, %s, resource group %s", what, img.Label, s.Size, s.DiskGB, in.Region, azureZoneWord(s.Zone), s.Group)
	if n := len(s.ExtraDisks); n > 0 {
		ch.Summary += fmt.Sprintf(", %d extra disk%s", n, plural(n))
	}
	if s.PublicIP {
		ch.Summary += ", with a public address"
	}
	if s.Agent {
		ch.Summary += ", with the Argus agent"
	} else {
		ch.Summary += ", without the agent: nobody can sign in until access is reset in the Azure portal"
	}
	return ch, http.StatusOK, nil
}
