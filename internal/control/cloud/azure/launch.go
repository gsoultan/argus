package azure

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Launching a VM. One call makes the VM, its disks, its network interface
and, when asked, its public address: the interface and the address are
described inside the VM (networkInterfaceConfigurations), so Azure makes
them with it or makes nothing, and nothing is left behind by a launch that
fails halfway. Each is marked to be deleted with the VM, so removing a VM
Argus launched leaves nothing billed.

Azure does not make a Linux VM without an administrator's account that
has a password or an SSH key. Argus gives it a key made for this one
launch and keeps no part of it: nobody, Argus included, can sign in with
it. People reach the VM through its Argus agent, which enrolls at first
boot; without the agent, someone resets the account's access in the Azure
portal.

The VM gets no managed identity, so it holds no Azure credentials, and
Argus needs no right to hand one out.

A Spot VM runs on Azure's spare capacity at the Spot price, with no price
cap of Argus's (maxPrice -1: it is never taken back over price, and never
costs more than the ordinary price). When Azure takes the capacity back it
deallocates the VM: the VM, its disks and its address stay, so nothing on
them is lost and the VM starts again from Argus once there is room, and
the disks and the address go on being billed meanwhile. Azure has no Spot
capacity for B-series sizes.
*/

// AdminUser is the administrator's account Azure requires on a new VM.
const AdminUser = "azureuser"

// LaunchSizes are the VM sizes the launch form offers in Azure.
var LaunchSizes = []string{"Standard_B1s", "Standard_B2s", "Standard_B2ms", "Standard_B4ms",
	"Standard_D2s_v5", "Standard_D4s_v5", "Standard_D8s_v5", "Standard_D2as_v5", "Standard_D4as_v5",
	"Standard_E2s_v5", "Standard_E4s_v5", "Standard_F2s_v2", "Standard_F4s_v2"}

// SpotSize reports whether Azure offers size as a Spot VM: every size but
// the burstable B-series.
func SpotSize(size string) bool {
	return !strings.HasPrefix(strings.ToLower(size), "standard_b")
}

// OSDiskType is the type of the disks a launch makes.
const OSDiskType = "StandardSSD_LRS"

// The sizes of a launch's boot disk, in GiB: the images offered are 30.
const (
	MinBootGB = 30
	MaxBootGB = 4095
)

// marketImages are the Marketplace images Argus offers to launch from,
// each at its latest version so a VM starts patched. All are free of
// Marketplace terms to accept, and second generation.
var marketImages = []struct{ publisher, offer, sku, family, label string }{
	{"Canonical", "ubuntu-24_04-lts", "server", "ubuntu-24.04", "Ubuntu 24.04 LTS"},
	{"Canonical", "0001-com-ubuntu-server-jammy", "22_04-lts-gen2", "ubuntu-22.04", "Ubuntu 22.04 LTS"},
	{"Debian", "debian-12", "12-gen2", "debian-12", "Debian 12"},
}

const ownImagePrefix = "image/"

// OwnImage is the id the launch form knows one of the subscription's own
// images by.
func OwnImage(group, name string) string { return ownImagePrefix + group + "/" + name }

func imagePath(subscription, rg, name string) string {
	return SubscriptionScope(subscription) + "/resourceGroups/" + url.PathEscape(rg) + "/providers/Microsoft.Compute/images/" + url.PathEscape(name)
}

// Images are the Marketplace images Argus offers, then the subscription's
// own images in region that are ready.
func (p *Provider) Images(ctx context.Context, a cloud.Account, actor cloud.Actor, region string) ([]cloud.Image, error) {
	t, _, err := p.session(ctx, a, actor)
	if err != nil {
		return nil, err
	}
	var out []cloud.Image
	for _, m := range marketImages {
		out = append(out, cloud.Image{Family: m.family, Label: m.label, ID: m.publisher + ":" + m.offer + ":" + m.sku,
			Name: m.offer + " " + m.sku, Linux: true, MinDiskGB: MinBootGB})
	}
	var own []cloud.Image
	err = p.pages(ctx, t, actor, p.ep.arm+SubscriptionScope(a.AccountID)+"/providers/Microsoft.Compute/images?api-version="+computeAPI,
		func(v json.RawMessage) error {
			var items []raw
			if err := json.Unmarshal(v, &items); err != nil {
				return err
			}
			for _, im := range items {
				rg := group(im.ID)
				if place(im.Location) != region || !strings.EqualFold(im.Properties.ProvisioningState, "Succeeded") || !ValidGroup(rg) || !ValidDiskName(im.Name) {
					continue
				}
				od := im.Properties.StorageProfile.OSDisk
				own = append(own, cloud.Image{Family: "own", Label: im.Name + " (this subscription)", ID: OwnImage(rg, im.Name), Name: im.Name,
					Linux: strings.EqualFold(od.OSType, "Linux"), MinDiskGB: od.DiskSizeGB})
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(own, func(x, y cloud.Image) int { return strings.Compare(x.ID, y.ID) })
	return append(out, own...), nil
}

// imageReference is what a VM is told to start from, for an image id the
// form offered.
func imageReference(subscription, id string) (map[string]string, bool) {
	for _, m := range marketImages {
		if id == m.publisher+":"+m.offer+":"+m.sku {
			return map[string]string{"publisher": m.publisher, "offer": m.offer, "sku": m.sku, "version": "latest"}, true
		}
	}
	if rest, ok := strings.CutPrefix(id, ownImagePrefix); ok {
		rg, name, ok := strings.Cut(rest, "/")
		if ok && ValidGroup(rg) && ValidDiskName(name) {
			return map[string]string{"id": imagePath(subscription, rg, name)}, true
		}
	}
	return nil, false
}

// newKey makes the key a launch gives the administrator's account.
var newKey = throwawayKey

// throwawayKey makes the public half of an RSA key in OpenSSH's form and
// forgets the private half.
func throwawayKey() (string, error) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", err
	}
	field := func(b []byte) []byte {
		out := make([]byte, 4, 4+len(b))
		binary.BigEndian.PutUint32(out, uint32(len(b)))
		return append(out, b...)
	}
	mpint := func(n *big.Int) []byte {
		b := n.Bytes()
		if len(b) > 0 && b[0]&0x80 != 0 {
			b = append([]byte{0}, b...)
		}
		return field(b)
	}
	blob := append(field([]byte("ssh-rsa")), mpint(big.NewInt(int64(k.PublicKey.E)))...)
	blob = append(blob, mpint(k.PublicKey.N)...)
	return "ssh-rsa " + base64.StdEncoding.EncodeToString(blob) + " argus-no-one-holds-this-key", nil
}

// subnetPath finds the subnet the console knows as id, in region, and
// builds its path from the checked names of its network and itself.
func (p *Provider) subnetPath(ctx context.Context, t token, actor cloud.Actor, subscription, region, id string) (string, error) {
	path := ""
	err := p.pages(ctx, t, actor, p.ep.arm+SubscriptionScope(subscription)+"/providers/Microsoft.Network/virtualNetworks?api-version="+networkAPI,
		func(v json.RawMessage) error {
			var items []raw
			if err := json.Unmarshal(v, &items); err != nil {
				return err
			}
			for _, nw := range items {
				for _, s := range nw.Properties.Subnets {
					if pathID(s.ID) != id || place(nw.Location) != region {
						continue
					}
					rg := group(nw.ID)
					if !ValidGroup(rg) || !reNICName.MatchString(nw.Name) || !reNICName.MatchString(s.Name) {
						return fmt.Errorf("Azure named subnet %s in a way Argus cannot address", s.Name)
					}
					path = SubscriptionScope(subscription) + "/resourceGroups/" + url.PathEscape(rg) + "/providers/Microsoft.Network/virtualNetworks/" +
						url.PathEscape(nw.Name) + "/subnets/" + url.PathEscape(s.Name)
				}
			}
			return nil
		})
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", fmt.Errorf("subscription %s has no such subnet in %s", subscription, region)
	}
	return path, nil
}

// securityGroupPath finds the network security group with resourceGuid id
// in region.
func (p *Provider) securityGroupPath(ctx context.Context, t token, actor cloud.Actor, subscription, region, id string) (string, error) {
	if !ValidGUID(id) {
		return "", fmt.Errorf("%q is not a network security group id", id)
	}
	path := ""
	err := p.pages(ctx, t, actor, p.ep.arm+SubscriptionScope(subscription)+"/providers/Microsoft.Network/networkSecurityGroups?api-version="+networkAPI,
		func(v json.RawMessage) error {
			var items []raw
			if err := json.Unmarshal(v, &items); err != nil {
				return err
			}
			for _, g := range items {
				if !strings.EqualFold(g.Properties.ResourceGUID, id) {
					continue
				}
				rg := group(g.ID)
				if place(g.Location) != region {
					return fmt.Errorf("%s is in %s, not %s", g.Name, place(g.Location), region)
				}
				if !ValidGroup(rg) || !reNICName.MatchString(g.Name) {
					return fmt.Errorf("Azure named network security group %s in a way Argus cannot address", id)
				}
				path = SubscriptionScope(subscription) + "/resourceGroups/" + url.PathEscape(rg) + "/providers/Microsoft.Network/networkSecurityGroups/" + url.PathEscape(g.Name)
			}
			return nil
		})
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", fmt.Errorf("subscription %s has no network security group with id %s", subscription, id)
	}
	return path, nil
}

// launchNetworkAPI is the Microsoft.Network version Azure makes a VM's
// interface and address with, when they are described inside the VM.
const launchNetworkAPI = "2022-11-01"

// Launch makes one Linux VM in s.Group: from an image, with a boot disk
// and any extra disks, one network interface on a subnet, under at most
// one network security group, and a static public address when asked.
// Everything it makes is deleted with the VM.
func (p *Provider) Launch(ctx context.Context, a cloud.Account, actor cloud.Actor, s cloud.LaunchSpec) (cloud.Result, error) {
	if !cloud.ValidHostname(s.Name) {
		return cloud.Result{}, fmt.Errorf("%q cannot name a VM: lowercase letters, digits and -, up to 63", s.Name)
	}
	if !ValidGroup(s.Group) {
		return cloud.Result{}, errors.New("a VM is made in a resource group; pick one")
	}
	if !ValidSize(s.Size) {
		return cloud.Result{}, fmt.Errorf("%q is not a VM size, like Standard_B2s", s.Size)
	}
	if place(s.Region) == "" || place(s.Region) != s.Region {
		return cloud.Result{}, fmt.Errorf("%q is not a region", s.Region)
	}
	zone := ""
	if s.Zone != "" {
		n, ok := ZoneNumber(s.Region, s.Zone)
		if !ok {
			return cloud.Result{}, fmt.Errorf("pick a zone of %s, or none", s.Region)
		}
		zone = n
	}
	image, ok := imageReference(a.AccountID, s.Image)
	if !ok {
		return cloud.Result{}, errors.New("pick one of the images offered")
	}
	if s.DiskGB < MinBootGB || s.DiskGB > MaxBootGB {
		return cloud.Result{}, fmt.Errorf("the boot disk is %d to %d GB", MinBootGB, MaxBootGB)
	}
	if len(s.ExtraDisks) > cloud.MaxExtraDisks {
		return cloud.Result{}, fmt.Errorf("at most %d extra disks", cloud.MaxExtraDisks)
	}
	for _, gb := range s.ExtraDisks {
		if gb < MinDiskGB || gb > MaxDiskGB {
			return cloud.Result{}, fmt.Errorf("an extra disk is %d to %d GB", MinDiskGB, MaxDiskGB)
		}
	}
	if len(s.Firewalls) > 1 {
		return cloud.Result{}, errors.New("a network interface takes one network security group")
	}
	switch {
	case s.Spot && !SpotSize(s.Size):
		return cloud.Result{}, errors.New("Azure has no Spot capacity for B-series sizes; pick another size")
	case s.CapacityReservation != "":
		return cloud.Result{}, errNotYet("launch into a reservation")
	case s.Protect:
		return cloud.Result{}, errNotYet("lock a VM against deletion")
	}
	for k, v := range s.Tags {
		if why := TagProblem(k, v); why != "" {
			return cloud.Result{}, fmt.Errorf("%s=%s: %s", k, v, why)
		}
	}
	t, _, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Result{}, err
	}
	subnet, err := p.subnetPath(ctx, t, actor, a.AccountID, s.Region, s.Subnet)
	if err != nil {
		return cloud.Result{}, err
	}
	nicProps := map[string]any{"primary": true, "deleteOption": "Delete"}
	if len(s.Firewalls) == 1 {
		nsg, err := p.securityGroupPath(ctx, t, actor, a.AccountID, s.Region, s.Firewalls[0])
		if err != nil {
			return cloud.Result{}, err
		}
		nicProps["networkSecurityGroup"] = map[string]string{"id": nsg}
	}
	ipconfig := map[string]any{"subnet": map[string]string{"id": subnet}, "primary": true}
	if s.PublicIP {
		ipconfig["publicIPAddressConfiguration"] = map[string]any{"name": s.Name + "-ip", "sku": map[string]string{"name": "Standard"},
			"properties": map[string]any{"deleteOption": "Delete", "publicIPAllocationMethod": "Static"}}
	}
	nicProps["ipConfigurations"] = []map[string]any{{"name": "ipconfig1", "properties": ipconfig}}

	// A PUT at a name in use would change the VM that has it.
	path := SubscriptionScope(a.AccountID) + "/resourceGroups/" + url.PathEscape(s.Group) + "/providers/Microsoft.Compute/virtualMachines/" + url.PathEscape(s.Name)
	u := p.ep.arm + path + "?api-version=" + computeAPI
	if there, err := p.exists(ctx, t, actor, u, nil); err != nil {
		return cloud.Result{}, err
	} else if there {
		return cloud.Result{}, fmt.Errorf("resource group %s already has a VM named %s", s.Group, s.Name)
	}
	key, err := newKey()
	if err != nil {
		return cloud.Result{}, fmt.Errorf("could not make the VM's throwaway key: %w", err)
	}
	managed := map[string]string{"storageAccountType": OSDiskType}
	data := []map[string]any{}
	for i, gb := range s.ExtraDisks {
		data = append(data, map[string]any{"lun": i, "name": fmt.Sprintf("%s-data-%d", s.Name, i+1), "createOption": "Empty",
			"diskSizeGB": gb, "deleteOption": "Delete", "managedDisk": managed})
	}
	osProfile := map[string]any{"computerName": s.Name, "adminUsername": AdminUser,
		"linuxConfiguration": map[string]any{"disablePasswordAuthentication": true,
			"ssh": map[string]any{"publicKeys": []map[string]string{{"path": "/home/" + AdminUser + "/.ssh/authorized_keys", "keyData": key}}}}}
	if s.UserData != "" {
		// cloud-init runs a script given as custom data once, at the first boot.
		osProfile["customData"] = base64.StdEncoding.EncodeToString([]byte(s.UserData))
	}
	body := map[string]any{
		"location": s.Region,
		"properties": map[string]any{
			"hardwareProfile": map[string]string{"vmSize": s.Size},
			"storageProfile": map[string]any{
				"imageReference": image,
				"osDisk": map[string]any{"name": s.Name + "_OsDisk", "createOption": "FromImage", "diskSizeGB": s.DiskGB,
					"deleteOption": "Delete", "managedDisk": managed},
				"dataDisks": data,
			},
			"osProfile": osProfile,
			"networkProfile": map[string]any{"networkApiVersion": launchNetworkAPI,
				"networkInterfaceConfigurations": []map[string]any{{"name": s.Name + "-nic", "properties": nicProps}}},
		},
	}
	if s.Spot {
		props := body["properties"].(map[string]any)
		props["priority"] = "Spot"
		props["evictionPolicy"] = "Deallocate"
		props["billingProfile"] = map[string]any{"maxPrice": -1}
	}
	if len(s.Tags) > 0 {
		body["tags"] = s.Tags
	}
	if zone != "" {
		body["zones"] = []string{zone}
	}
	var made raw
	req, err := p.send(ctx, t, actor, http.MethodPut, u, body, &made)
	if err != nil {
		return cloud.Result{}, err
	}
	id := strings.ToLower(made.Properties.VMID)
	if !ValidGUID(id) {
		id = pathID(path)
	}
	return cloud.Result{ResourceID: id, RequestID: req}, nil
}
