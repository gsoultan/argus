package agent

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

type fakeAsker struct {
	inUse    []tunnel.Logon
	allowed  bool
	answered bool
	asked    []string
	told     []string
}

func (f *fakeAsker) InUse(context.Context) ([]tunnel.Logon, error) { return f.inUse, nil }
func (f *fakeAsker) Ask(_ context.Context, l tunnel.Logon, _, text string, _ int) (bool, bool, error) {
	f.asked = append(f.asked, l.Account+": "+text)
	return f.allowed, f.answered, nil
}
func (f *fakeAsker) Tell(_ context.Context, l tunnel.Logon, _, text string) error {
	f.told = append(f.told, l.Account+": "+text)
	return nil
}

func TestAskAtMachine(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sam := []tunnel.Logon{{Session: 1, Account: `PC\sam`, State: tunnel.LogonActive, Console: true}}
	ask := &tunnel.GrantAsk{Mode: tunnel.AskFirst, Seconds: 60, Who: "Ada Lovelace"}
	grant := func(a *tunnel.GrantAsk) tunnel.Grant { return tunnel.Grant{Email: "ada@example.com", Ask: a} }

	// A server's grant asks nobody.
	f := &fakeAsker{inUse: sam}
	if err := askAtMachine(context.Background(), f, grant(nil), "x", log); err != nil || len(f.asked) != 0 {
		t.Fatalf("no ask: %v %v", err, f.asked)
	}
	// Nobody there: opens without asking.
	f = &fakeAsker{}
	if err := askAtMachine(context.Background(), f, grant(ask), "x", log); err != nil || len(f.asked) != 0 {
		t.Fatalf("nobody there: %v %v", err, f.asked)
	}
	// Allowed.
	f = &fakeAsker{inUse: sam, allowed: true, answered: true}
	if err := askAtMachine(context.Background(), f, grant(ask), "open a terminal as ops", log); err != nil {
		t.Fatalf("allowed: %v", err)
	}
	if len(f.asked) != 1 || !strings.Contains(f.asked[0], "Ada Lovelace wants to open a terminal as ops") ||
		!strings.Contains(f.asked[0], "60 seconds") {
		t.Fatalf("question: %v", f.asked)
	}
	// Said no, and no answer: refused, with the prefix the control plane reads.
	f = &fakeAsker{inUse: sam, answered: true}
	err := askAtMachine(context.Background(), f, grant(ask), "x", log)
	if err == nil || !strings.HasPrefix(err.Error(), tunnel.AskRefusedPrefix) || !strings.Contains(err.Error(), "sam said no") {
		t.Fatalf("declined: %v", err)
	}
	f = &fakeAsker{inUse: sam}
	err = askAtMachine(context.Background(), f, grant(ask), "x", log)
	if err == nil || !strings.Contains(err.Error(), "did not answer in 60 seconds") {
		t.Fatalf("no answer: %v", err)
	}
	// Tell: opens at once, everyone there is told.
	f = &fakeAsker{inUse: append(sam, tunnel.Logon{Account: "bo", State: tunnel.LogonActive})}
	tell := &tunnel.GrantAsk{Mode: tunnel.TellOnly, Who: "Ada Lovelace"}
	if err := askAtMachine(context.Background(), f, grant(tell), "open a terminal as ops", log); err != nil ||
		len(f.told) != 2 || len(f.asked) != 0 {
		t.Fatalf("tell: %v %v %v", err, f.told, f.asked)
	}
	// A question the console never sends is refused.
	bad := &tunnel.GrantAsk{Mode: tunnel.AskFirst, Seconds: 5, Who: "x"}
	if err := askAtMachine(context.Background(), &fakeAsker{inUse: sam}, grant(bad), "x", log); err == nil {
		t.Fatal("a 5 second question was accepted")
	}
}
