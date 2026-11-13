//go:build linux

package agent

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxScreenFindsXServersAndNotXwayland(t *testing.T) {
	for _, tc := range []struct {
		args []string
		ok   bool
		want xServer
	}{
		{[]string{"/usr/lib/xorg/Xorg", "vt2", "-displayfd", "3", "-auth", "/run/user/1000/gdm/Xauthority",
			"-nolisten", "tcp", "-background", "none", "-noreset", "-keeptty", "-novtswitch", "-verbose", "3"},
			true, xServer{display: ":0", auth: "/run/user/1000/gdm/Xauthority", vt: "2"}},
		{[]string{"/usr/lib/xorg/Xorg", ":1", "-seat", "seat0", "-auth", "/var/run/lightdm/root/:1", "vt8"},
			true, xServer{display: ":1", auth: "/var/run/lightdm/root/:1", vt: "8"}},
		{[]string{"Xvfb", ":99", "-screen", "0", "1280x720x24"}, true, xServer{display: ":99"}},
		{[]string{"/usr/bin/Xwayland", ":0", "-rootless"}, false, xServer{}},
		{[]string{"/usr/bin/bash"}, false, xServer{}},
	} {
		got, ok := parseXServer(tc.args)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("%v: got %+v %v, want %+v %v", tc.args, got, ok, tc.want, tc.ok)
		}
	}
}

func TestLinuxScreenReadsTheCookieForItsDisplay(t *testing.T) {
	dir := t.TempDir()
	one, two := make([]byte, 16), make([]byte, 16)
	_, _ = rand.Read(one)
	_, _ = rand.Read(two)
	path := filepath.Join(dir, "auth")
	data := append(xauthEntry("0", one), xauthEntry("1", two)...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := x11Cookie(path, ":1.0"); err != nil || got != hex.EncodeToString(two) {
		t.Fatalf("display :1 got %s, %v", got, err)
	}
	if got, err := x11Cookie(path, ":0"); err != nil || got != hex.EncodeToString(one) {
		t.Fatalf("display :0 got %s, %v", got, err)
	}
	if err := os.WriteFile(path, data[:10], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := x11Cookie(path, ":0"); err == nil {
		t.Fatal("a truncated authority file gave a cookie")
	}
}

// The console is the active session on seat0; a Wayland one is found, so
// that it can be refused with a reason, and a remote one is not the
// console.
func TestLinuxScreenFindsTheActiveSessionOnTheSeat(t *testing.T) {
	dir := t.TempDir()
	runDir, proc := filepath.Join(dir, "sessions"), filepath.Join(dir, "proc")
	for _, d := range []string{runDir, filepath.Join(proc, "812")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(proc, "812", "cmdline"),
		strings.Join([]string{"/usr/lib/xorg/Xorg", "vt2", "-displayfd", "3", "-auth", "/run/user/0/gdm/Xauthority"}, "\x00")+"\x00")
	write(filepath.Join(runDir, "7"), "UID=0\nUSER=root\nACTIVE=1\nREMOTE=1\nTYPE=tty\n")
	write(filepath.Join(runDir, "c1"), "UID=0\nUSER=gdm\nACTIVE=0\nSEAT=seat0\nTYPE=x11\nCLASS=greeter\nVTNR=1\n")
	write(filepath.Join(runDir, "2"), "UID=0\nUSER=root\nACTIVE=1\nSEAT=seat0\nTYPE=x11\nCLASS=user\nVTNR=2\n")
	write(filepath.Join(runDir, "2.ref"), "")
	h := linuxScreenHost{runDir: runDir, proc: proc}
	c, ok := h.console()
	if !ok {
		t.Fatal("no console found")
	}
	if c.id != "2" || c.user != "root" || c.display != ":0" || c.auth != "/run/user/0/gdm/Xauthority" || c.wayland {
		t.Fatalf("console %+v", c)
	}
	if h.ConsoleAccount(0) != "root" {
		t.Fatalf("account %q", h.ConsoleAccount(0))
	}
	first, _ := h.ConsoleSession()

	// Signed out: the sign-in screen is the console, nobody's account, and
	// a different session to follow.
	write(filepath.Join(runDir, "2"), "UID=0\nUSER=root\nACTIVE=0\nSEAT=seat0\nTYPE=x11\nVTNR=2\n")
	write(filepath.Join(runDir, "c1"), "UID=0\nUSER=gdm\nACTIVE=1\nSEAT=seat0\nTYPE=x11\nCLASS=greeter\nVTNR=2\n")
	if a := h.ConsoleAccount(0); a != "" {
		t.Fatalf("the sign-in screen's account is %q, want none", a)
	}
	if next, _ := h.ConsoleSession(); next == first {
		t.Fatal("the console moving to the sign-in screen did not change the session")
	}

	write(filepath.Join(runDir, "c1"), "UID=0\nUSER=alice\nACTIVE=1\nSEAT=seat0\nTYPE=wayland\nVTNR=2\n")
	c, _ = h.console()
	if !c.wayland {
		t.Fatal("a Wayland session was not seen as one")
	}
	if _, err := h.StartHelper(0, helperMode{}); err == nil || !strings.Contains(err.Error(), "Wayland") {
		t.Fatalf("a Wayland console started a helper: %v", err)
	}
}
