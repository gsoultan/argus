package main

import (
	"os"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/control"
	"github.com/gsoultan/argus/internal/notify"
)

func ptr(s string) *string { return &s }

// The API key goes to a file of its own, never into the config, and a save
// that leaves it out keeps the one configured.
func TestPanmailKeyIsWrittenToAFileAndKept(t *testing.T) {
	st := &secretStage{dir: t.TempDir()}
	in := control.EmailSettings{Enabled: true, Via: notify.ViaPanmail, From: "argus@x.io", To: []string{"sec@x.io"},
		Panmail: control.PanmailSettings{URL: " https://mail.example.com ", ProviderID: "prov-1", APIKey: ptr("pm_SECRET")}}
	got, err := emailConfigFrom(in, nil, st)
	if err != nil {
		t.Fatal(err)
	}
	if got.Via != notify.ViaPanmail || got.Panmail == nil || got.Panmail.URL != "https://mail.example.com" ||
		got.Panmail.ProviderID != "prov-1" {
		t.Fatalf("config = %+v", got)
	}
	ref := got.Panmail.APIKey
	if !strings.HasPrefix(ref, "${file:") || strings.Contains(ref, "pm_SECRET") {
		t.Fatalf("api key reference = %q", ref)
	}
	body, err := os.ReadFile(strings.TrimSuffix(strings.TrimPrefix(ref, "${file:"), "}"))
	if err != nil || strings.TrimSpace(string(body)) != "pm_SECRET" {
		t.Fatalf("key file = %q, %v", body, err)
	}

	in.Panmail.APIKey = nil
	again, err := emailConfigFrom(in, got, st)
	if err != nil {
		t.Fatal(err)
	}
	if again.Panmail.APIKey != ref {
		t.Fatalf("key not kept: %q", again.Panmail.APIKey)
	}
}

// Switching to Panmail and back does not lose the SMTP password, and SMTP,
// the default, is not written to the file.
func TestSwitchingDeliveryKeepsTheOtherSettings(t *testing.T) {
	prev := &notify.EmailConfig{Host: "smtp.x.io", Port: 587, Username: "u", Password: "${file:/s/smtp-password}",
		From: "argus@x.io", To: []string{"sec@x.io"}}
	in := control.EmailSettings{Enabled: true, Via: notify.ViaPanmail, Host: "smtp.x.io", Port: 587, Username: "u",
		From: "argus@x.io", To: []string{"sec@x.io"},
		Panmail: control.PanmailSettings{URL: "https://mail.example.com", ProviderID: "p"}}
	pm, err := emailConfigFrom(in, prev, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pm.Password != prev.Password || pm.Host != "smtp.x.io" {
		t.Fatalf("smtp settings lost: %+v", pm)
	}
	in.Via = notify.ViaSMTP
	back, err := emailConfigFrom(in, pm, nil)
	if err != nil {
		t.Fatal(err)
	}
	if back.Via != "" || back.Password != prev.Password || back.Panmail == nil || back.Panmail.URL == "" {
		t.Fatalf("after switching back: %+v", back)
	}
	if _, err := notify.NewEmail(*back); err != nil {
		t.Fatalf("smtp config no longer starts: %v", err)
	}
}
