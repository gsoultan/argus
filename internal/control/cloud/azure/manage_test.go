package azure

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/control/cloud"
)

const web1 = "aaaaaaaa-0000-4000-8000-000000000001"

// managed is a subscription where argus-control holds both roles for ext.
func managed(t *testing.T) (*Provider, *fakeMicrosoft, cloud.Account) {
	t.Helper()
	const ext = "argus-0123abcd"
	p, m, acct := newFake(t, ext, false)
	m.set(assignment{homeOID, ReaderRoleID, "/subscriptions/" + testSub, Description(ext)},
		assignment{homeOID, ManageRoleID(testSub), "/subscriptions/" + testSub, ManageDescription(ext)})
	acct.RoleARN = RoleID(testSub, ManageRoleID(testSub))
	return p, m, acct
}

func TestPowerStartsDeallocatesAndRestartsAsThePerson(t *testing.T) {
	p, m, acct := managed(t)
	ctx := context.Background()
	for act, want := range map[cloud.Action]string{cloud.ActionStart: "POST /start ", cloud.ActionStop: "POST /deallocate ", cloud.ActionReboot: "POST /restart "} {
		res, err := p.Power(ctx, acct, "dana@northwind.id", "eastus", web1, act)
		if err != nil {
			t.Fatalf("%s: %v", act, err)
		}
		if res.ResourceID != web1 || res.RequestID != "corr-1234" {
			t.Fatalf("%s must keep the id Azure's Activity log files it under: %+v", act, res)
		}
		if got := m.changed(); got[len(got)-1] != want {
			t.Fatalf("%s called %q, want %q", act, got[len(got)-1], want)
		}
	}
	m.mu.Lock()
	agent := m.agents[len(m.agents)-1]
	m.mu.Unlock()
	if !strings.Contains(agent, "dana@northwind.id") {
		t.Fatalf("the call must name the person who asked: %q", agent)
	}
	// A stop never leaves the VM shut down and still charged for.
	if slices.ContainsFunc(m.changed(), func(c string) bool { return strings.Contains(c, "powerOff") }) {
		t.Fatal("a stop must deallocate")
	}
	if _, err := p.Power(ctx, acct, "dana@northwind.id", "eastus", web1, cloud.ActionHibernate); !errors.Is(err, cloud.ErrUnsupported) {
		t.Fatalf("hibernate is not made in Azure yet: %v", err)
	}
}

func TestAChangeIsRefusedWithoutThisArgussManageAssignment(t *testing.T) {
	const ext = "argus-0123abcd"
	sub := "/subscriptions/" + testSub
	reader := assignment{homeOID, ReaderRoleID, sub, Description(ext)}
	for why, as := range map[string][]assignment{
		"Reader alone does not allow a change":                 {reader},
		"a manage role given for another Argus":                {reader, {homeOID, ManageRoleID(testSub), sub, ManageDescription("argus-ffff")}},
		"a manage role with no description":                    {reader, {homeOID, ManageRoleID(testSub), sub, ""}},
		"another principal's manage role":                      {reader, {otherOID, ManageRoleID(testSub), sub, ManageDescription(ext)}},
		"a manage role on one resource group":                  {reader, {homeOID, ManageRoleID(testSub), sub + "/resourceGroups/web-rg", ManageDescription(ext)}},
		"Contributor, which Argus does not take as its manage": {reader, {homeOID, "b24988ac-6180-42a0-ab88-20f7382dd24c", sub, ManageDescription(ext)}},
		"another subscription's manage role id":                {reader, {homeOID, ManageRoleID(otherSub), sub, ManageDescription(ext)}},
	} {
		p, m, acct := managed(t)
		m.set(as...)
		if _, err := p.Power(context.Background(), acct, "dana@northwind.id", "eastus", web1, cloud.ActionStop); err == nil {
			t.Errorf("%s: the stop was made", why)
		}
		if len(m.changed()) != 0 {
			t.Errorf("%s: Azure was asked to change something: %v", why, m.changed())
		}
	}
}

func TestAChangeFindsItsVMAndRefusesAnother(t *testing.T) {
	p, m, acct := managed(t)
	ctx := context.Background()
	for why, call := range map[string]func() error{
		"a VM in another region than the one named": func() error {
			_, err := p.Power(ctx, acct, "dana@northwind.id", "westeurope", web1, cloud.ActionStop)
			return err
		},
		"an id the subscription does not have": func() error {
			_, err := p.Power(ctx, acct, "dana@northwind.id", "eastus", "aaaaaaaa-0000-4000-8000-00000000ffff", cloud.ActionStop)
			return err
		},
		"a path in place of an id": func() error {
			_, err := p.Power(ctx, acct, "dana@northwind.id", "eastus", "../../web-1", cloud.ActionStop)
			return err
		},
		"a size that is not one": func() error {
			_, err := p.Resize(ctx, acct, "dana@northwind.id", "eastus", web1, "Standard_B2s?x=1")
			return err
		},
		"a tag Azure keeps for itself": func() error {
			_, err := p.SetTags(ctx, acct, "dana@northwind.id", "eastus", web1, map[string]string{"azure-owner": "x"}, nil)
			return err
		},
		"a disk's tags, which Argus does not change yet": func() error {
			_, err := p.SetTagsOf(ctx, acct, "dana@northwind.id", "eastus", cloud.KindVolume, web1, map[string]string{"a": "b"}, nil)
			return err
		},
		"an actor that cannot be named": func() error {
			_, err := p.Power(ctx, acct, "dana\r\nX-Evil: 1", "eastus", web1, cloud.ActionStop)
			return err
		},
	} {
		if err := call(); err == nil {
			t.Errorf("%s: not refused", why)
		}
	}
	if len(m.changed()) != 0 {
		t.Fatalf("a refused change must not reach Azure: %v", m.changed())
	}
	// Azure's own refusal comes back in its words.
	m.mu.Lock()
	m.refuse = true
	m.mu.Unlock()
	if _, err := p.Power(ctx, acct, "dana@northwind.id", "eastus", web1, cloud.ActionStop); err == nil || !strings.Contains(err.Error(), "authorization") {
		t.Fatalf("Azure's refusal: %v", err)
	}
}

func TestResizeSizesAndTags(t *testing.T) {
	p, m, acct := managed(t)
	ctx := context.Background()
	if _, err := p.Resize(ctx, acct, "dana@northwind.id", "eastus", web1, "Standard_D2s_v5"); err != nil {
		t.Fatal(err)
	}
	if got := m.changed(); got[len(got)-1] != `PATCH  {"properties":{"hardwareProfile":{"vmSize":"Standard_D2s_v5"}}}` {
		t.Fatalf("resize sent %q", got[len(got)-1])
	}
	sizes, err := p.Sizes(ctx, acct, "dana@northwind.id", "eastus", web1)
	if err != nil || strings.Join(sizes, ",") != "Standard_B2s,Standard_D2s_v5" {
		t.Fatalf("sizes: %v %v", sizes, err)
	}

	// web-1 has team=payments. Removing it by another case, and setting a
	// new tag, is a delete by the name and value it has, then a merge.
	before := len(m.changed())
	if _, err := p.SetTags(ctx, acct, "dana@northwind.id", "eastus", web1, map[string]string{"Owner": "dana"}, []string{"TEAM", "absent"}); err != nil {
		t.Fatal(err)
	}
	got := m.changed()[before:]
	want := []string{
		`PATCH /providers/Microsoft.Resources/tags/default {"operation":"Delete","properties":{"tags":{"team":"payments"}}}`,
		`PATCH /providers/Microsoft.Resources/tags/default {"operation":"Merge","properties":{"tags":{"Owner":"dana"}}}`,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("tags sent\n%q\nwant\n%q", got, want)
	}
	// Only removing a tag that is not there asks Azure for nothing.
	before = len(m.changed())
	if _, err := p.SetTags(ctx, acct, "dana@northwind.id", "eastus", web1, nil, []string{"absent"}); err != nil || len(m.changed()) != before {
		t.Fatalf("nothing to remove: %v %v", err, m.changed()[before:])
	}
	many := map[string]string{}
	for i := range MaxTags + 1 {
		many["k"+string(rune('a'+i/26))+string(rune('a'+i%26))] = "v"
	}
	if _, err := p.SetTags(ctx, acct, "dana@northwind.id", "eastus", web1, many, nil); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("more tags than Azure keeps: %v", err)
	}
}

func TestWhatArgusDoesInAzureSoFar(t *testing.T) {
	p := New(Options{})
	for _, a := range SupportedActions() {
		if !p.Supports(a) {
			t.Errorf("%s is listed but not supported", a)
		}
	}
	for _, a := range []cloud.Action{cloud.ActionImageCreate, cloud.ActionImageDelete, cloud.ActionTargetRegister, cloud.ActionHibernate} {
		if p.Supports(a) {
			t.Errorf("%s is not made in Azure yet", a)
		}
	}
	ctx := context.Background()
	if _, err := p.CreateImage(ctx, cloud.Account{}, "x", "eastus", web1, "img", false); !errors.Is(err, cloud.ErrUnsupported) {
		t.Fatalf("image: %v", err)
	}
	for k, why := range map[string]string{"": "empty", "a/b": "slash", "Windows-x": "reserved", strings.Repeat("k", 513): "long"} {
		if TagProblem(k, "v") == "" {
			t.Errorf("a tag name that is %s must be refused", why)
		}
	}
	if TagProblem("ArgusSchedule", "office-hours") != "" || TagProblem("k", strings.Repeat("v", 257)) == "" {
		t.Fatal("the schedule tag must be allowed, and a long value refused")
	}
	for s, ok := range map[string]bool{"Standard_B2s": true, "Standard_D4s_v5": true, "Standard_NC24ads_A10_v4": true, "Basic_A1": true,
		"t3.medium": false, "e2-medium": false, "Standard": false, "Standard_B2s/x": false, "": false} {
		if ValidSize(s) != ok {
			t.Errorf("ValidSize(%q) = %v", s, !ok)
		}
	}
}
