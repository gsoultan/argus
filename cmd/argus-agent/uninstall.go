package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	xterm "golang.org/x/term"

	"github.com/gsoultan/argus/internal/agent"
	"github.com/gsoultan/argus/internal/reporter"
)

/*
Taking the agent off a server.

	stop the service ─▶ ship what is still queued ─▶ take out the sshd hook
	      ─▶ revoke this host's credential ─▶ delete it ─▶ unregister the service

The order is the point. The service stops first so it seals every open
recording and nothing new is queued behind the flush. Anything that has not
reached the control plane stops the uninstall and the agent is started again,
because after the credential is revoked nothing here can ship it. The sshd hook
goes before anything that cannot be undone: a ForceCommand left pointing at a
binary that is about to be removed refuses every SSH login, and the machine's
own console becomes the only way back in.

Recordings, the local session log and the report spool stay where they are.
Removing the software must never destroy the evidence it produced -- that is
what someone covering their tracks would reach for -- so deleting them is left
to a person, deliberately, with the path printed.
*/

// secretStateFiles are what an uninstall deletes from the state directory:
// the credential, what it trusts, and state that only meant something while
// the agent ran. Everything else in there is evidence and is kept.
var secretStateFiles = []string{
	agent.EnrollmentFile,
	"control-ca.pem",
	agent.GrantKeyFile,
	"console-only",
	"direct-ssh",
	"heartbeat.json",
	"posture.json",
	"facts.json",
}

func runUninstall(args []string) int {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	stateDir := fs.String("state", agent.DefaultStateDir, "the agent's state directory")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	force := fs.Bool("force", false,
		"uninstall even when recordings or reports have not reached the control plane "+
			"(they stay on this host), or when nobody could log in over SSH afterwards")
	_ = fs.Parse(args)

	if !isPrivileged() {
		fmt.Fprintln(os.Stderr, "argus-agent uninstall: run as "+privilegedName)
		return 1
	}
	enr, err := agent.LoadEnrollment(*stateDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "argus-agent uninstall: the enrollment could not be read: %v\n", err)
		return 1
	}
	hostname, _ := os.Hostname()
	if enr != nil {
		hostname = enr.Hostname
	}
	// Before anything changes: once the agent is gone, plain SSH is how people
	// get in, and a host where nobody can is one only its console reaches.
	ways, known := remainingSSHWaysIn()
	if known && len(ways) == 0 && !*force {
		fmt.Fprintln(os.Stderr, "argus-agent uninstall: once the agent is gone nobody could "+
			"log in to this host over SSH: no account has a key in authorized_keys and "+
			"password logins are off. Add a key for an account (or turn on "+
			"PasswordAuthentication), then run this again, or pass --force to uninstall "+
			"anyway. Nothing was changed.")
		return 1
	}
	if !*yes && !confirm(hostname, enr, ways) {
		fmt.Fprintln(os.Stderr, "argus-agent uninstall: nothing was changed")
		return 1
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	step("stopping the agent, so it seals its open recordings")
	if err := stopAgent(); err != nil {
		fmt.Fprintf(os.Stderr, "argus-agent uninstall: the agent did not stop: %v\n", err)
		return 1
	}
	// From here on, a refusal puts the host back as it was.
	abort := func(format string, a ...any) int {
		fmt.Fprintf(os.Stderr, "argus-agent uninstall: "+format+"\n", a...)
		if err := startAgent(); err != nil {
			fmt.Fprintf(os.Stderr, "the agent could not be started again either: %v\n"+
				"start it with: %s\n", err, startHint)
		} else {
			fmt.Fprintln(os.Stderr, "the agent has been started again; nothing was removed")
		}
		return 1
	}

	var rep *reporter.Client
	if enr != nil {
		tlsCfg, err := enr.TLS()
		if err != nil {
			return abort("the enrollment's TLS configuration failed: %v", err)
		}
		rep = reporter.NewWithTLS(enr.ControlURL, enr.Credential,
			filepath.Join(*stateDir, "report-spool.jsonl"), tlsCfg, log)
		step("shipping what has not reached the control plane yet")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		rep.Drain(ctx)
		recordings := agent.NewUploads(*stateDir, rep, log).Pass(ctx)
		cancel()
		if reports := rep.Spooled(); (reports > 0 || recordings > 0) && !*force {
			return abort("%d recording(s) and %d report(s) have not reached the control "+
				"plane at %s, and once this host's credential is revoked nothing can send "+
				"them. Fix the connection and run this again, or pass --force to leave them "+
				"on this host only.", recordings, reports, enr.ControlURL)
		} else if reports > 0 || recordings > 0 {
			warn("%d recording(s) and %d report(s) never reached the control plane; "+
				"they stay in %s", recordings, reports, *stateDir)
		}
	}

	step("taking the agent out of sshd's configuration")
	hooks, left, err := removeSSHDHook()
	if err != nil {
		return abort("%v", err)
	}
	for _, h := range hooks {
		fmt.Println("  " + h)
	}

	// Nothing below can be undone, and nothing below stops the uninstall.
	if rep != nil {
		step("revoking this host's credential")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := rep.Deregister(ctx, map[string]string{"hostname": hostname, "ran_by": ranBy()})
		cancel()
		if err != nil {
			warn("the control plane could not be told (%v)", err)
			left = append(left, "Revoke "+hostname+" in the console, under Servers > Add a "+
				"server > Enrolled agents: the control plane still lists it as enrolled.")
		}
	}

	step("deleting the credential and the agent's working state")
	for _, name := range secretStateFiles {
		p := filepath.Join(*stateDir, name)
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			warn("%s could not be deleted: %v", p, err)
			left = append(left, "Delete "+p+".")
		}
	}

	step("unregistering the service")
	more, err := removeAgentService()
	if err != nil {
		warn("%v", err)
	}
	left = append(left, more...)

	fmt.Printf("\nargus-agent is uninstalled from %s.\n", hostname)
	if len(ways) > 0 {
		fmt.Println("Plain SSH works for: " + strings.Join(ways, ", ") + ".")
	}
	fmt.Printf("\nKept, deliberately: recordings and the session log in %s.\n"+
		"Removing the software must never destroy the evidence it produced. Delete\n"+
		"them yourself once you are sure they are no longer needed.\n", *stateDir)
	if len(left) > 0 {
		fmt.Println("\nStill to do:")
		for _, l := range left {
			fmt.Println("  - " + l)
		}
	}
	return 0
}

// confirm asks the person at the terminal. Without one, --yes is required:
// a script that meant to uninstall says so.
func confirm(hostname string, enr *agent.Enrollment, ways []string) bool {
	if !xterm.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintln(os.Stderr, "argus-agent uninstall: no terminal to confirm on; pass --yes")
		return false
	}
	fmt.Printf("This takes argus-agent off %s:\n", hostname)
	fmt.Println("  - sessions can no longer be opened to it from the console")
	if shimSupported {
		fmt.Println("  - direct SSH logins are no longer refused or recorded")
	}
	if enr != nil {
		fmt.Printf("  - its credential with %s is revoked\n", enr.ControlURL)
	}
	if len(ways) > 0 {
		fmt.Println("Plain SSH will work for: " + strings.Join(ways, ", "))
	}
	fmt.Println("Recordings stay on this host.")
	fmt.Print("Uninstall? [y/N] ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

// ranBy is who asked, as far as this host can tell: the account sudo was run
// from, or the one running this. The control plane records it as the host's
// claim.
func ranBy() string {
	if u := os.Getenv("SUDO_USER"); u != "" {
		return u
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}

func step(msg string) { fmt.Println("- " + msg) }

func warn(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "  warning: "+format+"\n", a...)
}
