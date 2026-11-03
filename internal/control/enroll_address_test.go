package control

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/tlsconfig"
)

/*
The pin an install command carries has to be the key curl meets at the address
the command uses. These hold the cases: the address is this process, the
address is a proxy with a key of its own (a Cloudflare Tunnel), and the address
cannot be reached from here.
*/

const ownPin = "sha256:" + "11111111111111111111111111111111" + "11111111111111111111111111111111"

func pinAPI(fleetURL string, probe func(context.Context, string) (PinProbeResult, error)) *API {
	return &API{
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Enroll:   EnrollTrust{Pin: func() (string, error) { return ownPin, nil }},
		FleetURL: fleetURL,
		PinProbe: probe,
	}
}

func TestThePinIsHandedOutWhenTheAddressPresentsThisKey(t *testing.T) {
	var dialed string
	a := pinAPI("https://argus-internal.example.com:8080", func(_ context.Context, hp string) (PinProbeResult, error) {
		dialed = hp
		return PinProbeResult{Pin: ownPin}, nil
	})
	r := httptest.NewRequest("POST", "https://argus.example.com/api/v1/agents/enroll-tokens", nil)
	pin, _, problem := a.enrolPin(r)
	if pin != ownPin || problem != "" {
		t.Fatalf("pin %q problem %q", pin, problem)
	}
	if dialed != "argus-internal.example.com:8080" {
		t.Fatalf("probed %q, want the address servers use", dialed)
	}
}

func TestAProxyInFrontMeansNoPinAndAReason(t *testing.T) {
	var dialed string
	a := pinAPI("", func(_ context.Context, hp string) (PinProbeResult, error) {
		dialed = hp
		return PinProbeResult{Pin: "sha256:" + strings.Repeat("ab", 32)}, nil // a proxy's own key
	})
	r := httptest.NewRequest("POST", "https://argus.example.com/api/v1/agents/enroll-tokens", nil)
	pin, address, problem := a.enrolPin(r)
	if pin != "" {
		t.Fatalf("handed out pin %q that curl would refuse", pin)
	}
	if dialed != "argus.example.com:443" || address != "https://argus.example.com" {
		t.Fatalf("probed %q for %q, want the console's own address", dialed, address)
	}
	if !strings.Contains(problem, "Address servers use") {
		t.Fatalf("problem does not say what to set: %q", problem)
	}
}

// Cloudflare in front: its certificate is not ours, and a public CA signed
// it. Ordinary verification is the check, so no pin and no complaint.
func TestAPublicCertificateInFrontMeansNoPinAndNoProblem(t *testing.T) {
	a := pinAPI("", func(context.Context, string) (PinProbeResult, error) {
		return PinProbeResult{Pin: "sha256:" + strings.Repeat("cd", 32), Public: true}, nil
	})
	r := httptest.NewRequest("POST", "https://argus.example.com/api/v1/agents/enroll-tokens", nil)
	if pin, _, problem := a.enrolPin(r); pin != "" || problem != "" {
		t.Fatalf("pin %q problem %q, want neither", pin, problem)
	}
}

func TestAnUnreachableAddressKeepsThePin(t *testing.T) {
	a := pinAPI("", func(context.Context, string) (PinProbeResult, error) {
		return PinProbeResult{}, errors.New("connection refused")
	})
	r := httptest.NewRequest("POST", "https://argus.example.com/api/v1/agents/enroll-tokens", nil)
	if pin, _, problem := a.enrolPin(r); pin != ownPin || problem != "" {
		t.Fatalf("pin %q problem %q", pin, problem)
	}
}

func TestNoPinOfOurOwnMeansNoProbe(t *testing.T) {
	a := pinAPI("", func(context.Context, string) (PinProbeResult, error) {
		t.Fatal("probed with nothing to compare")
		return PinProbeResult{}, nil
	})
	a.Enroll.Pin = nil // behind a TLS proxy
	r := httptest.NewRequest("POST", "https://argus.example.com/api/v1/agents/enroll-tokens", nil)
	if pin, _, problem := a.enrolPin(r); pin != "" || problem != "" {
		t.Fatalf("pin %q problem %q", pin, problem)
	}
}

// The real probe, against a real TLS server: it reports the key it was shown.
func TestTheProbeReadsThePresentedKey(t *testing.T) {
	srv := httptest.NewTLSServer(nil)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	got, err := probeServerPin(context.Background(), u.Host)
	if err != nil {
		t.Fatal(err)
	}
	if want := tlsconfig.Pin(srv.Certificate()); got.Pin != want {
		t.Fatalf("probe saw %s, server's key is %s", got.Pin, want)
	}
	if got.Public {
		t.Fatal("httptest's own certificate was reported as publicly trusted")
	}
}
