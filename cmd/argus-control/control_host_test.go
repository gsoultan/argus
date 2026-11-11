package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/control"
	"github.com/gsoultan/argus/internal/tlsconfig"
)

func controlHostPlanFor(t *testing.T, cfg config, pin func() (string, error)) (controlHostPlan, string) {
	t.Helper()
	was := controlHostSupport
	controlHostSupport = func() string { return "" }
	t.Cleanup(func() { controlHostSupport = was })
	loc := configLocation{Managed: filepath.Join(t.TempDir(), "control.yaml")}
	api := &control.API{}
	api.Enroll.Pin = pin
	return controlHostOptions(cfg, loc, api)
}

func ownPin() (string, error) { return "sha256:" + strings.Repeat("ab", 32), nil }

// Where argus-control serves and pins its own certificate, the helper uses
// the main listener.
func TestTheHelperUsesTheMainListenerWhenItCanPinIt(t *testing.T) {
	plan, why := controlHostPlanFor(t, config{Listen: ":8443", TLS: &tlsConfig{CertFile: "x"}}, ownPin)
	if why != "" || plan.Listen != "" || plan.opts.ControlURL != "https://127.0.0.1:8443" {
		t.Fatalf("plan %+v, why %q", plan, why)
	}
}

// Behind a proxy or tunnel (cloudflared), on plain HTTP, or where agents need
// a client certificate, the helper gets a loopback listener and pins its key.
func TestTheHelperGetsItsOwnListenerWhereTheMainOneWillNotDo(t *testing.T) {
	for name, c := range map[string]struct {
		cfg config
		pin func() (string, error)
	}{
		"plain HTTP":       {config{Listen: ":8080"}, nil},
		"behind_tls_proxy": {config{Listen: ":8443", TLS: &tlsConfig{CertFile: "x"}, BehindTLSProxy: true}, nil},
		"client_ca_file":   {config{Listen: ":8443", TLS: &tlsConfig{CertFile: "x", ClientCAFile: "ca"}}, ownPin},
	} {
		t.Run(name, func(t *testing.T) {
			plan, why := controlHostPlanFor(t, c.cfg, c.pin)
			if why != "" {
				t.Fatalf("refused: %s", why)
			}
			if plan.Listen != defaultControlHostListen || plan.opts.ControlURL != "https://127.0.0.1:8449" {
				t.Fatalf("plan %+v", plan)
			}
			got, err := plan.opts.Pin()
			want, _ := tlsconfig.PinFile(plan.CertFile)
			if err != nil || got != want || want == "" {
				t.Fatalf("pin %q, want the loopback key's %q (%v)", got, want, err)
			}
		})
	}
}

func TestTheHelperListenerIsLoopbackOnly(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8449", ":8449", "10.0.0.5:8449", "localhost"} {
		if _, why := controlHostPlanFor(t, config{Listen: ":8080", ControlHostListen: addr}, nil); why == "" {
			t.Errorf("%s was accepted for the helper's listener", addr)
		}
	}
	plan, why := controlHostPlanFor(t, config{Listen: ":8080", ControlHostListen: "[::1]:9000"}, nil)
	if why != "" || plan.opts.ControlURL != "https://[::1]:9000" {
		t.Fatalf("plan %+v, why %q", plan, why)
	}
}
