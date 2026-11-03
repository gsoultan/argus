package main

import (
	"testing"

	"github.com/gsoultan/argus/internal/control"
	"gopkg.in/yaml.v3"
)

// A config file that never mentioned sign-in alerts gets the defaults: both
// audiences, new devices only.
func TestSignInAlertsDefaultOn(t *testing.T) {
	var c config
	if err := yaml.Unmarshal([]byte("alerts:\n  telegram:\n    bot_token: x\n    chat_ids: ['1']\n"), &c); err != nil {
		t.Fatal(err)
	}
	if p := signInConfigOf(c.Alerts).policy(); p != control.DefaultSignInAlertPolicy {
		t.Fatalf("policy = %+v", p)
	}
	if p := signInConfigOf(nil).policy(); p != control.DefaultSignInAlertPolicy {
		t.Fatalf("no alerts block: %+v", p)
	}
}

func TestSignInAlertsReadFromTheFile(t *testing.T) {
	var c config
	if err := yaml.Unmarshal([]byte("alerts:\n  sign_in:\n    person: false\n    when: every\n"), &c); err != nil {
		t.Fatal(err)
	}
	want := control.SignInAlertPolicy{Admins: true, Person: false, Every: true}
	if p := signInConfigOf(c.Alerts).policy(); p != want {
		t.Fatalf("policy = %+v, want %+v", p, want)
	}
}

// Saving from Settings writes only what differs from the defaults, and a save
// from a console that does not know about sign-in alerts keeps what is set.
func TestSavingSettingsKeepsSignInAlerts(t *testing.T) {
	if got := signInFromSettings(control.SignInAlertSettings{Admins: true, Person: true, When: "new_device"}); got != nil {
		t.Fatalf("defaults written out: %+v", got)
	}
	off := false
	c := &config{Alerts: &alertsConfig{SignIn: &signInAlertsConfig{Person: &off}}}
	if err := applyAlerts(c, control.Settings{}, nil); err != nil {
		t.Fatal(err)
	}
	if c.Alerts == nil || c.Alerts.SignIn == nil || c.Alerts.SignIn.Person == nil || *c.Alerts.SignIn.Person {
		t.Fatalf("an older console's save dropped the sign-in settings: %+v", c.Alerts)
	}
	if err := applyAlerts(c, control.Settings{SignInAlerts: &control.SignInAlertSettings{
		Admins: false, Person: true, When: "every"}}, nil); err != nil {
		t.Fatal(err)
	}
	if p := signInConfigOf(c.Alerts).policy(); p != (control.SignInAlertPolicy{Admins: false, Person: true, Every: true}) {
		t.Fatalf("policy after save = %+v", p)
	}
}

// Location and server alerts follow the same rule: defaults are not written,
// an older console's save keeps what is set, and a bad value is refused.
func TestSavingSettingsKeepsLocationAndServerAlerts(t *testing.T) {
	c := &config{Alerts: &alertsConfig{ServerConnected: "every",
		SignIn: &signInAlertsConfig{Location: "lookup"}}}
	if err := applyAlerts(c, control.Settings{SignInAlerts: &control.SignInAlertSettings{
		Admins: true, Person: true, When: "new_device"}}, nil); err != nil {
		t.Fatal(err)
	}
	if c.Alerts == nil || c.Alerts.ServerConnected != "every" || signInConfigOf(c.Alerts).locationMode() != control.LocationLookup {
		t.Fatalf("an older console's save dropped them: %+v", c.Alerts)
	}
	if err := applyAlerts(c, control.Settings{
		SignInAlerts: &control.SignInAlertSettings{Admins: true, Person: true, When: "new_device", Location: "proxy"},
		ServerAlerts: &control.ServerAlertSettings{Connected: "first"}}, nil); err != nil {
		t.Fatal(err)
	}
	if c.Alerts != nil {
		t.Fatalf("defaults written out: %+v", c.Alerts)
	}
	if err := applyAlerts(c, control.Settings{ServerAlerts: &control.ServerAlertSettings{Connected: "sometimes"}}, nil); err == nil {
		t.Fatal("an unknown server alert setting was accepted")
	}
}

// The security section writes the two-step policy and the per-account limits,
// and a save without it keeps the limits a save from the limits card used to
// drop.
func TestSavingSecuritySettings(t *testing.T) {
	b := &settingsBackend{}
	c := &config{RateLimit: &rateLimitConfig{AccountFailuresPerHour: 7}}
	if err := b.apply(t.Context(), c, control.Settings{Database: control.DatabaseSettings{Raw: true}}, nil); err != nil {
		t.Fatal(err)
	}
	if c.RateLimit.AccountFailuresPerHour != 7 {
		t.Fatalf("a save without the security section dropped the account limit: %+v", c.RateLimit)
	}
	in := control.Settings{Database: control.DatabaseSettings{Raw: true}, Security: &control.SecuritySettings{
		RequireMFA: "privileged", TemporaryPasswordTTL: "24h", AccountFailuresPerHour: 3}}
	if err := b.apply(t.Context(), c, in, nil); err != nil {
		t.Fatal(err)
	}
	if c.RequireMFA != "privileged" || c.TemporaryPasswordTTL.String() != "24h0m0s" || c.RateLimit.AccountFailuresPerHour != 3 {
		t.Fatalf("security not applied: mfa %q ttl %v limit %d", c.RequireMFA, c.TemporaryPasswordTTL, c.RateLimit.AccountFailuresPerHour)
	}
	in.Security.RequireMFA = "all"
	if err := b.apply(t.Context(), c, in, nil); err != nil {
		t.Fatal(err)
	}
	if c.RequireMFA != "" {
		t.Fatalf("the default was written out: %q", c.RequireMFA)
	}
	in.Security.RequireMFA = "sometimes"
	if err := b.apply(t.Context(), c, in, nil); err == nil {
		t.Fatal("an unknown policy was accepted")
	}
}
