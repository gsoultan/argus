package control

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
)

/*
Cloud, phase 3: disks, snapshots, images and Elastic IPs, against the fake
provider. The account is Production, so changes wait for someone else and
an operate change (a snapshot) does not.
*/

const storageRegion = "ap-southeast-3"

// storageAPI is changeAPI with a spare disk, a snapshot an image is made
// of, a snapshot nothing uses and an unattached Elastic IP.
func storageAPI(t *testing.T) changeEnv {
	t.Helper()
	e := changeAPI(t)
	z := storageRegion + "a"
	res := e.fake.Resources[storageRegion]
	res[0].Zone = z
	res[0].Details = map[string]string{"rootDeviceName": "/dev/xvda", "image": "ami-base"}
	e.fake.Resources[storageRegion] = append(res,
		cloud.Resource{Kind: cloud.KindVolume, ID: e.account + "-vol-1", Name: "pay-api-01 root", Region: storageRegion, Zone: z,
			State: "in-use", Size: "20 GiB gp3", Links: []cloud.Ref{{Kind: cloud.KindInstance, ID: e.instance(), Name: "/dev/xvda"}}},
		cloud.Resource{Kind: cloud.KindVolume, ID: e.account + "-vol-spare", Name: "spare", Region: storageRegion, Zone: z,
			State: "available", Size: "100 GiB gp3"},
		cloud.Resource{Kind: cloud.KindSnapshot, ID: e.account + "-snap-img", Name: "base root", Region: storageRegion,
			State: "completed", Size: "20 GiB"},
		cloud.Resource{Kind: cloud.KindSnapshot, ID: e.account + "-snap-old", Name: "old backup", Region: storageRegion,
			State: "completed", Size: "50 GiB"},
		cloud.Resource{Kind: cloud.KindImage, ID: "ami-base", Name: "base", Region: storageRegion, State: "available",
			Links: []cloud.Ref{{Kind: cloud.KindSnapshot, ID: e.account + "-snap-img", Name: "/dev/xvda"}}},
		cloud.Resource{Kind: cloud.KindAddress, ID: e.account + "-eip-1", Name: "203.0.113.7", PublicIP: "203.0.113.7",
			Region: storageRegion, State: "unattached"},
	)
	e.resync(t)
	return e
}

func (e changeEnv) resync(t *testing.T) {
	t.Helper()
	acct, err := e.s.CloudAccount(context.Background(), e.acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A change reads the account in the background after it; wait for
	// that, then read it again so the fake's latest state is in.
	deadline := time.Now().Add(5 * time.Second)
	for !e.a.SyncCloudAccount(context.Background(), acct) {
		if time.Now().After(deadline) {
			t.Fatal("the account was being read for too long")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e changeEnv) storage(action, id, extra string) string {
	return `{"accountId":"` + e.acct.ID + `","region":"` + storageRegion + `","action":"` + action + `","resourceId":"` + id + `"` + extra + `}`
}

func (e changeEnv) approveChange(t *testing.T, c CloudChange) CloudChange {
	t.Helper()
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes/"+c.ID+"/approve", "approver-token", "")
	if w.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", w.Code, w.Body)
	}
	var done CloudChange
	_ = json.Unmarshal(w.Body.Bytes(), &done)
	return done
}

func (e changeEnv) called(t *testing.T, want string) {
	t.Helper()
	for _, c := range e.fake.CallLog() {
		if c == want {
			return
		}
	}
	t.Fatalf("no call %q in %v", want, e.fake.CallLog())
}

// A new disk on Production waits, is encrypted, runs as the person who
// asked and lands attached to the instance in its zone.
func TestCloudVolumeCreateAndAttach(t *testing.T) {
	e := storageAPI(t)
	spec := `,"spec":{"name":"pay-data","zone":"` + storageRegion + `a","sizeGb":200,"type":"gp3","attachTo":"` + e.instance() + `"}`
	c := e.ask(t, "admin-token", e.storage("volume-create", "", spec), http.StatusAccepted)
	if c.Level != cloud.LevelChange || !strings.Contains(c.Summary, "200 GiB gp3, encrypted") || !strings.Contains(c.Summary, "as /dev/sdf") {
		t.Fatalf("create plan: %+v", c)
	}
	done := e.approveChange(t, c)
	if done.State != "applied" || !strings.HasPrefix(done.ResourceID, "vol-fake") {
		t.Fatalf("create did not apply: %+v", done)
	}
	e.called(t, "volume-create "+storageRegion+"a "+e.admin)
	if !strings.Contains(string(done.Result), "Attached to "+e.instance()) {
		t.Fatalf("not attached: %s", done.Result)
	}
	// Another zone, a bad size and a bad type are refused before AWS.
	e.ask(t, "admin-token", e.storage("volume-create", "", `,"spec":{"zone":"us-east-1a","sizeGb":10}`), http.StatusBadRequest)
	e.ask(t, "admin-token", e.storage("volume-create", "", `,"spec":{"zone":"`+storageRegion+`a","sizeGb":0}`), http.StatusBadRequest)
	e.ask(t, "admin-token", e.storage("volume-create", "", `,"spec":{"zone":"`+storageRegion+`a","sizeGb":10,"type":"magnetic"}`), http.StatusBadRequest)
	// A disk from a snapshot is at least the snapshot's size.
	e.ask(t, "admin-token", e.storage("volume-create", "", `,"spec":{"zone":"`+storageRegion+`a","sizeGb":10,"snapshot":"`+e.account+`-snap-old"}`), http.StatusBadRequest)
}

func TestCloudVolumeGrowNeverShrinks(t *testing.T) {
	e := storageAPI(t)
	spare := e.account + "-vol-spare"
	e.ask(t, "admin-token", e.storage("volume-grow", spare, `,"spec":{"sizeGb":50}`), http.StatusBadRequest)
	e.ask(t, "admin-token", e.storage("volume-grow", spare, `,"spec":{"sizeGb":100}`), http.StatusBadRequest)
	e.ask(t, "admin-token", e.storage("volume-grow", spare, `,"spec":{"iops":99999}`), http.StatusBadRequest)
	c := e.ask(t, "admin-token", e.storage("volume-grow", spare, `,"spec":{"sizeGb":250}`), http.StatusAccepted)
	if c.Summary != "Grow spare from 100 to 250 GiB" {
		t.Fatalf("grow summary: %q", c.Summary)
	}
	done := e.approveChange(t, c)
	if done.State != "applied" || !strings.Contains(string(done.Result), "Disks tab") {
		t.Fatalf("grow must say the file system grows separately: %+v %s", done, done.Result)
	}
}

// A snapshot is an operate change: even on Production it runs at once.
func TestCloudSnapshotRunsAtOnce(t *testing.T) {
	e := storageAPI(t)
	c := e.ask(t, "admin-token", e.storage("snapshot-create", e.account+"-vol-1", `,"spec":{"name":"before upgrade"}`), http.StatusOK)
	if c.State != "applied" || c.ResourceID != e.account+"-vol-1" || !strings.Contains(string(c.Result), "snap-fake") {
		t.Fatalf("snapshot: %+v %s", c, c.Result)
	}
	e.called(t, "snapshot-create "+e.account+"-vol-1 "+e.admin)
	target := m2target(c)
	if auditCount(t, e.s, "cloud.change.requested", target) != 1 || auditCount(t, e.s, "cloud.change.applied", target) != 1 {
		t.Fatal("a snapshot must be audited as requested and applied")
	}
}

func TestCloudVolumeDeleteNeedsTheNameAndADetachedDisk(t *testing.T) {
	e := storageAPI(t)
	root := e.account + "-vol-1"
	spare := e.account + "-vol-spare"
	e.ask(t, "admin-token", e.storage("volume-delete", spare, `,"confirm":"spar"`), http.StatusBadRequest)
	e.ask(t, "admin-token", e.storage("volume-delete", root, `,"confirm":"pay-api-01 root"`), http.StatusConflict)
	// The root disk of a running instance is not detached.
	e.ask(t, "admin-token", e.storage("volume-detach", root, ""), http.StatusConflict)
	c := e.ask(t, "admin-token", e.storage("volume-delete", spare, `,"confirm":"spare","spec":{"finalSnapshot":true}`), http.StatusAccepted)
	if c.Level != cloud.LevelRemove || !strings.Contains(c.Summary, "after a final snapshot") {
		t.Fatalf("delete plan: %+v", c)
	}
	done := e.approveChange(t, c)
	if done.State != "applied" || !strings.Contains(string(done.Result), "snap-fake") {
		t.Fatalf("delete: %+v %s", done, done.Result)
	}
	e.called(t, "volume-delete "+spare+" snapshot=true "+e.admin)
}

func TestCloudAttachAndDetachASpareDisk(t *testing.T) {
	e := storageAPI(t)
	spare := e.account + "-vol-spare"
	c := e.ask(t, "admin-token", e.storage("volume-attach", spare, `,"spec":{"instance":"`+e.instance()+`","device":"/dev/xvda"}`), http.StatusBadRequest)
	_ = c
	c = e.ask(t, "admin-token", e.storage("volume-attach", spare, `,"spec":{"instance":"`+e.instance()+`"}`), http.StatusAccepted)
	if !strings.Contains(c.Summary, "Attach spare to pay-api-01 as /dev/sdf") {
		t.Fatalf("attach: %q", c.Summary)
	}
	if e.approveChange(t, c).State != "applied" {
		t.Fatal("attach did not apply")
	}
	e.resync(t)
	d := e.ask(t, "admin-token", e.storage("volume-detach", spare, ""), http.StatusAccepted)
	if !strings.Contains(d.Summary, "Detach spare (/dev/sdf) from pay-api-01") {
		t.Fatalf("detach: %q", d.Summary)
	}
	if e.approveChange(t, d).State != "applied" {
		t.Fatal("detach did not apply")
	}
}

// A snapshot an image is made of is not deleted on its own; deleting the
// image with its snapshots takes both.
func TestCloudSnapshotInUseByAnImage(t *testing.T) {
	e := storageAPI(t)
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes", "admin-token",
		e.storage("snapshot-delete", e.account+"-snap-img", `,"confirm":"base root"`))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "image base") {
		t.Fatalf("a snapshot an image uses must be refused plainly: %d %s", w.Code, w.Body)
	}
	c := e.ask(t, "admin-token", e.storage("image-delete", "ami-base", `,"confirm":"base","spec":{"withSnapshots":true}`), http.StatusAccepted)
	if !strings.Contains(c.Summary, "and its snapshots") || !strings.Contains(c.Summary, "1 instance launched from it keep") {
		t.Fatalf("image delete: %q", c.Summary)
	}
	done := e.approveChange(t, c)
	if done.State != "applied" || !strings.Contains(string(done.Result), "Deleted 1 of its snapshots") {
		t.Fatalf("image delete: %+v %s", done, done.Result)
	}
	// One nothing uses goes.
	old := e.ask(t, "admin-token", e.storage("snapshot-delete", e.account+"-snap-old", `,"confirm":"old backup"`), http.StatusAccepted)
	if e.approveChange(t, old).State != "applied" {
		t.Fatal("snapshot delete did not apply")
	}
}

func TestCloudImageFromAnInstance(t *testing.T) {
	e := storageAPI(t)
	e.ask(t, "admin-token", e.storage("image-create", e.instance(), `,"spec":{"name":"x"}`), http.StatusBadRequest)
	e.ask(t, "admin-token", e.storage("image-create", e.instance(), `,"spec":{"name":"base"}`), http.StatusConflict)
	c := e.ask(t, "admin-token", e.storage("image-create", e.instance(), `,"spec":{"name":"pay-api 2026-11","reboot":true}`), http.StatusAccepted)
	if !strings.Contains(c.Summary, "rebooting it") {
		t.Fatalf("image plan: %q", c.Summary)
	}
	done := e.approveChange(t, c)
	if done.State != "applied" || done.ResourceID != e.instance() || !strings.Contains(string(done.Result), "ami-fake") {
		t.Fatalf("image: %+v %s", done, done.Result)
	}
	e.called(t, "image-create "+e.instance()+" reboot=true "+e.admin)
}

func TestCloudElasticIPs(t *testing.T) {
	e := storageAPI(t)
	eip := e.account + "-eip-1"
	c := e.ask(t, "admin-token", e.storage("address-associate", eip, `,"spec":{"instance":"`+e.instance()+`"}`), http.StatusAccepted)
	if e.approveChange(t, c).State != "applied" {
		t.Fatal("associate did not apply")
	}
	e.resync(t)
	// It cannot be released while attached, and needs the address typed.
	e.ask(t, "admin-token", e.storage("address-release", eip, `,"confirm":"203.0.113.7"`), http.StatusConflict)
	al := e.ask(t, "admin-token", e.storage("address-allocate", "", `,"spec":{"name":"spare"}`), http.StatusAccepted)
	al = e.approveChange(t, al)
	if al.State != "applied" || !strings.HasPrefix(al.ResourceID, "eipalloc-fake") {
		t.Fatalf("allocate: %+v", al)
	}
	e.resync(t)
	// The instance has an address now, so a second is refused.
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes", "admin-token",
		e.storage("address-associate", al.ResourceID, `,"spec":{"instance":"`+e.instance()+`"}`))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "already has the Elastic IP 203.0.113.7") {
		t.Fatalf("a second address must be refused: %d %s", w.Code, w.Body)
	}
	e.ask(t, "admin-token", e.storage("address-release", al.ResourceID, `,"confirm":"nope"`), http.StatusBadRequest)
	ip := ""
	for _, r := range e.fake.Resources[storageRegion] {
		if r.ID == al.ResourceID {
			ip = r.PublicIP
		}
	}
	rel := e.ask(t, "admin-token", e.storage("address-release", al.ResourceID, `,"confirm":"`+ip+`"`), http.StatusAccepted)
	if e.approveChange(t, rel).State != "applied" {
		t.Fatal("release did not apply")
	}
}

// An account whose manage role predates this phase is told what to do,
// and AWS is never asked.
func TestCloudStorageNeedsTheUpdatedRole(t *testing.T) {
	e := storageAPI(t)
	e.fake.Missing = []string{"ec2:volume-create"}
	e.resync(t)
	acct, _ := e.s.CloudAccount(context.Background(), e.acct.ID)
	if len(acct.ManageMissing) != 1 || acct.ManageCheckedAt == nil {
		t.Fatalf("the sync must record what the role lacks: %+v", acct)
	}
	w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes", "admin-token",
		e.storage("volume-create", "", `,"spec":{"zone":"`+storageRegion+`a","sizeGb":10}`))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "argus-access stack") {
		t.Fatalf("a missing permission must be refused with what to do: %d %s", w.Code, w.Body)
	}
	for _, c := range e.fake.CallLog() {
		if strings.HasPrefix(c, "volume-create") {
			t.Fatal("AWS must not be asked")
		}
	}
	// Other storage changes still work, and the stack update is seen at
	// the next read.
	e.ask(t, "admin-token", e.storage("snapshot-create", e.account+"-vol-1", ""), http.StatusOK)
	e.fake.Missing = nil
	e.resync(t)
	e.ask(t, "admin-token", e.storage("volume-create", "", `,"spec":{"zone":"`+storageRegion+`a","sizeGb":10}`), http.StatusAccepted)
}

func TestCloudTagsOnADisk(t *testing.T) {
	e := storageAPI(t)
	spare := e.account + "-vol-spare"
	c := e.ask(t, "admin-token", e.storage("tags", spare, `,"kind":"volume","spec":{"set":{"Team":"payments"}}`), http.StatusOK)
	if c.State != "applied" || c.ResourceName != "spare" {
		t.Fatalf("tags on a disk: %+v", c)
	}
	e.ask(t, "admin-token", e.storage("tags", spare, `,"kind":"firewall","spec":{"set":{"Team":"payments"}}`), http.StatusBadRequest)
	e.ask(t, "admin-token", e.storage("stop", spare, `,"kind":"volume"`), http.StatusNotFound)
}

func TestFreeDevice(t *testing.T) {
	if got := cloud.FreeDevice([]string{"/dev/xvda", "/dev/sdf", "/dev/xvdg"}); got != "/dev/sdh" {
		t.Fatalf("free device: %s", got)
	}
	if !cloud.ValidDevice("/dev/sdp") || cloud.ValidDevice("/dev/sda1") || cloud.ValidDevice("/dev/sdq") {
		t.Fatal("device names")
	}
}
