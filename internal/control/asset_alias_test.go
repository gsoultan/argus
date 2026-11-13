package control

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

/*
An asset's alias is the name people read; the hostname stays the name Argus,
its agent and a certificate go by. The hostname has to be a DNS name, so
"Payroll database" or "Front desk PC" could not be given to an asset at all.

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable \
	  go test ./internal/control/ -run Alias
*/

func TestAnAliasIsNormalisedAndBounded(t *testing.T) {
	for _, tc := range []struct {
		alias, want, refused string
	}{
		{"  Payroll   database ", "Payroll database", ""},
		{"Kasir lantai 2 — Café", "Kasir lantai 2 — Café", ""},
		{"", "", ""},
		// The same name twice says nothing: the hostname is shown anyway.
		{"PAY-01", "", ""},
		{strings.Repeat("x", maxAlias), strings.Repeat("x", maxAlias), ""},
		{strings.Repeat("x", maxAlias+1), "", "at most"},
		{"pay\u0007db", "", "control characters"},
	} {
		got, err := AssetInput{Hostname: "pay-01", Alias: tc.alias, Principals: []string{"ops"}}.normalise()
		if tc.refused != "" {
			if err == nil || !strings.Contains(err.Error(), tc.refused) {
				t.Errorf("alias %q: err %v, want one saying %q", tc.alias, err, tc.refused)
			}
			continue
		}
		if err != nil {
			t.Errorf("alias %q refused: %v", tc.alias, err)
			continue
		}
		if got.Alias != tc.want {
			t.Errorf("alias %q stored as %q, want %q", tc.alias, got.Alias, tc.want)
		}
	}
}

// The edit form sends an asset back as it read it, and every server and
// workstation reads back with control mode "attended". Saving an alias, or any
// other field, must not be refused for a value the control plane stored itself.
func TestAnEditSendingTheStoredControlModeIsAccepted(t *testing.T) {
	for _, kind := range []string{"server", "workstation"} {
		in := AssetInput{Hostname: "pay-01", Kind: kind, Alias: "Payroll database",
			Principals: []string{"ops"}, ControlMode: "attended"}
		got, err := in.normalise()
		if err != nil {
			t.Errorf("%s with its stored control mode refused: %v", kind, err)
			continue
		}
		if got.ControlMode != "attended" || got.ControlNeedsApproval || got.Alias != "Payroll database" {
			t.Errorf("%s normalised to %+v", kind, got)
		}
	}
}

func TestAnAliasIsSavedWithoutRenamingTheServer(t *testing.T) {
	e := newTunnelEnv(t)
	ctx := context.Background()

	host := unique("alias") + ".corp.example"
	asset, err := e.store.CreateAsset(ctx, AssetInput{
		Hostname: host, Address: "192.0.2.40", Principals: []string{"ops"}, Alias: "Front desk PC",
	})
	if err != nil {
		t.Fatal(err)
	}
	if asset.Alias != "Front desk PC" {
		t.Fatalf("alias not stored on create: %q", asset.Alias)
	}

	in := AssetInput{
		Hostname: asset.Hostname, Address: asset.Address, Port: asset.Port, Protocol: asset.Protocol,
		Tags: asset.Tags, GroupName: asset.GroupName, Principals: asset.Principals,
		CredentialMode: asset.CredentialMode, Kind: asset.Kind, Alias: "Payroll database",
	}
	body, _ := json.Marshal(in)
	w := do(t, e.h, http.MethodPatch, "/api/v1/assets/"+asset.ID, "admin-token", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	var got Asset
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Alias != "Payroll database" || got.Hostname != host || got.AgentHostname != "" {
		t.Fatalf("an alias changed more than the label: %+v", got)
	}

	// The audit entry names the server by its hostname and says both labels,
	// so the history can be found under either.
	var target, detail string
	if err := e.store.pool.QueryRow(ctx, `
		SELECT target, detail FROM audit_events
		 WHERE action = 'asset.updated' AND target = $1 ORDER BY seq DESC LIMIT 1`, host).
		Scan(&target, &detail); err != nil {
		t.Fatalf("reading the audit chain: %v", err)
	}
	if !strings.Contains(detail, `"Payroll database"`) || !strings.Contains(detail, `"Front desk PC"`) {
		t.Errorf("audit detail does not say the alias change: %q", detail)
	}
	if strings.Contains(detail, "Renamed") {
		t.Errorf("an alias change was recorded as a rename: %q", detail)
	}
}
