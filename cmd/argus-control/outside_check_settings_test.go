package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/control"
)

// The check-in address is the service's secret: it goes to a file of its
// own, a save that leaves it out keeps it, and an empty one removes it.
func TestTheOutsideCheckAddressIsASecretAndCanBeRemoved(t *testing.T) {
	st := &secretStage{dir: t.TempDir()}
	c := &config{}
	in := control.Settings{OutsideCheck: &control.OutsideCheckSettings{Every: "5m",
		URL: ptr(" https://hc-ping.com/6f0c-secret ")}}
	if err := applyAlerts(c, in, st); err != nil {
		t.Fatal(err)
	}
	if c.Alerts == nil || c.Alerts.OutsideCheck == nil {
		t.Fatalf("not saved: %+v", c.Alerts)
	}
	ref := c.Alerts.OutsideCheck.URL
	if !strings.HasPrefix(ref, "${file:") || strings.Contains(ref, "secret") {
		t.Fatalf("address reference = %q", ref)
	}
	body, err := os.ReadFile(strings.TrimSuffix(strings.TrimPrefix(ref, "${file:"), "}"))
	if err != nil || strings.TrimSpace(string(body)) != "https://hc-ping.com/6f0c-secret" {
		t.Fatalf("address file = %q, %v", body, err)
	}
	if c.Alerts.OutsideCheck.Every != "5m" {
		t.Errorf("every = %q", c.Alerts.OutsideCheck.Every)
	}

	// Left out: kept. Back to one minute, which is the default and not written.
	in.OutsideCheck = &control.OutsideCheckSettings{Every: "1m"}
	if err := applyAlerts(c, in, st); err != nil {
		t.Fatal(err)
	}
	if c.Alerts.OutsideCheck == nil || c.Alerts.OutsideCheck.URL != ref || c.Alerts.OutsideCheck.Every != "" {
		t.Fatalf("kept: %+v", c.Alerts.OutsideCheck)
	}

	// A console older than the setting sends none: kept as configured.
	if err := applyAlerts(c, control.Settings{}, st); err != nil {
		t.Fatal(err)
	}
	if c.Alerts == nil || c.Alerts.OutsideCheck == nil {
		t.Fatal("an older console's save removed the check-in")
	}

	// Empty: removed, and with nothing else set, no alerts block at all.
	in.OutsideCheck = &control.OutsideCheckSettings{Every: "1m", URL: ptr("")}
	if err := applyAlerts(c, in, st); err != nil {
		t.Fatal(err)
	}
	if c.Alerts != nil {
		t.Fatalf("not removed: %+v", c.Alerts)
	}
}

func TestABadCheckInAddressIsRefused(t *testing.T) {
	for _, bad := range []string{"ftp://x/y", "https://u:p@x/y"} {
		in := control.Settings{OutsideCheck: &control.OutsideCheckSettings{Every: "1m", URL: ptr(bad)}}
		if err := applyAlerts(&config{}, in, &secretStage{dir: t.TempDir()}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	in := control.Settings{OutsideCheck: &control.OutsideCheckSettings{Every: "2m", URL: ptr("https://x/y")}}
	if err := applyAlerts(&config{}, in, &secretStage{dir: t.TempDir()}); err == nil {
		t.Error("every 2m accepted")
	}
}

// An older control.yaml has no outside_check, and starts with it off.
func TestAnOlderConfigHasNoOutsideCheck(t *testing.T) {
	var c *alertsConfig
	o, err := c.outsideCheck()
	if err != nil || o.URL != "" {
		t.Fatalf("nil alerts: %+v, %v", o, err)
	}
	o, err = (&alertsConfig{OutsideCheck: &outsideCheckConfig{URL: "https://hc-ping.com/x\n", Every: "5m"}}).outsideCheck()
	if err != nil || o.URL != "https://hc-ping.com/x" || o.Every != 5*time.Minute {
		t.Fatalf("set: %+v, %v", o, err)
	}
}
