package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// rabbitMQGuide is Ubuntu's page on preparing RabbitMQ for a release
// upgrade, which do-release-upgrade's own refusal points to.
const rabbitMQGuide = "https://discourse.ubuntu.com/t/77271"

// rabbitMQBlocker is why Ubuntu's upgrader would refuse this server for
// its RabbitMQ, or "". The upgrader stops before changing anything when
// RabbitMQ is not ready: from 22.04 always (3.9 cannot go straight to
// 24.04's 3.12), and to 26.04 unless RabbitMQ is 3.13 with its feature
// flags enabled, since 26.04's 4.0 cannot read older data. Saying so on the
// card beats a run that stops on it.
func (c PackageChecker) rabbitMQBlocker(ctx context.Context, from, to string) string {
	b, _ := c.rabbitMQ(ctx, from, to)
	return b
}

// rabbitMQ is rabbitMQBlocker, and whether a run can prepare RabbitMQ
// itself (applyPrepareRabbitMQ): only on 24.04 going to 26.04, where the
// guide's steps are three and Argus can take them.
func (c PackageChecker) rabbitMQ(ctx context.Context, from, to string) (blocker string, prepare bool) {
	status, version := rabbitMQInstalled(ctx, c)
	if !strings.HasSuffix(status, " installed") || version == "" {
		return "", false
	}
	upstream := debUpstream(version)
	switch {
	case versionIs(from, "22.04"):
		return "RabbitMQ " + upstream + " is installed, and Ubuntu's upgrader will not move it to 24.04 by itself: " +
			"step it through 3.10 and 3.11 from Canonical's PPAs (ppa:canonical-server/rabbitmq-3.10-jammy, then " +
			"-3.11-jammy), running `sudo rabbitmqctl enable_feature_flag all` after each, as " + rabbitMQGuide +
			" describes. Back it up first. If nothing uses RabbitMQ, remove it instead (sudo apt remove rabbitmq-server)", false
	case !versionIs(to, "26.04"):
		return "", false
	case compareVersions(upstream, "3.13") < 0:
		return "RabbitMQ " + upstream + " is installed, and Ubuntu's upgrader stops unless it is 3.13 with all its " +
			"feature flags enabled. Back it up, then run `sudo add-apt-repository ppa:canonical-server/rabbitmq-3.13-noble`, " +
			"`sudo apt update && sudo apt upgrade` (the broker restarts) and `sudo rabbitmqctl enable_feature_flag all` " +
			"(it cannot be undone), and Check now. On a cluster, one node at a time; see " + rabbitMQGuide +
			". If nothing uses RabbitMQ, remove it instead (sudo apt remove rabbitmq-server). " +
			"Or press Prepare RabbitMQ, and Argus does these steps", versionIs(from, "24.04")
	}
	if _, err := c.Look("rabbitmqctl"); err != nil {
		return "", false
	}
	out, code, err := output(ctx, c.Run, aptEnv, "rabbitmqctl", "-q", "list_feature_flags", "name", "state", "stability")
	if err != nil || code != 0 {
		return "RabbitMQ " + upstream + " is installed, and Argus could not ask it which feature flags are enabled " +
			"(is it running?). Ubuntu's upgrader stops unless they all are: start it (sudo systemctl start " +
			"rabbitmq-server), run `sudo rabbitmqctl enable_feature_flag all`, and Check now", false
	}
	if off := disabledFeatureFlags(out); len(off) > 0 {
		return "RabbitMQ's feature flags " + strings.Join(off, ", ") + " are not enabled, and Ubuntu's upgrader stops " +
			"until they are: run `sudo rabbitmqctl enable_feature_flag all` (it cannot be undone), and Check now, " +
			"or press Prepare RabbitMQ", versionIs(from, "24.04")
	}
	return "", false
}

// disabledFeatureFlags reads `rabbitmqctl -q list_feature_flags name state
// stability`: the flags not enabled, leaving out experimental ones, which
// `enable_feature_flag all` does not turn on either.
func disabledFeatureFlags(b []byte) []string {
	var off []string
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] == "name" || f[1] == "enabled" {
			continue
		}
		if len(f) >= 3 && f[2] == "experimental" {
			continue
		}
		off = append(off, f[0])
	}
	return off
}

// debUpstream is a Debian version without its epoch and revision:
// 3.12.1-1ubuntu1 is 3.12.1.
func debUpstream(v string) string {
	if _, rest, ok := strings.Cut(v, ":"); ok {
		v = rest
	}
	if i := strings.LastIndex(v, "-"); i > 0 {
		v = v[:i]
	}
	return v
}

/* ── The helper: preparing RabbitMQ ──────────────────────────────────────── */

// rabbitMQPPA is Canonical's RabbitMQ 3.13 for 24.04, the step Ubuntu's
// guide gives before 26.04's 4.0.
const rabbitMQPPA = "ppa:canonical-server/rabbitmq-3.13-noble"

// rabbitMQWait is how long the broker gets to come back after its upgrade,
// and rabbitMQPause how long between asking. Replaced in tests.
var (
	rabbitMQWait  = 3 * time.Minute
	rabbitMQPause = func() { time.Sleep(5 * time.Second) }
)

// applyPrepareRabbitMQ takes the guide's steps on a single 24.04 node:
// RabbitMQ 3.13 from Canonical's PPA, then every stable feature flag on.
// It keeps the broker's definitions first, refuses a cluster (one node at a
// time is a person's call), and checks the upgrader would now let it by.
func (a *PackageApplier) applyPrepareRabbitMQ(ctx context.Context) PackageRunStatus {
	c := PackageChecker{Run: a.Run, Look: a.Look}
	nothing := func(msg string, next ...string) PackageRunStatus {
		if len(next) == 0 {
			next = []string{"Fix what it says on the server.", "Press Check for updates.", "Prepare RabbitMQ again."}
		}
		return a.failWith(msg, RunOutcome{Changed: ChangedNo, Why: msg, Next: next})
	}
	a.update(func(s *PackageRunStatus) {
		s.Step, s.Message = "preflight", "checking RabbitMQ can be prepared for the next Ubuntu release"
	})
	osr := osRelease()
	if osr["ID"] != "ubuntu" || !versionIs(osr["VERSION_ID"], "24.04") {
		return nothing("RabbitMQ is prepared from the console only on Ubuntu 24.04, for 26.04; this server is " +
			orUnknown(osr["PRETTY_NAME"]))
	}
	blocker, prepare := c.rabbitMQ(ctx, "24.04", "26.04")
	switch {
	case blocker == "":
		a.writeReportNow(ctx)
		return a.finish(RunDone, "RabbitMQ is already ready for Ubuntu 26.04; nothing was changed", nil)
	case !prepare:
		return nothing("not prepared, nothing was changed: " + blocker)
	}
	if _, err := a.Look("rabbitmqctl"); err != nil {
		return nothing("rabbitmqctl is not installed, so Argus cannot talk to RabbitMQ; nothing was changed")
	}
	if n, err := a.rabbitMQNodes(ctx); err != nil {
		return nothing("RabbitMQ did not answer (" + err.Error() + "), so nothing was changed: start it with " +
			"`sudo systemctl start rabbitmq-server` and try again")
	} else if n > 1 {
		return nothing(fmt.Sprintf("this RabbitMQ node is one of a cluster of %d, which is upgraded one node at a "+
			"time by hand; nothing was changed. Follow %s and RabbitMQ's rolling upgrade guide", n, rabbitMQGuide))
	}

	// Its definitions (users, vhosts, queues, policies) first: not the
	// messages, but what it takes to build the broker again.
	keep := filepath.Join(a.Plan.StateDir, packagesDir,
		"rabbitmq-definitions-"+a.Now().UTC().Format("20060102-150405")+".json")
	a.update(func(s *PackageRunStatus) { s.Step, s.Message = "backup", "keeping RabbitMQ's definitions in "+keep })
	if code, err := a.Run(ctx, a.lw, aptEnv, "rabbitmqctl", "-q", "export_definitions", keep); err != nil || code != 0 {
		return nothing(fmt.Sprintf("RabbitMQ would not export its definitions (exit %d%s), so nothing was changed",
			code, errSuffix(err)))
	}
	fmt.Fprintln(a.lw, "argus: kept RabbitMQ's definitions in "+keep)

	_, version := rabbitMQInstalled(ctx, c)
	if compareVersions(debUpstream(version), "3.13") < 0 {
		a.update(func(s *PackageRunStatus) {
			s.Step, s.Message = "install", "installing RabbitMQ 3.13 from "+rabbitMQPPA+"; the broker restarts"
		})
		if _, err := a.Look("add-apt-repository"); err != nil {
			if code, err := a.Run(ctx, a.lw, aptEnv, "apt-get", append(append([]string{"install", "-y"}, aptLock...),
				"software-properties-common")...); err != nil || code != 0 {
				return nothing("could not install software-properties-common for add-apt-repository; nothing else was changed")
			}
		}
		if code, err := a.Run(ctx, a.lw, aptEnv, "add-apt-repository", "-y", rabbitMQPPA); err != nil || code != 0 {
			return nothing(fmt.Sprintf("could not add %s (exit %d%s); nothing else was changed", rabbitMQPPA, code,
				errSuffix(err)))
		}
		args := append(append([]string{"install", "-y", "--only-upgrade",
			"-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold"}, aptLock...),
			"rabbitmq-server")
		if code, err := a.Run(ctx, a.lw, aptEnv, "apt-get", "update"); err != nil || code != 0 {
			return nothing(fmt.Sprintf("apt-get update failed after adding %s (exit %d%s); RabbitMQ was not touched",
				rabbitMQPPA, code, errSuffix(err)))
		}
		if code, err := a.Run(ctx, a.lw, aptEnv, "apt-get", args...); err != nil || code != 0 {
			msg := fmt.Sprintf("upgrading RabbitMQ from %s failed (exit %d%s)", rabbitMQPPA, code, errSuffix(err))
			return a.failWith(msg, RunOutcome{Changed: ChangedUnknown, Why: msg, Next: []string{
				"Run `sudo apt-get -f install` and `sudo systemctl status rabbitmq-server` on the server.",
				"Its definitions are kept in " + keep + "; `sudo rabbitmqctl import_definitions " + keep +
					"` puts them back.",
				"Press Check for updates, then Prepare RabbitMQ again."}})
		}
		_, version = rabbitMQInstalled(ctx, c)
		fmt.Fprintln(a.lw, "argus: rabbitmq-server is now "+version)
	}

	a.update(func(s *PackageRunStatus) {
		s.Step, s.Message = "feature_flags", "waiting for RabbitMQ, then enabling its feature flags"
	})
	if err := a.waitRabbitMQ(ctx); err != nil {
		msg := "RabbitMQ did not come back after its upgrade: " + err.Error()
		return a.failWith(msg, RunOutcome{Changed: ChangedYes, Why: msg, Next: []string{
			"Look at `sudo systemctl status rabbitmq-server` and `sudo journalctl -u rabbitmq-server`.",
			"Once it runs, press Prepare RabbitMQ again to enable its feature flags."}})
	}
	if code, err := a.Run(ctx, a.lw, aptEnv, "rabbitmqctl", "enable_feature_flag", "all"); err != nil || code != 0 {
		msg := fmt.Sprintf("RabbitMQ would not enable its feature flags (exit %d%s)", code, errSuffix(err))
		return a.failWith(msg, RunOutcome{Changed: ChangedYes, Why: msg, Next: []string{
			"Run `sudo rabbitmqctl list_feature_flags name state` to see which are off.",
			"Press Prepare RabbitMQ again."}})
	}
	a.writeReportNow(ctx)
	if b, _ := c.rabbitMQ(ctx, "24.04", "26.04"); b != "" {
		return a.failWith("RabbitMQ was upgraded, but is still not ready: "+b, RunOutcome{Changed: ChangedYes, Why: b})
	}
	return a.finish(RunDone, "RabbitMQ is on "+debUpstream(version)+" with its feature flags enabled, ready for "+
		"Ubuntu 26.04; its definitions are kept in "+keep, nil)
}

// writeReportNow writes a fresh report, so the card shows what changed.
func (a *PackageApplier) writeReportNow(ctx context.Context) {
	if m := detectManager(a.Look); m != nil {
		a.writeReport(ctx, m)
	}
}

// rabbitMQInstalled is rabbitmq-server's dpkg status and version.
func rabbitMQInstalled(ctx context.Context, c PackageChecker) (status, version string) {
	out, _, err := output(ctx, c.Run, aptEnv, "dpkg-query", "-W", "-f", "${Status}\t${Version}", "rabbitmq-server")
	if err != nil {
		return "", ""
	}
	status, version, _ = strings.Cut(strings.TrimSpace(string(out)), "\t")
	return status, version
}

// rabbitMQNodes is how many nodes the broker's cluster has, itself counted.
func (a *PackageApplier) rabbitMQNodes(ctx context.Context) (int, error) {
	out, code, err := output(ctx, a.Run, aptEnv, "rabbitmqctl", "-q", "cluster_status", "--formatter", "json")
	if err != nil {
		return 0, err
	}
	if code != 0 {
		return 0, fmt.Errorf("rabbitmqctl cluster_status exit %d", code)
	}
	var st struct {
		Disk []string `json:"disk_nodes"`
		RAM  []string `json:"ram_nodes"`
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return 0, fmt.Errorf("cannot read rabbitmqctl cluster_status: %w", err)
	}
	return len(st.Disk) + len(st.RAM), nil
}

// waitRabbitMQ waits for the broker to answer after its package restarted it.
func (a *PackageApplier) waitRabbitMQ(ctx context.Context) error {
	until := a.Now().Add(rabbitMQWait)
	for {
		_, code, err := output(ctx, a.Run, aptEnv, "rabbitmqctl", "-q", "status")
		if err == nil && code == 0 {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if a.Now().After(until) {
			return fmt.Errorf("it did not answer within %s", rabbitMQWait)
		}
		rabbitMQPause()
	}
}

func errSuffix(err error) string {
	if err == nil {
		return ""
	}
	return ": " + err.Error()
}
