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
