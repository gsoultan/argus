package notify

import (
	"strings"
	"testing"
	"time"
)

// The welcome email carries the password once, in the body, and says what
// to do with it; the subject never has it.
func TestWelcomeMessage(t *testing.T) {
	m := WelcomeMessage(Welcome{Email: "rani@northwind.id", Name: "Rani Putri", Role: "operator",
		Password: "3U=pLgr@=Pzdh%LPa6", ExpiresAt: time.Date(2026, 11, 6, 3, 0, 0, 0, time.UTC),
		By: "maya@northwind.id", MFARequired: true}, "https://argus.northwind.id")
	if strings.Contains(m.Subject, "3U=") {
		t.Fatal("the password is in the subject")
	}
	for _, want := range []string{"Hi Rani", "as a member", "Temporary password: 3U=pLgr@=Pzdh%LPa6",
		"Fri 6 Nov 2026, 03:00 UTC", "authenticator app", "https://argus.northwind.id/"} {
		if !strings.Contains(m.Text, want) {
			t.Errorf("text lacks %q:\n%s", want, m.Text)
		}
	}
	if !strings.Contains(m.HTML, "3U=pLgr@=Pzdh%LPa6") {
		t.Error("the HTML part lacks the password")
	}
	r := WelcomeMessage(Welcome{Email: "a@b.c", Password: "x", Reset: true}, "")
	if !strings.Contains(r.Subject, "new temporary password") || strings.Contains(r.Text, "authenticator app") {
		t.Errorf("reset message:\n%s\n%s", r.Subject, r.Text)
	}
}

func TestServerConnectedMessage(t *testing.T) {
	m := ServerConnectedMessage(ServerConnected{Host: "db-01", First: true, Transport: "quic",
		RemoteAddr: "203.0.113.9", At: time.Now()}, "https://argus.example")
	if m.Subject != "Argus: db-01 connected to Argus" || !strings.Contains(m.Text, "203.0.113.9") ||
		!strings.Contains(m.Telegram, "db-01") {
		t.Fatalf("%s\n%s", m.Subject, m.Text)
	}
	again := ServerConnectedMessage(ServerConnected{Host: "db-01", At: time.Now()}, "")
	if !strings.Contains(again.Subject, "connected again") {
		t.Fatal(again.Subject)
	}
}

// A sign-in alert names the place next to the address, and says it is a guess.
func TestSignInNamesTheLocation(t *testing.T) {
	m := SignInPersonMessage(SignIn{Email: "a@b.c", ClientIP: "203.0.113.7", Location: "Jakarta, ID",
		At: time.Now()}, "", "")
	if !strings.Contains(m.Text, "203.0.113.7 (Jakarta, ID, approximate)") {
		t.Fatal(m.Text)
	}
}
