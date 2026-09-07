package gateway

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/reporter"
)

/*
An elevated principal needs an approval behind it.

The console has always said so, and the browser terminal has always enforced
it. This path -- the one a product that leads with "Linux SSH first" is
actually used through -- enforced nothing. It checked authorized_keys and the
asset's principal list, then dialled the target as root; where a key is
injected for root, which is the entire point of credential injection, that
session opened.

Being listed in an asset's principals is permission to ask, not permission to
have.
*/

func elevationServer(t *testing.T, pol Policy) *Server {
	t.Helper()
	srv := testServer(t, t.TempDir())
	srv.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	h := &PolicyHolder{}
	h.Set(pol)
	srv.cfg.Policy = h
	return srv
}

// With no control plane there is nothing to consult, so root is refused.
func TestElevatedSessionRefusedWithNoControlPlane(t *testing.T) {
	srv := elevationServer(t, DefaultPolicy())

	err := srv.authorizeElevated("lin@northwind.id", "root", "pay-01")
	if err == nil {
		t.Fatal("root opened with no approval and no control plane to check one")
	}
	if !strings.Contains(err.Error(), "approved access request") {
		t.Errorf("the refusal should say what is needed: %v", err)
	}
}

// Ordinary principals are unaffected: they need no approval, so an outage must
// not stop anyone doing their job.
func TestOrdinaryPrincipalsNeedNoApproval(t *testing.T) {
	srv := elevationServer(t, DefaultPolicy())
	for _, principal := range []string{"ops", "deploy", "webapp"} {
		if err := srv.authorizeElevated("lin@northwind.id", principal, "pay-01"); err != nil {
			t.Errorf("%q was refused: %v", principal, err)
		}
	}
}

// The list is case-insensitive. Administrator and administrator are the same
// Windows account, and a check that told them apart would be one spelling away
// from nothing at all.
func TestElevationIsCaseInsensitive(t *testing.T) {
	srv := elevationServer(t, DefaultPolicy())
	for _, principal := range []string{"root", "ROOT", "Root", "Administrator", "administrator", "AdMiN"} {
		if err := srv.authorizeElevated("lin@northwind.id", principal, "win-01"); err == nil {
			t.Errorf("%q was treated as ordinary", principal)
		}
	}
}

// The list comes from the control plane, so a deployment can name its own.
func TestTheElevatedListComesFromPolicy(t *testing.T) {
	pol := DefaultPolicy()
	pol.ElevatedPrincipals = []string{"dbadmin"}
	srv := elevationServer(t, pol)

	if err := srv.authorizeElevated("lin@northwind.id", "dbadmin", "pay-01"); err == nil {
		t.Error("a principal the policy calls elevated was let through")
	}
	// And root is no longer elevated for this deployment, because the control
	// plane said so. Surprising, but it is the control plane's call to make
	// and the gateway must not hold a second opinion.
	if err := srv.authorizeElevated("lin@northwind.id", "root", "pay-01"); err != nil {
		t.Errorf("root was refused though the policy does not list it: %v", err)
	}
}

// A policy that arrives with no list at all must not mean "nothing is
// elevated" -- that would remove the approval requirement during an upgrade
// from a control plane too old to send it.
func TestAnAbsentListFallsBackToTheDefault(t *testing.T) {
	got := policyFromWire(reporter.GatewayPolicy{ProxySftpSubsystem: true})
	if len(got.ElevatedPrincipals) == 0 {
		t.Fatal("an absent list became an empty one; nothing would be elevated")
	}
	if !got.IsElevated("root") {
		t.Error("root is not elevated after falling back to the default")
	}
}

// Equal has to notice a change in the list, or the sync would never apply one.
func TestPolicyEqualNoticesTheList(t *testing.T) {
	a := DefaultPolicy()
	b := DefaultPolicy()
	if !a.Equal(b) {
		t.Fatal("two default policies compared unequal")
	}
	b.ElevatedPrincipals = append([]string{"dbadmin"}, b.ElevatedPrincipals...)
	if a.Equal(b) {
		t.Error("a changed elevated list compared equal; the sync would ignore it")
	}
}
