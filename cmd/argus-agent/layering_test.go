package main

import (
	"os/exec"
	"sort"
	"strings"
	"testing"
)

/*
The agent binary carries no control-plane code.

argus-agent is installed on every managed host, potentially thousands of them,
and each copy is a target. Every package it links is attack surface on those
hosts and a reason to ship an agent release when only the control plane
changed. So the agent's import graph is bounded here, and the bound is the
layering rule for the whole module:

	cmd/argus-agent, internal/agent/...   the agent, and only the agent
	shared (listed below)                 wire types and code both ends run
	everything else under internal/       the control plane

The agent tree may import the agent tree and the shared set. The shared set
may import only itself. Control-plane code may import the shared set and, in
its tests, the agent -- but never in the code it ships, so a control package
that reaches for internal/agent fails here too.

A new shared package is a deliberate decision: add it to the list below and
say in its package comment which end needs it and why.
*/

const module = "github.com/gsoultan/argus/"

// shared lists the packages both binaries may link. Keep it short: a package
// goes here when both ends genuinely run the same code -- the tunnel's wire
// format, the recording format the agent writes and the control plane
// verifies -- not when it is merely convenient to put it in one place.
var shared = []string{
	// What an Android package says about itself: the control plane reads it
	// when an APK is added to the software library, the Argus app before it
	// installs one. Standard library only.
	module + "internal/apk",
	module + "internal/hashchain",
	// The fan-out to live viewers: the control plane's for the sessions it
	// relays, the agent's for every session it records.
	module + "internal/live",
	module + "internal/logquiet",
	module + "internal/rdp",
	module + "internal/rdp/pixels",
	module + "internal/recorder",
	module + "internal/secrets",
	module + "internal/tlsconfig",
	module + "internal/tunnel",
}

// controlOnlyModules are third-party dependencies that exist for the control
// plane: its database, object store and YAML config. None of them has any
// business in the agent.
//
// github.com/coder/websocket used to be here, for the console's WebSockets.
// The tunnel now also runs over a WebSocket where a proxy in front of the
// control plane forwards nothing else (internal/tunnel/transport_ws.go), so
// both ends link it through the shared tunnel package.
var controlOnlyModules = []string{
	"github.com/gsoultan/storm",
	"github.com/minio/minio-go",
	"gopkg.in/yaml.v3",
	"github.com/google/uuid",
}

func isAgent(pkg string) bool {
	return pkg == module+"cmd/argus-agent" || pkg == module+"internal/agent" ||
		strings.HasPrefix(pkg, module+"internal/agent/")
}

func isShared(pkg string) bool {
	for _, s := range shared {
		if pkg == s {
			return true
		}
	}
	return false
}

func isFirstParty(pkg string) bool { return strings.HasPrefix(pkg, module) }

// goList runs go list from the module root and returns one line per result.
func goList(t *testing.T, args ...string) []string {
	t.Helper()
	root, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		t.Fatalf("go list -m: %v", err)
	}
	cmd := exec.Command("go", append([]string{"list"}, args...)...)
	cmd.Dir = strings.TrimSpace(string(root))
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("go list %s: %v\n%s", strings.Join(args, " "), err, ee.Stderr)
		}
		t.Fatalf("go list %s: %v", strings.Join(args, " "), err)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// Every package linked into the agent is the agent's own or shared. -deps
// without -test is the linker's view: what ships, not what the tests pull in.
func TestAgentLinksNoControlPlaneCode(t *testing.T) {
	deps := goList(t, "-deps", "-f", "{{.ImportPath}}", "./cmd/argus-agent")

	var bad []string
	for _, pkg := range deps {
		switch {
		case isFirstParty(pkg) && !isAgent(pkg) && !isShared(pkg):
			bad = append(bad, pkg)
		default:
			for _, m := range controlOnlyModules {
				if pkg == m || strings.HasPrefix(pkg, m+"/") {
					bad = append(bad, pkg)
				}
			}
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Errorf("argus-agent links control-plane code:\n  %s\n"+
			"The agent may import only internal/agent/... and the shared set in layering_test.go. "+
			"Move the agent's half into internal/agent/, or add the package to the shared set if both ends truly run it.",
			strings.Join(bad, "\n  "))
	}
}

// The shared set imports nothing but itself, so adding one shared package can
// never drag the control plane in behind it.
func TestSharedPackagesStayNeutral(t *testing.T) {
	args := append([]string{"-deps", "-f", "{{.ImportPath}}"}, func() []string {
		var rel []string
		for _, s := range shared {
			rel = append(rel, "./"+strings.TrimPrefix(s, module))
		}
		return rel
	}()...)
	var bad []string
	for _, pkg := range goList(t, args...) {
		if isFirstParty(pkg) && !isShared(pkg) {
			bad = append(bad, pkg)
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Errorf("a shared package imports something outside the shared set:\n  %s", strings.Join(bad, "\n  "))
	}
}

// The control plane never ships agent code either. Its tests may run a real
// agent against it; the binary may not.
func TestControlPlaneShipsNoAgentCode(t *testing.T) {
	lines := goList(t, "-f", "{{.ImportPath}} {{join .Imports \" \"}}", "./cmd/...", "./internal/...")
	var bad []string
	for _, l := range lines {
		fields := strings.Fields(l)
		pkg := fields[0]
		if isAgent(pkg) || isShared(pkg) {
			continue
		}
		for _, imp := range fields[1:] {
			if isAgent(imp) {
				bad = append(bad, pkg+" imports "+imp)
			}
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Errorf("control-plane code imports the agent:\n  %s", strings.Join(bad, "\n  "))
	}
}
