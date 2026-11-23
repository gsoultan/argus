package agent

import "testing"

// NATS is found by its process, unit, package or image, so the server's
// page offers "Add an account" where nats-server runs.
func TestNATSIsFound(t *testing.T) {
	if k := kindByProcess("nats-server"); k != "nats" {
		t.Errorf("process: %q", k)
	}
	if k := kindByUnit("nats-server.service"); k != "nats" {
		t.Errorf("unit: %q", k)
	}
	if k := kindByPackage("nats-server"); k != "nats" {
		t.Errorf("package: %q", k)
	}
	if k := kindByImage("docker.io/library/nats:2.11-alpine"); k != "nats" {
		t.Errorf("image: %q", k)
	}
}
