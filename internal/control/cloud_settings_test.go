package control

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/control/cloud"
)

// A settings change names only what differs from now, waits for approval
// on Production, then runs as the person who asked. Stop protection then
// refuses a stop, Argus's own included.
func TestCloudSettingsChangeOnlyWhatDiffers(t *testing.T) {
	e := changeAPI(t)
	w := do(t, e.h, http.MethodGet, "/api/v1/cloud/instance-settings?accountId="+e.acct.ID+"&region=ap-southeast-3&resourceId="+e.instance(), "admin-token", "")
	if w.Code != http.StatusOK {
		t.Fatalf("read settings: %d %s", w.Code, w.Body)
	}
	var cur cloud.InstanceConfig
	_ = json.Unmarshal(w.Body.Bytes(), &cur)
	if cur.StopProtect || cur.RequireTokens || cur.CreditMode != "" {
		t.Fatalf("settings now = %+v", cur)
	}

	// m6i.xlarge has no credit mode; nothing at all is no change.
	e.ask(t, "admin-token", e.body("settings", `,"spec":{"creditMode":"unlimited"}`), http.StatusBadRequest)
	e.ask(t, "admin-token", e.body("settings", `,"spec":{"stopProtect":false}`), http.StatusBadRequest)

	c := e.ask(t, "admin-token", e.body("settings", `,"spec":{"stopProtect":true,"requireTokens":true}`), http.StatusAccepted)
	if c.State != "awaiting_approval" || c.Level != cloud.LevelChange {
		t.Fatalf("on Production a settings change waits: %+v", c)
	}
	if !strings.Contains(c.Summary, "stop protection") || !strings.Contains(c.Summary, "IMDSv2") {
		t.Fatalf("summary = %q", c.Summary)
	}
	if w := do(t, e.h, http.MethodPost, "/api/v1/cloud/changes/"+c.ID+"/approve", "second-token", ""); w.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", w.Code, w.Body)
	}
	got, err := e.s.CloudChange(context.Background(), c.ID)
	if err != nil || got.State != "applied" {
		t.Fatalf("after approval: %+v %v", got, err)
	}
	ran := false
	for _, call := range e.fake.CallLog() {
		ran = ran || call == "configure "+e.instance()+" "+e.admin
	}
	if !ran {
		t.Fatalf("settings must run as the person who asked: %v", e.fake.CallLog())
	}
	cfg := e.fake.Configs[e.instance()]
	if !cfg.StopProtect || !cfg.RequireTokens {
		t.Fatalf("fake settings = %+v", cfg)
	}

	// Stop protection holds: the stop is asked for, and AWS refuses it.
	stop := e.ask(t, "admin-token", e.body("stop", ""), http.StatusOK)
	if stop.State != "failed" || !strings.Contains(stop.Error, "stop protection") {
		t.Fatalf("stop with stop protection on: %+v", stop)
	}
	// It was not launched to hibernate, so Argus does not offer it.
	e.ask(t, "admin-token", e.body("hibernate", ""), http.StatusConflict)
}
