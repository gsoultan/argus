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
// Plus four helpers:
//
//	argus-agent enroll    # exchange a token from the console for this host's credential
//	argus-agent scan      # one-shot posture report
//	argus-agent install   # print the sshd_config needed to enable recording
//	argus-agent uninstall # take the agent off this host, keeping its recordings
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gsoultan/argus/internal/agent"
	"github.com/gsoultan/argus/internal/agent/execlog"
	"github.com/gsoultan/argus/internal/agent/reporter"
	"github.com/gsoultan/argus/internal/logquiet"
	"github.com/gsoultan/argus/internal/tlsconfig"
	"github.com/gsoultan/argus/internal/tunnel"
)

var version = "dev"

func main() {
	if code, ok := runAsService(); ok {
		os.Exit(code)
	}
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "daemon":
		os.Exit(runDaemon(os.Args[2:]))
	case "shim":
		os.Exit(runShim(os.Args[2:]))
	case "personal-apply":
		// Run by the daemon, outside its sandbox, to make personal accounts.
		os.Exit(agent.RunPersonalApply(os.Stdin, os.Stdout, os.Stderr))
	case "file-op":
		// Run by the files service as the account, one per operation.
		os.Exit(agent.RunFileOp(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	case "screen-helper":
		// Run by the daemon in the console's session, on Windows, to share
		// the screen: input on stdin, screen updates on stdout.
		os.Exit(agent.RunScreenHelper(os.Stdin, os.Stdout))
	case "pg-admin":
		// Run by the postgres service as the postgres account: one request
		// on stdin, one answer on stdout, over the local socket.
		os.Exit(agent.PGAdminMain(context.Background(), os.Stdin, os.Stdout))
	case "update-apply":
		// Run by the daemon in a unit of its own to install an update the
		// console asked for, restart the agent, and go back if it fails.
		fs := flag.NewFlagSet("update-apply", flag.ExitOnError)
		plan := fs.String("plan", "", "the update plan the daemon wrote")
		_ = fs.Parse(os.Args[2:])
		os.Exit(agent.RunUpdateApply(*plan, os.Stdout))
	case "pg-backup":
		// Run by the backup service as the postgres account: one dump,
		// restore or catalog, with the archive on stdout or stdin.
		os.Exit(agent.PGBackupMain(context.Background(), os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	case "scan":
		os.Exit(runScan(os.Args[2:]))
	case "probe":
		os.Exit(runProbe())
	case "install":
		os.Exit(runInstall(os.Args[2:]))
	case "uninstall":
		os.Exit(runUninstall(os.Args[2:]))
	case "enroll", "enrol":
		os.Exit(runEnroll(os.Args[2:]))
	case "service":
		os.Exit(runService(os.Args[2:]))
	case "--version", "version":
		fmt.Printf("argus-agent %s\n", version)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `argus-agent — Argus host agent

  argus-agent enroll    exchange a token from the console for this host's credential
  argus-agent daemon    own recordings, scan posture, heartbeat (run as root)
  argus-agent shim      wrap one session (invoked by sshd ForceCommand)
  argus-agent scan      report how reachable this host is without Argus
  argus-agent probe     report whether this kernel can observe command execution
  argus-agent install   print the sshd_config required to enable recording
  argus-agent service   install|uninstall|start|stop the Windows service
  argus-agent uninstall take the agent off this host, keeping its recordings

`)
}

func runDaemon(args []string) int {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	socket := fs.String("socket", agent.SocketPath, "unix socket to listen on")
	dir := fs.String("recordings", agent.DefaultRecordingsDir, "where recordings are written")
	scanEvery := fs.Duration("scan-interval", 5*time.Minute, "how often to scan posture")
	beatEvery := fs.Duration("heartbeat-interval", 30*time.Second, "how often to report liveness")
	shimPath := fs.String("shim", agent.DefaultShimPath, "path sshd invokes")
	stateDir := fs.String("state", agent.DefaultStateDir, "where posture and heartbeat are written")
	controlURL := fs.String("control-url", "",
		"argus-control base URL; empty uses the enrollment in --state, or runs standalone")
	controlToken := fs.String("control-token", "", "reporter token (unenrolled agents only)")
	controlCA := fs.String("control-ca", "", "CA that signed the control plane's certificate")
	controlCert := fs.String("control-cert", "", "client certificate presented to the control plane")
	controlKey := fs.String("control-key", "", "key for --control-cert")
	useTunnel := fs.Bool("tunnel", true,
		"hold an outbound connection to the control plane (enrolled agents only)")
	keepShipped := fs.Duration("keep-shipped", agent.DefaultKeepShipped,
		"how long to keep a recording on this host once the control plane has verified and "+
			"stored it (compressed after an hour); 0 keeps it for ever. Never applies to one "+
			"the control plane does not have. Unset, the console's Settings decide")
	logLevel := fs.String("log-level", envOr("ARGUS_LOG_LEVEL", "info"),
		"debug, info, warn or error; ARGUS_LOG_LEVEL in agent.env sets it too")
	_ = fs.Parse(args)
	// Given on the command line, the host's own choice beats the console's.
	keepPinned := false
	fs.Visit(func(f *flag.Flag) { keepPinned = keepPinned || f.Name == "keep-shipped" })

	// A line that repeats -- the control plane unreachable, a tunnel a proxy
	// keeps dropping -- is written once every few minutes with a count, not
	// on every retry: see internal/logquiet.
	log := slog.New(logquiet.New(slog.NewTextHandler(logOutput,
		&slog.HandlerOptions{Level: logquiet.ParseLevel(*logLevel)}), 0))

	// Before anything is written there: on Windows a new directory is
	// readable by every local user until it is locked down.
	for _, d := range []string{*stateDir, *dir} {
		if err := agent.ProtectStateDir(d); err != nil {
			log.Error("cannot make the agent's directory private", "dir", d, "error", err)
			return 1
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var clientTLS *tls.Config

	// An enrolled agent reports with its own credential. Flags still win, so
	// a deployment configured the old way keeps working through an upgrade.
	if *controlURL == "" {
		enr, err := agent.LoadEnrollment(*stateDir)
		if err != nil {
			log.Error("enrollment could not be read", "error", err)
			return 1
		}
		if enr != nil {
			*controlURL, *controlToken = enr.ControlURL, enr.Credential
			if clientTLS, err = enr.TLS(); err != nil {
				log.Error("enrollment TLS configuration failed", "error", err)
				return 1
			}
			if enr.Pin != "" {
				log.Warn("trusting the control plane by its key pin",
					"detail", "reports will stop when its key changes; re-enroll with "+
						"--control-ca to trust its CA instead")
			}
			if h, _ := os.Hostname(); !strings.EqualFold(h, enr.Hostname) {
				// The control plane will refuse every report: the credential
				// may speak only for the name it was issued to.
				log.Error("this host was enrolled under another name",
					"enrolled_as", enr.Hostname, "hostname", h,
					"detail", "reports will be refused; revoke it in the console and enroll again")
			}
		} else if *controlToken == "" {
			log.Warn("not enrolled: running standalone",
				"detail", "sessions are recorded on this host but not reported; "+
					"run argus-agent enroll with a token from the console")
		}
	}
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

	// The tunnel: one outbound connection the control plane opens streams
	// on, so this host needs no inbound port. Only for an enrolled agent: the
	// control plane admits a tunnel on a per-host credential and never on the
	// shared reporter token.
	col := agent.NewCollector(*dir, log)

	// Updating from the console. It waits for everything open on the server
	// to end, and while it restarts, new sessions wait rather than fail:
	// see internal/agent/update.go.
	exe, _ := os.Executable()
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	updater := agent.NewUpdater(*stateDir, version, exe, log)
	updater.Marker = agent.UpdateMarkerPath(*socket)
	var (
		openStreams atomic.Int64
		holding     atomic.Bool
		listening   atomic.Bool
		tunnelUp    atomic.Bool
	)
	updater.Busy = func() int { return col.ShimCount() + int(openStreams.Load()) }
	updater.Hold = func(on bool) {
		holding.Store(on)
		col.Hold(on)
	}
	ready := func() { agent.WriteReady(*stateDir, version, listening.Load(), tunnelUp.Load()) }

	if *useTunnel && strings.HasPrefix(*controlToken, "argus-agent-") {
		updater.NeedTunnel = true
		hostname, _ := os.Hostname()
		services := agent.DefaultTunnelServices()
		// Browser sessions open here, as the account, recorded by this
		// daemon before anything is shown.
		services[agent.TerminalService] = agent.Terminal(agent.TerminalConfig{
			Collector: col,
			Verifier:  &tunnel.GrantVerifier{Host: hostname},
			Hostname:  hostname,
			Log:       log,
		})
		// A read-only view of any session being recorded here, for the
		// console's Watch live.
		services[agent.WatchService] = agent.Watch(agent.WatchConfig{
			Collector: col,
			Verifier:  &tunnel.GrantVerifier{Host: hostname},
			Log:       log,
		})
		// File transfer, as the account, on a grant of its own.
		services[agent.FilesService] = agent.Files(agent.FilesConfig{
			Verifier: &tunnel.GrantVerifier{Host: hostname},
			Log:      log,
		})
		// Postgres on this server, as the person's own role, every statement
		// recorded before it runs.
		services[agent.PostgresService] = agent.Postgres(agent.PostgresConfig{
			Verifier:  &tunnel.GrantVerifier{Host: hostname},
			RecordDir: *dir,
			Log:       log,
		})
		// Redis and RabbitMQ on this server, as the person's own user for the
		// session, every command recorded before it runs.
		services[agent.RedisService] = agent.Redis(agent.StoreConfig{
			Verifier:  &tunnel.GrantVerifier{Host: hostname},
			RecordDir: *dir,
			Log:       log,
		})
		services[agent.RabbitMQService] = agent.RabbitMQ(agent.RabbitConfig{StoreConfig: agent.StoreConfig{
			Verifier:  &tunnel.GrantVerifier{Host: hostname},
			RecordDir: *dir,
			Log:       log,
		}})
		// Backups of those databases, as the postgres account, encrypted
		// here before they touch a disk; see internal/agent/backup.go.
		services[agent.BackupService] = agent.Backup(agent.BackupConfig{
			Verifier: &tunnel.GrantVerifier{Host: hostname},
			Dir:      filepath.Join(*stateDir, "backups"),
			Log:      log,
		})
		// What databases are on this server, for the console's add form;
		// reads names only, see internal/agent/discover.go.
		services[agent.DiscoverDBService] = agent.DiscoverDatabases(agent.DiscoverDBConfig{
			Verifier: &tunnel.GrantVerifier{Host: hostname},
			Log:      log,
		})
		// Remote Desktop, to this host's own listener, on Windows: the agent is
		// the RDP client and records the session before it is shown.
		if runtime.GOOS == "windows" {
			services[agent.DesktopService] = agent.Desktop(agent.DesktopConfig{
				Collector: col,
				Verifier:  &tunnel.GrantVerifier{Host: hostname},
				Hostname:  hostname,
				Log:       log,
			})
			// The console itself, for a host without Remote Desktop.
			services[agent.ScreenService] = agent.Screen(agent.DesktopConfig{
				Collector: col,
				Verifier:  &tunnel.GrantVerifier{Host: hostname},
				Hostname:  hostname,
				Log:       log,
			})
		}
		go agent.RunTunnel(ctx, agent.TunnelConfig{
			Dial: tunnel.DialOptions{
				ControlURL: *controlURL,
				Credential: *controlToken,
				Hostname:   hostname,
				Version:    version,
				TLS:        clientTLS,
			},
			Services: services,
			Log:      log,
			GrantPin: agent.NewGrantPin(*stateDir),
			Gate: func(string) string {
				if holding.Load() {
					return "this server's agent is restarting for an update; try again in a few seconds"
				}
				return ""
			},
			Open: &openStreams,
			OnConnected: func() {
				tunnelUp.Store(true)
				ready()
			},
		})
	}

	// The direct SSH setting survives a restart: an agent restarted during a
	// control plane outage keeps refusing or holding direct logins rather than
	// reopening sshd until the next heartbeat gets through. console-only is
	// what an agent older than the setting wrote, and is read when the newer
	// file is absent.
	directSSHFile := *stateDir + "/direct-ssh"
	// direct-ssh.json holds the mode and the named accounts together, written
	// in one rename so a restart never pairs one heartbeat's mode with
	// another's accounts. direct-ssh, the mode alone, is still written for an
	// agent downgraded to before per-account modes.
	directSSHStateFile := *stateDir + "/direct-ssh.json"
	consoleOnlyFile := *stateDir + "/console-only"
	var saved directSSHState
	if b, err := os.ReadFile(directSSHStateFile); err == nil && json.Unmarshal(b, &saved) == nil && saved.Mode != "" {
		col.SetDirectSSH(saved.Mode)
		col.SetDirectSSHAccounts(saved.Accounts)
		log.Info("direct ssh logins: applying the last setting heard",
			"mode", col.DirectSSH(), "accounts", len(saved.Accounts))
	} else if b, err := os.ReadFile(directSSHFile); err == nil {
		col.SetDirectSSH(strings.TrimSpace(string(b)))
		log.Info("direct ssh logins: applying the last setting heard", "mode", col.DirectSSH())
	} else if b, err := os.ReadFile(consoleOnlyFile); err == nil && strings.TrimSpace(string(b)) == "1" {
		col.SetConsoleOnly(true)
		log.Info("console-only: direct logins to this host are refused until the console says otherwise")
	}
	// Personal accounts survive a restart the same way: the last list heard
	// decides which direct logins are a person's own until the next heartbeat.
	personalFile := *stateDir + "/personal-accounts.json"
	provisioner := &agent.Provisioner{
		Sudoers: "/etc/sudoers.d/argus-personal",
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		},
		Log: log,
	}
	// useradd, sudoers and authorized_keys are all out of reach of the
	// unit's sandbox, so on a systemd host a pass runs as a unit of its own.
	if exe, err := os.Executable(); err == nil {
		provisioner.Outside = agent.SystemdOutside(exe)
	}
	var personalStates []agent.PersonalAccountState
	// The login accounts an admin made here from the console. Kept between
	// heartbeats, so a reply that leaves them out locks nothing.
	var serverLogins struct {
		Keep   []string `json:"keep"`
		Delete []string `json:"delete"`
	}
	if b, err := os.ReadFile(personalFile); err == nil {
		var list []agent.PersonalAccount
		if err := json.Unmarshal(b, &list); err == nil {
			col.SetPersonalAccounts(list)
			log.Info("personal accounts: applying the last list heard", "accounts", len(list))
		}
	}
	col.AskApproval = func(ctx context.Context, m agent.SessionStart) agent.Approval {
		return askDirectApproval(ctx, rep, m, log)
	}
	col.OnAllowed = func(m agent.SessionStart) {
		hostname, _ := os.Hostname()
		rep.Login(ctx, map[string]any{
			"hostname":    hostname,
			"principal":   m.Principal,
			"client_addr": m.ClientAddr,
			"command":     m.Command,
			"origin":      m.Origin,
			"allowed":     true,
			// Set when the login used a personal account its owner may
			// reach directly, which is what let it in.
			"owner": m.Actor,
			"auth":  m.Auth,
			"at":    time.Now().UTC(),
		})
	}
	col.OnRefused = func(m agent.SessionStart) {
		hostname, _ := os.Hostname()
		rep.Login(ctx, map[string]any{
			"hostname":    hostname,
			"principal":   m.Principal,
			"client_addr": m.ClientAddr,
			"command":     m.Command,
			"origin":      m.Origin,
			"scope":       "console_only",
			"refused":     true,
			"auth":        m.Auth,
			"at":          time.Now().UTC(),
		})
	}
	var uploads *agent.Uploads
	if rep.Enabled() {
		uploads = agent.NewUploads(*stateDir, rep, log)
		uploads.Keep = *keepShipped
		go uploads.Run(ctx, time.Minute)
	}
	// Removing this server in the console asks, on a heartbeat, for the agent
	// to take itself off; see selfUninstall.
	removed := &selfUninstall{stateDir: *stateDir, rep: rep, uploads: uploads, log: log}
	col.OnSession = func(rec agent.SessionRecord) {
		// Always keep the local copy. The control plane is where a session
		// becomes visible, not where it becomes real.
		appendJSONL(*stateDir+"/sessions.jsonl", rec, log)

		rep.Session(ctx, sessionReport(rec))
		// The recording itself, so it can be replayed from the console. Queued
		// on disk first: the host's copy is the evidence until the control
		// plane has verified and stored its own.
		uploads.Queue(rec)
	}
	// A session through the tunnel shows in the console while it runs, and is
	// spoken for every minute, so a long one is not taken for one whose host
	// went quiet.
	col.OnActive = func(rec agent.SessionRecord) { rep.Session(ctx, sessionReport(rec)) }

	// Kernel execution tracing, when the host can support it. A failure here is
	// a supported configuration, not a fatal one: the agent still records
	// terminal output, and the reason is carried on every session so an auditor
	// can see why a recording is PTY-only instead of guessing.
	execCtx := execlog.Start(ctx, func(c context.Context) (execlog.Probe, error) {
		return execlog.Open(c, log)
	})
	if execCtx.Tracer == nil {
		log.Warn("kernel execution tracing is off; recordings will be terminal output only",
			"reason", execCtx.Reason,
			"detail", "a user can obscure what they ran — base64, a sourced script, "+
				"a shell spawned from an editor — and none of it is distinguishable "+
				"in terminal output alone")
	} else {
		defer execCtx.Tracer.Close()
	}
	col.Exec = execCtx

	// Posture scanning and heartbeat run alongside the collector. The heartbeat
	// is what makes killing the agent detectable: a user with root can stop it,
	// but they cannot make the silence look like health.
	stop := make(chan struct{})
	// One scan shared by the posture loop and the heartbeat. The loop scans
	// in full; the heartbeat reuses that answer while nothing the scan read
	// has changed, which it checks file by file on every beat.
	posture := agent.NewPostureCache(agent.DefaultScanConfig(*shimPath), *scanEvery)
	go loop(time.Minute, stop, func() {
		for _, rec := range col.ActiveTunnelRecords() {
			rep.Session(ctx, sessionReport(rec))
		}
	})
	go loop(*scanEvery, stop, func() {
		p, err := posture.Rescan()
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

	// One heartbeat at a time: an update reports a change at once, between
	// the regular ones, and two in flight would apply replies out of order.
	var beating sync.Mutex
	// Usage rides about one heartbeat a minute: often enough for the
	// server page to be current, and half the writes of every beat.
	var (
		usage     agent.UsageSampler
		usageSent time.Time
	)
	heartbeat := func() {
		beating.Lock()
		defer beating.Unlock()
		hostname, _ := os.Hostname()
		beat := map[string]any{
			"at":              time.Now().UTC(),
			"version":         version,
			"active_sessions": col.ActiveCount(),
		}
		writeJSON(*stateDir+"/heartbeat.json", beat, log)

		// The heartbeat carries posture so the console can show coverage
		// without a second call, and so silence removes both at once.
		var postureReport any
		if p, err := posture.Scan(); err == nil {
			postureReport = map[string]any{
				"rating":         p.Rating(),
				"unmanaged_keys": p.UnmanagedKeys,
				"shim_installed": p.ShimInstalled,
				"drift":          p.Drift,
				"login_accounts": p.LoginAccounts,
				"sshd_auth":      p.SSHDAuth,
			}
		}
		var usageReport any
		if time.Since(usageSent) >= 55*time.Second {
			if u, ok := usage.Sample(); ok {
				usageReport, usageSent = u, time.Now()
			}
		}
		reply, ok := rep.HeartbeatWithReply(ctx, map[string]any{
			"hostname":        hostname,
			"version":         version,
			"active_sessions": col.ActiveCount(),
			"posture":         postureReport,
			"exec_tracing":    execCtx.Tracer != nil,
			"exec_reason":     execCtx.Reason,
			"console_only":    col.ConsoleOnly(),
			"direct_ssh":      col.DirectSSH(),
			// nil until the first list has been applied, so the console
			// can tell "none" from "not yet".
			"personal_accounts": personalStates,
			"update":            updater.Status(),
			"usage":             usageReport,
		})
		// An update is acted on only when the reply arrived: silence from
		// the control plane neither starts one nor takes one back.
		if ok {
			rep.SetShipZstd(reply.AcceptsZstd())
		}
		if ok && !keepPinned && reply.KeepRecordingsHours != nil && *reply.KeepRecordingsHours >= 0 {
			uploads.SetKeep(time.Duration(*reply.KeepRecordingsHours) * time.Hour)
		}
		if ok {
			var req *agent.UpdateRequest
			if len(reply.Update) > 0 && string(reply.Update) != "null" {
				req = &agent.UpdateRequest{}
				if err := json.Unmarshal(reply.Update, req); err != nil {
					log.Error("update: cannot read the request", "error", err)
					req = nil
				}
			}
			if req != nil || updater.Status().State != "" {
				updater.Handle(ctx, req)
			}
		}
		// Only a delivered reply changes anything. An unreachable control
		// plane leaves the last setting in place, not a default chosen by an
		// outage.
		if ok && len(reply.PersonalAccounts) > 0 && string(reply.PersonalAccounts) != "null" {
			var list []agent.PersonalAccount
			if err := json.Unmarshal(reply.PersonalAccounts, &list); err != nil {
				log.Error("personal accounts: cannot read the list", "error", err)
			} else {
				col.SetPersonalAccounts(list)
				if b, err := json.Marshal(list); err == nil {
					if err := os.WriteFile(personalFile, b, 0o600); err != nil {
						log.Error("personal accounts: cannot persist the list", "error", err)
					}
				}
				if len(reply.ServerLogins) > 0 && string(reply.ServerLogins) != "null" {
					if err := json.Unmarshal(reply.ServerLogins, &serverLogins); err != nil {
						log.Error("server logins: cannot read the list", "error", err)
					}
				}
				// Shared logins are made and locked like personal accounts,
				// but are nobody's own: they go to the provisioner and not
				// to the collector, so no direct login is a person's because
				// it used one.
				want := slices.Clone(list)
				for _, name := range serverLogins.Keep {
					want = append(want, agent.PersonalAccount{Name: name, Owner: agent.SharedLoginOwner})
				}
				personalStates = provisioner.ReconcileDeleting(ctx, want, serverLogins.Delete)
			}
		}
		if ok && reply.Uninstall {
			removed.start(ctx)
		}
		if ok {
			// Absent from an older control plane, or one that could not read
			// its policy: keep what was last heard.
			if reply.DirectSSH != nil {
				// A control plane with per-account modes says what every
				// other account gets in direct_ssh_default; direct_ssh is
				// then the strictest mode, for agents that cannot tell
				// accounts apart. An older one sends direct_ssh alone.
				mode, accounts := *reply.DirectSSH, map[string]string{}
				if reply.DirectSSHDefault != nil {
					mode, accounts = *reply.DirectSSHDefault, reply.DirectSSHAccounts
				}
				if mode != col.DirectSSH() {
					log.Warn("direct ssh logins changed by the console", "mode", mode)
				}
				if !maps.Equal(accounts, col.DirectSSHAccounts()) {
					log.Warn("direct ssh logins for named accounts changed by the console",
						"accounts", len(accounts))
				}
				col.SetDirectSSHAccounts(accounts)
				col.SetDirectSSH(mode)
				if err := saveDirectSSHState(directSSHStateFile, directSSHState{
					Mode: col.DirectSSH(), Accounts: col.DirectSSHAccounts(),
				}); err != nil {
					log.Error("direct ssh: cannot persist the setting", "error", err)
				}
				if err := os.WriteFile(directSSHFile, []byte(col.DirectSSH()+"\n"), 0o600); err != nil {
					log.Error("direct ssh: cannot persist the setting", "error", err)
				}
			} else if reply.ConsoleOnly != nil {
				on := *reply.ConsoleOnly
				if on != col.ConsoleOnly() {
					log.Warn("console-only changed by the console", "console_only", on,
						"detail", "direct ssh, scp and sftp logins are refused while this is on")
				}
				col.SetConsoleOnly(on)
				v := "0"
				if on {
					v = "1"
				}
				if err := os.WriteFile(consoleOnlyFile, []byte(v+"\n"), 0o600); err != nil {
					log.Error("console-only: cannot persist the setting", "error", err)
				}
				// Read first on a restart, so they must not hold an older answer.
				col.SetDirectSSHAccounts(nil)
				_ = saveDirectSSHState(directSSHStateFile, directSSHState{Mode: col.DirectSSH()})
				_ = os.WriteFile(directSSHFile, []byte(col.DirectSSH()+"\n"), 0o600)
			}
		}
	}
	updater.Changed = func() {
		if rep.Enabled() {
			heartbeat()
		}
	}
	go loop(*beatEvery, stop, heartbeat)
	go updater.Watch(ctx)

	// Host facts on their own, slower loop. The inventory is hand-written, so
	// "Argus covers all privileged access" is only checkable if hosts say what
	// they are — the machine nobody remembered to add is the one that matters.
	//
	// Checked every ten minutes, because what runs on a host (a database
	// someone just installed) changes more often than its OS, but sent only
	// when something changed or an hour has passed, so an idle host does not
	// rewrite its row every ten minutes to say nothing happened.
	var (
		lastFacts []byte
		lastSent  time.Time
	)
	go loop(10*time.Minute, stop, func() {
		f := agent.Discover(agent.DefaultDiscoverConfig())
		apps := agent.DiscoverApps(ctx)
		sys := agent.DiscoverSystem()
		writeJSON(*stateDir+"/facts.json", f, log)
		writeJSON(*stateDir+"/applications.json", apps, log)

		report := map[string]any{
			"hostname":     f.Hostname,
			"fqdn":         f.FQDN,
			"machine_id":   f.MachineID,
			"os":           f.OS,
			"addresses":    f.Addresses,
			"ssh_ports":    f.SSHPorts,
			"accounts":     f.Accounts,
			"applications": apps,
			"system":       sys,
		}
		// Compared without the scan time, which differs every run.
		probe := apps
		probe.CheckedAt = time.Time{}
		fingerprint, _ := json.Marshal([]any{f, probe, sys})
		if bytes.Equal(fingerprint, lastFacts) && time.Since(lastSent) < time.Hour {
			return
		}
		lastFacts, lastSent = fingerprint, time.Now()

		if len(f.SSHPorts) > 1 {
			// A port the inventory does not know about is a documented,
			// unmonitored way in.
			log.Info("sshd listens on more than one port",
				"ports", f.SSHPorts,
				"detail", "confirm every port is covered by the inventory")
		}
		rep.Facts(ctx, report)
	})

	stopReq := stopRequests()

	log.Info("argus-agent starting", "version", version, "socket", *socket)
	listenErr := make(chan error, 1)
	go func() { listenErr <- col.Listen(*socket) }()
	// Say when logins are being taken, for an update helper waiting to see
	// this version come up.
	go func() {
		for i := 0; i < 200 && ctx.Err() == nil; i++ {
			if c, err := net.DialTimeout("unix", *socket, time.Second); err == nil {
				_ = c.Close()
				listening.Store(true)
				ready()
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	select {
	case err := <-listenErr:
		// Failed on its own; no shutdown is in flight.
		if err != nil {
			log.Error("collector failed", "error", err)
			return 1
		}
		return 0
	case <-stopReq:
		// Handled here rather than in a goroutine. Close shuts the socket
		// first, so Listen returns immediately -- and returning on that exited
		// the agent through the middle of its own drain, losing the seal on
		// every recording still open. On a fleet an upgrade did that to every
		// host with someone logged in.
		log.Info("shutting down, sealing in-flight recordings")
		close(stop)
		cancel()
		if updater.Restarting() {
			// An update: the sessions it ends are the ones the operator chose
			// to end, and SSH logins resume recording on the next agent, so
			// a drain would only keep the logins waiting meanwhile.
			_ = col.CloseWithin(updateDrain)
		} else {
			_ = col.Close()
		}
		<-listenErr
		log.Info("shutdown complete")
		return 0
	}
}

// updateDrain is how long an agent restarting for an update waits for
// sessions before it disconnects them and seals their recordings.
const updateDrain = 2 * time.Second

// sessionReport is what the control plane is told about a session.
func sessionReport(rec agent.SessionRecord) map[string]any {
	// A session through the tunnel is attributed to the person the control
	// plane signed it for; a login through sshd only to the local account.
	who := rec.Principal + "@" + rec.Hostname
	if rec.Actor != "" {
		who = rec.Actor
	}
	protocol, state := rec.Protocol, rec.State
	if protocol == "" {
		protocol = "ssh"
	}
	if state == "" {
		state = "closed"
	}
	fidelity := rec.Fidelity
	if protocol == "rdp" {
		fidelity = "rdp"
	}
	out := map[string]any{
		"id":             rec.ID,
		"userEmail":      who,
		"assetHostname":  rec.Hostname,
		"principal":      rec.Principal,
		"protocol":       protocol,
		"origin":         string(rec.Origin),
		"state":          state,
		"startedAt":      rec.StartedAt,
		"clientIp":       rec.ClientAddr,
		"fidelity":       fidelity,
		"commandCount":   rec.Commands,
		"recordingBytes": rec.Bytes,
		"reportedBy":     "agent:" + rec.Hostname,
		"riskFlags":      agentRiskFlags(rec),
	}
	if state != "active" {
		out["endedAt"] = rec.EndedAt
		out["exitCode"] = rec.ExitCode
		out["chainHead"] = rec.ChainHead
	}
	if rec.TerminatedBy != "" {
		out["terminatedBy"] = rec.TerminatedBy
		out["terminationReason"] = rec.TerminationReason
	}
	if rec.Auth != nil {
		out["auth"] = rec.Auth
	}
	if rec.DirectApproval != "" {
		out["directApproval"] = rec.DirectApproval
		out["directLoginId"] = rec.DirectLoginID
	}
	return out
}

// askDirectApproval asks the control plane whether a direct login may go
// ahead, and waits for an admin's answer while it is held.
//
// Fails closed: a control plane that cannot be reached, an answer that cannot
// be read, or a deadline passing all refuse the login.
func askDirectApproval(ctx context.Context, rep *reporter.Client, m agent.SessionStart,
	log *slog.Logger) agent.Approval {
	type answer struct {
		ID, State, Reason, Mode string
		DecidedBy               string `json:"decidedBy"`
		Note                    string
	}
	hostname, _ := os.Hostname()
	var a answer
	if err := rep.Ask(ctx, "/api/v1/report/direct-login", map[string]any{
		"hostname":    hostname,
		"principal":   m.Principal,
		"client_addr": m.ClientAddr,
		"command":     m.Command,
		"auth":        m.Auth,
	}, &a); err != nil {
		log.Warn("direct login: cannot ask the control plane; refusing", "error", err)
		return agent.Approval{Reason: "Argus could not be reached to approve this login"}
	}
	for a.State == "pending" && a.ID != "" {
		var next answer
		err := rep.Ask(ctx, "/api/v1/report/direct-login/"+url.PathEscape(a.ID)+"/wait",
			map[string]any{}, &next)
		if ctx.Err() != nil {
			return agent.Approval{ID: a.ID, Reason: "nobody approved this login in time"}
		}
		if err != nil {
			// A dropped long poll is not an answer; ask again until the
			// deadline does decide.
			log.Warn("direct login: waiting for a decision failed; retrying", "error", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
			continue
		}
		next.ID = a.ID
		a = next
	}
	switch a.State {
	case "approved":
		if a.Mode == agent.DirectSSHAllow {
			return agent.Approval{Approved: true, Allowed: true}
		}
		return agent.Approval{Approved: true, ID: a.ID}
	case "expired":
		return agent.Approval{ID: a.ID, Reason: "nobody approved this login in time"}
	case "denied":
		reason := a.Reason
		if reason == "" {
			reason = "an admin denied this login"
			if a.DecidedBy != "" {
				reason = a.DecidedBy + " denied this login"
			}
			if a.Note != "" {
				reason += ": " + a.Note
			}
		}
		return agent.Approval{ID: a.ID, Reason: reason}
	}
	return agent.Approval{ID: a.ID, Reason: "Argus did not approve this login"}
}

// agentRiskFlags marks what the console highlights about a session the agent saw.
func agentRiskFlags(rec agent.SessionRecord) []string {
	flags := []string{}
	if rec.Origin == agent.Direct {
		flags = append(flags, "bypassed-gateway")
	}
	// Administrator is to Windows what root is to Linux.
	switch strings.ToLower(rec.Principal) {
	case "root", "administrator":
		flags = append(flags, "root-principal")
	}
	return append(flags, rec.Flags...)
}

func runShim(args []string) int {
	fs := flag.NewFlagSet("shim", flag.ExitOnError)
	socket := fs.String("socket", agent.SocketPath, "collector socket")
	// Accepted and ignored. An sshd_config written before the gateway was
	// retired still passes it, and a ForceCommand that fails on an unknown flag
	// refuses every login on the host.
	_ = fs.String("gateway-keys", "", "ignored: no longer used")
	_ = fs.String("gateways", "", "ignored: no longer used")
	_ = fs.Bool("skip-brokered", false, "ignored: no longer used")
	failOpen := fs.Bool("fail-open", false, "allow unrecorded sessions when the collector is down")
	breakGlass := fs.String("break-glass", strings.Join(agent.DefaultBreakGlassGroups, ","),
		`groups that may log in unrecorded while the agent is down (root always may); "none" turns it off`)
	shell := fs.String("shell", "", "override the login shell")
	_ = fs.Parse(args)

	var groups []string
	for _, g := range strings.Split(*breakGlass, ",") {
		if g = strings.TrimSpace(g); g != "" {
			groups = append(groups, g)
		}
	}
	return agent.RunShim(agent.ShimConfig{
		SocketPath:       *socket,
		FailOpen:         *failOpen,
		BreakGlassGroups: groups,
		Shell:            *shell,
	})
}

func runScan(args []string) int {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	shimPath := fs.String("shim", agent.DefaultShimPath, "path sshd should invoke")
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

	var canLogin []agent.LoginAccount
	for _, a := range p.LoginAccounts {
		if a.CanLogin {
			canLogin = append(canLogin, a)
		}
	}
	fmt.Printf("\n%d account(s) can log in over SSH:\n", len(canLogin))
	for _, a := range canLogin {
		fmt.Printf("  %-16s password %-8s keys %d\n", a.User, a.Password, a.Keys)
	}

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
			"leave no trace.")
		if shimSupported {
			fmt.Print(" Run `argus-agent install` to see what to change.\n")
		} else {
			fmt.Print(" Recording direct logins is not available on this platform\n" +
				"yet: stop sshd; sessions opened from the Argus console do not use it.\n")
		}
		return 1
	}
	return 0
}

func runInstall(args []string) int {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	shimPath := fs.String("shim", agent.DefaultShimPath, "path sshd will invoke")
	_ = fs.Parse(args)

	if !shimSupported {
		fmt.Fprintln(os.Stderr, "argus-agent install: direct SSH logins cannot be recorded "+
			"on this platform yet. Sessions opened from the Argus console are recorded; "+
			"to leave no unrecorded way in, stop sshd.")
		return 1
	}
	fmt.Printf(`# Argus host recording — append to /etc/ssh/sshd_config, then:
#   %s
#
# ForceCommand makes sshd run the recorder for EVERY session, including one
# opened with a standing key that never touched Argus. That is the point:
# without it, a bypass is invisible. Sessions opened from the Argus console do
# not use sshd at all; they reach the agent over its tunnel.

ForceCommand %s shim

# Tells the recorder how each direct login signed in -- a password, or which
# key -- so the console can show it on the approval and in Activity.
ExposeAuthInfo yes

# If the agent is ever stopped, root and members of sudo, wheel or admin can
# still log in (unrecorded, with a warning and a line in the auth log) so they
# can start it again. Change who with --break-glass=group1,group2, or turn it
# off with --break-glass=none, on the ForceCommand line.

# Recommended alongside it. These remove the ability to bypass rather than
# merely recording it, which is the stronger control:
#
#   PasswordAuthentication no      # a bypass that needs no key at all
#   PermitRootLogin no             # root is unattributable to a person
#   TrustedUserCAKeys /etc/ssh/argus_ca.pub   # lets standing keys be removed

# Verify with:
#   argus-agent scan

# Databases. If this server runs Postgres and you add its databases in the
# console, the agent signs people in with a Postgres role of their own, made
# as the postgres account over the local socket (peer authentication, no
# password stored anywhere). Those roles sign in with a one-time password that
# lasts for their session, so pg_hba.conf needs this line ABOVE any "local all
# all peer" line, then: pg_ctl reload (or systemctl reload postgresql)
#
#   local   all   +argus_people   scram-sha-256
`, reloadSSHDHint, *shimPath)
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

// maxJSONL is where a local log such as sessions.jsonl starts a new file.
// The previous one is kept as <path>.1, so the log takes at most twice this
// on disk however long the agent runs; the control plane holds the record.
const maxJSONL = 16 << 20

// logOutput is where the daemon logs: stdout for journald and launchd, a
// size-bounded file under the Windows service manager.
var logOutput io.Writer = os.Stdout

func appendJSONL(path string, v any, log *slog.Logger) {
	if err := os.MkdirAll(dirOf(path), 0o700); err != nil {
		return
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() >= maxJSONL {
		_ = os.Rename(path, path+".1")
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

// runProbe answers "can this host produce kernel evidence?" without starting
// the agent.
//
// Worth a command of its own because the answer decides what a recording from
// this host is worth in an audit, and an operator needs it before a session
// happens rather than after. It also exits non-zero when the answer is no, so a
// fleet check is one loop rather than log-scraping.
func runProbe() int {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	p, err := execlog.Open(context.Background(), log)
	if err != nil {
		fmt.Printf("kernel execution tracing: UNAVAILABLE\n")
		fmt.Printf("  fidelity  %s\n", execlog.FidelityPTY)
		fmt.Printf("  reason    %s\n", err)
		fmt.Println()
		fmt.Println("  Sessions on this host are recorded as terminal output only.")
		fmt.Println("  A user can obscure what they ran — base64, a sourced script, a")
		fmt.Println("  shell spawned from an editor — and none of it is distinguishable")
		fmt.Println("  from terminal output alone.")
		return 1
	}
	defer p.Close()

	fmt.Printf("kernel execution tracing: AVAILABLE\n")
	fmt.Printf("  fidelity  %s\n", execlog.FidelityEBPF)
	fmt.Println("  Commands are observed at execve, so a recording shows what ran")
	fmt.Println("  rather than what the terminal displayed.")
	return 0
}

func runEnroll(args []string) int {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	controlURL := fs.String("control-url", "", "argus-control base URL, as this host reaches it (https://)")
	token := fs.String("token", "", "single-use enrollment token from the console")
	pin := fs.String("pin", "", "sha256 pin of the control plane's key, shown beside the token")
	ca := fs.String("control-ca", "", "CA that signed the control plane's certificate; overrides --pin")
	hostname := fs.String("hostname", "", "name to enroll as; defaults to this host's name")
	stateDir := fs.String("state", agent.DefaultStateDir, "where the enrollment is kept")
	force := fs.Bool("force", false, "replace an existing enrollment (revoke the old one first)")
	_ = fs.Parse(args)

	if !isPrivileged() {
		fmt.Fprintln(os.Stderr, "argus-agent enroll: run as "+privilegedName+
			" -- the credential must not be readable by the users it records")
		return 1
	}
	// A token on the command line lands in shell history; reading it from the
	// environment is offered for anyone who would rather it did not.
	if *token == "" {
		*token = os.Getenv("ARGUS_ENROLL_TOKEN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	enr, err := agent.Enroll(ctx, agent.EnrollOptions{
		ControlURL: *controlURL, Token: *token, Pin: *pin, CAFile: *ca,
		Hostname: *hostname, StateDir: *stateDir, Force: *force,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "argus-agent enroll: %v\n", err)
		return 1
	}
	fmt.Printf("enrolled as %s with %s\n", enr.Hostname, enr.ControlURL)
	switch {
	case enr.CAFile != "":
		fmt.Printf("trusting the control plane through %s\n", enr.CAFile)
	case enr.Pin != "":
		fmt.Println("WARNING: trusting the control plane by its key alone. Reports will stop\n" +
			"when its key changes. Re-enroll with --control-ca to trust its CA instead.")
	}
	fmt.Println("start it with: " + startHint)
	return 0
}

// directSSHState is what the agent keeps of the direct SSH setting across a
// restart: the mode for every account, and the accounts the console names.
type directSSHState struct {
	Mode     string            `json:"mode"`
	Accounts map[string]string `json:"accounts,omitempty"`
}

// saveDirectSSHState writes the setting by rename, so a crash mid-write
// leaves the previous one rather than half of this one.
func saveDirectSSHState(path string, st directSSHState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// envOr is the environment variable's value, or def when it is unset or empty.
func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
