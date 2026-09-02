// Command argus-agent runs on a managed host.
//
// It closes the gap the gateway cannot see: a session that never went through
// Argus. Someone holding a standing key who connects straight to sshd on port
// 22 is invisible to the gateway, because nothing crossed it. Only something on
// the host can observe that, which is what this is.
//
// Two modes:
//
//	argus-agent daemon    # runs as root: owns recordings, scans posture, heartbeats
//	argus-agent shim      # runs as the connecting user, invoked by sshd ForceCommand
//
// Plus two helpers:
//
//	argus-agent scan      # one-shot posture report
//	argus-agent install   # print the sshd_config needed to enable recording
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gsoultan/argus/internal/agent"
	"github.com/gsoultan/argus/internal/reporter"
	"github.com/gsoultan/argus/internal/tlsconfig"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "daemon":
		os.Exit(runDaemon(os.Args[2:]))
	case "shim":
		os.Exit(runShim(os.Args[2:]))
	case "scan":
		os.Exit(runScan(os.Args[2:]))
	case "install":
		os.Exit(runInstall(os.Args[2:]))
	case "--version", "version":
		fmt.Printf("argus-agent %s\n", version)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `argus-agent — Argus host agent

  argus-agent daemon    own recordings, scan posture, heartbeat (run as root)
  argus-agent shim      wrap one session (invoked by sshd ForceCommand)
  argus-agent scan      report how reachable this host is without Argus
  argus-agent install   print the sshd_config required to enable recording

`)
}

func runDaemon(args []string) int {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	socket := fs.String("socket", agent.SocketPath, "unix socket to listen on")
	dir := fs.String("recordings", "/var/lib/argus/recordings", "where recordings are written")
	scanEvery := fs.Duration("scan-interval", 5*time.Minute, "how often to scan posture")
	beatEvery := fs.Duration("heartbeat-interval", 30*time.Second, "how often to report liveness")
	shimPath := fs.String("shim", "/usr/libexec/argus-shim", "path sshd invokes")
	stateDir := fs.String("state", "/var/lib/argus", "where posture and heartbeat are written")
	controlURL := fs.String("control-url", "", "argus-control base URL; empty runs standalone")
	controlToken := fs.String("control-token", "", "reporter token")
	controlCA := fs.String("control-ca", "", "CA that signed the control plane's certificate")
	controlCert := fs.String("control-cert", "", "client certificate presented to the control plane")
	controlKey := fs.String("control-key", "", "key for --control-cert")
	_ = fs.Parse(args)

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var clientTLS *tls.Config
	if *controlCA != "" || *controlCert != "" {
		var terr error
		clientTLS, terr = tlsconfig.Client(tlsconfig.ClientOptions{
			CAFile: *controlCA, CertFile: *controlCert, KeyFile: *controlKey,
		})
		if terr != nil {
			log.Error("TLS configuration failed", "error", terr)
			return 1
		}
	}
	rep := reporter.NewWithTLS(*controlURL, *controlToken,
		*stateDir+"/report-spool.jsonl", clientTLS, log)
	if strings.HasPrefix(*controlURL, "http://") {
		log.Warn("control plane URL is plaintext http://",
			"detail", "captured session records cross this link in the clear")
	}
	if rep.Enabled() {
		rep.Drain(ctx)
		rep.StartDrainLoop(ctx, 30*time.Second)
		log.Info("reporting to control plane", "url", *controlURL)
	}

	col := agent.NewCollector(*dir, log)
	col.OnSession = func(rec agent.SessionRecord) {
		// Always keep the local copy. The control plane is where a session
		// becomes visible, not where it becomes real.
		appendJSONL(*stateDir+"/sessions.jsonl", rec, log)

		rep.Session(ctx, map[string]any{
			"id":             rec.ID,
			"userEmail":      rec.Principal + "@" + rec.Hostname,
			"assetHostname":  rec.Hostname,
			"principal":      rec.Principal,
			"protocol":       "ssh",
			"origin":         string(rec.Origin),
			"state":          "closed",
			"startedAt":      rec.StartedAt,
			"endedAt":        rec.EndedAt,
			"clientIp":       rec.ClientAddr,
			"fidelity":       "pty",
			"recordingBytes": rec.Bytes,
			"exitCode":       rec.ExitCode,
			"chainHead":      rec.ChainHead,
			"reportedBy":     "agent:" + rec.Hostname,
			"riskFlags":      agentRiskFlags(rec),
		})
	}

	// Posture scanning and heartbeat run alongside the collector. The heartbeat
	// is what makes killing the agent detectable: a user with root can stop it,
	// but they cannot make the silence look like health.
	stop := make(chan struct{})
	go loop(*scanEvery, stop, func() {
		p, err := agent.Scan(agent.DefaultScanConfig(*shimPath))
		if err != nil {
			log.Error("posture scan failed", "error", err)
			return
		}
		writeJSON(*stateDir+"/posture.json", p, log)

		rating := p.Rating()
		attrs := []any{
			"rating", rating,
			"unmanaged_keys", len(p.UnmanagedKeys),
			"shim_installed", p.ShimInstalled,
		}
		if rating == "open" {
			log.Warn("host is UNMONITORED — a direct session here would leave no trace", attrs...)
		} else if len(p.Drift) > 0 {
			log.Warn("sshd configuration drift", append(attrs, "drift", strings.Join(p.Drift, "; "))...)
		} else {
			log.Info("posture", attrs...)
		}
	})

	go loop(*beatEvery, stop, func() {
		hostname, _ := os.Hostname()
		beat := map[string]any{
			"at":              time.Now().UTC(),
			"version":         version,
			"active_sessions": col.ActiveCount(),
		}
		writeJSON(*stateDir+"/heartbeat.json", beat, log)

		// The heartbeat carries posture so the console can show coverage
		// without a second call, and so silence removes both at once.
		var posture any
		if p, err := agent.Scan(agent.DefaultScanConfig(*shimPath)); err == nil {
			posture = map[string]any{
				"rating":         p.Rating(),
				"unmanaged_keys": p.UnmanagedKeys,
				"shim_installed": p.ShimInstalled,
				"drift":          p.Drift,
			}
		}
		rep.Heartbeat(ctx, map[string]any{
			"hostname":        hostname,
			"version":         version,
			"active_sessions": col.ActiveCount(),
			"posture":         posture,
		})
	})

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		log.Info("shutting down, sealing in-flight recordings")
		close(stop)
		cancel()
		_ = col.Close()
	}()

	log.Info("argus-agent starting", "version", version, "socket", *socket)
	if err := col.Listen(*socket); err != nil {
		log.Error("collector failed", "error", err)
		return 1
	}
	return 0
}

// agentRiskFlags marks what the console highlights about a session the agent saw.
func agentRiskFlags(rec agent.SessionRecord) []string {
	flags := []string{}
	if rec.Origin == agent.Direct {
		flags = append(flags, "bypassed-gateway")
	}
	if rec.Principal == "root" {
		flags = append(flags, "root-principal")
	}
	return flags
}

func runShim(args []string) int {
	fs := flag.NewFlagSet("shim", flag.ExitOnError)
	socket := fs.String("socket", agent.SocketPath, "collector socket")
	gatewayKeys := fs.String("gateway-keys", "/etc/argus/gateway_keys", "gateway public keys (authorized_keys format); the authoritative signal")
	gateways := fs.String("gateways", "", "fallback: comma-separated gateway addresses, never treated as proof")
	skipBrokered := fs.Bool("skip-brokered", false,
		"do not record sessions that appear to come from the gateway (saves storage; a stolen gateway key can then suppress recording)")
	failOpen := fs.Bool("fail-open", false, "allow unrecorded sessions when the collector is down")
	shell := fs.String("shell", "", "override the login shell")
	_ = fs.Parse(args)

	var gws []string
	for _, g := range strings.Split(*gateways, ",") {
		if g = strings.TrimSpace(g); g != "" {
			gws = append(gws, g)
		}
	}

	return agent.RunShim(agent.ShimConfig{
		SocketPath:   *socket,
		GatewayKeys:  *gatewayKeys,
		GatewayAddrs: gws,
		SkipBrokered: *skipBrokered,
		FailOpen:     *failOpen,
		Shell:        *shell,
	})
}

func runScan(args []string) int {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	shimPath := fs.String("shim", "/usr/libexec/argus-shim", "path sshd should invoke")
	asJSON := fs.Bool("json", false, "emit JSON")
	_ = fs.Parse(args)

	p, err := agent.Scan(agent.DefaultScanConfig(*shimPath))
	if err != nil {
		fmt.Fprintf(os.Stderr, "argus-agent: %v\n", err)
		return 2
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(p)
		return 0
	}

	rating := p.Rating()
	fmt.Printf("host       %s\n", p.Hostname)
	fmt.Printf("posture    %s\n", rating)
	fmt.Printf("recorder   %s\n", yesNo(p.ShimInstalled, "installed", "NOT INSTALLED"))
	fmt.Printf("cert auth  %s\n", yesNo(p.TrustedCAConfigured, "configured", "not configured"))
	fmt.Printf("passwords  %s\n", yesNo(!p.PasswordAuthEnabled, "disabled", "ENABLED"))

	if len(p.UnmanagedKeys) > 0 {
		fmt.Printf("\n%d key(s) Argus did not issue — each can reach this host directly:\n",
			len(p.UnmanagedKeys))
		for _, k := range p.UnmanagedKeys {
			fmt.Printf("  %-10s %s  %s\n", k.User, k.Fingerprint, k.Comment)
			fmt.Printf("  %-10s %s:%d\n", "", k.File, k.Line)
		}
	}
	if len(p.Drift) > 0 {
		fmt.Printf("\nconfiguration drift:\n")
		for _, d := range p.Drift {
			fmt.Printf("  - %s\n", d)
		}
	}

	if rating == "open" {
		fmt.Print("\nThis host is UNMONITORED. A direct connection to port 22 would\n" +
			"leave no trace. Run `argus-agent install` to see what to change.\n")
		return 1
	}
	return 0
}

func runInstall(args []string) int {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	shimPath := fs.String("shim", "/usr/libexec/argus-shim", "path sshd will invoke")
	gatewayKeys := fs.String("gateway-keys", "/etc/argus/gateway_keys", "gateway public keys")
	_ = fs.Parse(args)

	fmt.Printf(`# Argus host recording — append to /etc/ssh/sshd_config, then:
#   sshd -t && systemctl reload sshd
#
# ForceCommand makes sshd run the recorder for EVERY session, including one
# opened with a standing key that never touched the gateway. That is the point:
# without it, a bypass is invisible.

ForceCommand %s shim --gateway-keys %s

# ExposeAuthInfo lets the recorder identify the gateway by the key it
# authenticated with. Without it the agent cannot prove a session was brokered,
# so it records everything — correct, but noisier.
ExposeAuthInfo yes

# Recommended alongside it. These remove the ability to bypass rather than
# merely recording it, which is the stronger control:
#
#   PasswordAuthentication no      # a bypass that needs no key at all
#   PermitRootLogin no             # root is unattributable to a person
#   TrustedUserCAKeys /etc/ssh/argus_ca.pub   # lets standing keys be removed

# Verify with:
#   argus-agent scan
`, *shimPath, *gatewayKeys)
	return 0
}

func loop(every time.Duration, stop <-chan struct{}, fn func()) {
	fn() // run once immediately so state exists before the first tick
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			fn()
		case <-stop:
			return
		}
	}
}

func writeJSON(path string, v any, log *slog.Logger) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(dirOf(path), 0o700); err != nil {
		log.Error("create state dir", "error", err)
		return
	}
	// Write-then-rename so a reader never sees a half-written file.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Error("write state", "path", path, "error", err)
		return
	}
	_ = os.Rename(tmp, path)
}

func appendJSONL(path string, v any, log *slog.Logger) {
	if err := os.MkdirAll(dirOf(path), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.Error("open spool", "error", err)
		return
	}
	defer f.Close()
	_ = json.NewEncoder(f).Encode(v)
}

func dirOf(path string) string {
	if i := strings.LastIndex(path, "/"); i > 0 {
		return path[:i]
	}
	return "."
}

func yesNo(ok bool, yes, no string) string {
	if ok {
		return yes
	}
	return no
}
