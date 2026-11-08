package control

import (
	"context"
	"errors"
	"testing"
)

// A workstation is stored and opened exactly as a server is: the kind is
// the only difference, so every protocol path keeps working unchanged.
func TestAWorkstationNormalisesLikeAServer(t *testing.T) {
	for _, tc := range []AssetInput{
		{Hostname: "lt-rina", Kind: " Workstation "},
		{Hostname: "lt-rina", Kind: "workstation", Protocol: "rdp", Domain: "corp.example"},
	} {
		got, err := tc.normalise()
		if err != nil {
			t.Fatalf("%+v: %v", tc, err)
		}
		if got.Kind != "workstation" {
			t.Errorf("%+v: kind %q, want workstation", tc, got.Kind)
		}
		if got.ControlMode != "attended" || got.ControlNeedsApproval {
			t.Errorf("%+v: device settings leaked onto a workstation: %+v", tc, got)
		}
		if tc.Protocol == "rdp" && (got.Protocol != "rdp" || got.Domain != "corp.example") {
			t.Errorf("%+v: a Windows workstation lost how it opens: %+v", tc, got)
		}
	}
}

// Approving a discovered host as a workstation stores the kind the admin
// picked; a kind that is not a host's is refused before anything is written.
func TestEnrolAsWorkstation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	cleanHost(t, s, "disc-ws-01")
	seedDiscovered(t, s, "disc-ws-01")

	if _, err := s.EnrolHostAs(ctx, "android", "disc-ws-01", "admin@northwind.id", nil, nil, nil, false); !errors.As(err, new(ErrInvalidAsset)) {
		t.Fatalf("EnrolHostAs(android) = %v; a device must go through EnrolDevice", err)
	}
	id, err := s.EnrolHostAs(ctx, "workstation", "disc-ws-01", "admin@northwind.id", nil, nil, nil, false)
	if err != nil {
		t.Fatalf("EnrolHostAs: %v", err)
	}
	var kind string
	if err := s.pool.QueryRow(ctx, `SELECT kind FROM assets WHERE id = $1`, id).Scan(&kind); err != nil {
		t.Fatalf("read asset: %v", err)
	}
	if kind != "workstation" {
		t.Errorf("kind = %q, want workstation", kind)
	}
	// The database refuses a kind it does not know, whatever a caller sends.
	if _, err := s.pool.Exec(ctx, `UPDATE assets SET kind = 'ios' WHERE id = $1`, id); err == nil {
		t.Error("the kind check accepted 'ios'")
	}
}
