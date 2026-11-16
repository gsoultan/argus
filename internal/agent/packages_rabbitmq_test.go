package agent

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
)

func rabbitChecker(version string, flags string, flagsCode int) PackageChecker {
	sr := &scriptRun{on: map[string]func(io.Writer, []string) int{
		"do-release-upgrade -c": func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "New release '26.04.1 LTS' available.")
			return 0
		},
		"dpkg-query -W -f ${Status}\t${Version} rabbitmq-server": func(w io.Writer, _ []string) int {
			if version == "" {
				return 1
			}
			fmt.Fprint(w, "install ok installed\t"+version)
			return 0
		},
		"rabbitmqctl -q list_feature_flags": func(w io.Writer, _ []string) int {
			fmt.Fprint(w, flags)
			return flagsCode
		},
	}}
	return PackageChecker{Run: sr.run, Look: looks("apt-get", "dpkg-query", "do-release-upgrade", "rabbitmqctl")}
}

func rabbitBlockers(t *testing.T, c PackageChecker) string {
	t.Helper()
	info := c.release(context.Background(), PackageReport{})
	if info == nil || info.Next != "26.04" {
		t.Fatalf("info = %+v", info)
	}
	return strings.Join(info.Blockers, " | ")
}

func TestReleaseRabbitMQ(t *testing.T) {
	newReleaseHost(t, noble)
	ready := "name\tstate\tstability\nclassic_mirrored_queue_version\tenabled\tstable\n" +
		"khepri_db\tdisabled\texperimental\nstream_queue\tenabled\trequired\n"

	// Not installed, or 3.13 with every flag on: nothing to say.
	if got := rabbitBlockers(t, rabbitChecker("", "", 0)); got != "" {
		t.Errorf("no RabbitMQ: %s", got)
	}
	if got := rabbitBlockers(t, rabbitChecker("3.13.7-1~24.04.sav0", ready, 0)); got != "" {
		t.Errorf("ready: %s", got)
	}
	// 24.04's own 3.12: the PPA, the flags, and the guide.
	got := rabbitBlockers(t, rabbitChecker("3.12.1-1ubuntu1", ready, 0))
	for _, want := range []string{"RabbitMQ 3.12.1 is installed", "ppa:canonical-server/rabbitmq-3.13-noble",
		"enable_feature_flag all", rabbitMQGuide, "apt remove rabbitmq-server"} {
		if !strings.Contains(got, want) {
			t.Errorf("3.12: no %q in %s", want, got)
		}
	}
	// 3.13 with a stable flag off names it; experimental ones are left out.
	got = rabbitBlockers(t, rabbitChecker("3.13.7-1", ready+"message_containers\tdisabled\tstable\n", 0))
	if !strings.Contains(got, "feature flags message_containers are not enabled") || strings.Contains(got, "khepri") {
		t.Errorf("flag off: %s", got)
	}
	// A broker that does not answer cannot be vouched for.
	got = rabbitBlockers(t, rabbitChecker("3.13.7-1", "Error: unable to perform an operation on node", 69))
	if !strings.Contains(got, "could not ask it") {
		t.Errorf("not running: %s", got)
	}
}

func TestReleaseRabbitMQFromJammy(t *testing.T) {
	newReleaseHost(t, jammy)
	c := rabbitChecker("3.9.27-0ubuntu0.1", "", 0)
	c.Run = (&scriptRun{on: map[string]func(io.Writer, []string) int{
		"do-release-upgrade -c": func(w io.Writer, _ []string) int {
			fmt.Fprintln(w, "New release '24.04.1 LTS' available.")
			return 0
		},
		"dpkg-query -W -f ${Status}\t${Version} rabbitmq-server": func(w io.Writer, _ []string) int {
			fmt.Fprint(w, "install ok installed\t3.9.27-0ubuntu0.1")
			return 0
		},
	}}).run
	info := c.release(context.Background(), PackageReport{})
	if got := strings.Join(info.Blockers, " | "); !strings.Contains(got, "RabbitMQ 3.9.27 is installed") ||
		!strings.Contains(got, "rabbitmq-3.10-jammy") {
		t.Errorf("jammy: %s", got)
	}
}

func TestDebUpstream(t *testing.T) {
	for in, want := range map[string]string{"3.12.1-1ubuntu1": "3.12.1", "1:3.13.7-1~24.04": "3.13.7", "4.0.5": "4.0.5"} {
		if got := debUpstream(in); got != want {
			t.Errorf("debUpstream(%q) = %q", in, got)
		}
	}
}

// rabbitHost is a 24.04 node whose RabbitMQ moves as the run upgrades it.
func rabbitHost(t *testing.T, nodes int) (*scriptRun, *string, *bool) {
	newReleaseHost(t, noble)
	oldPause := rabbitMQPause
	rabbitMQPause = func() {}
	t.Cleanup(func() { rabbitMQPause = oldPause })
	version, flagsOn := "3.12.1-1ubuntu1", false
	sr := &scriptRun{on: map[string]func(io.Writer, []string) int{
		"dpkg-query -W -f ${Status}\t${Version} rabbitmq-server": func(w io.Writer, _ []string) int {
			fmt.Fprint(w, "install ok installed\t"+version)
			return 0
		},
		"rabbitmqctl -q cluster_status": func(w io.Writer, _ []string) int {
			ns := make([]string, nodes)
			for i := range ns {
				ns[i] = fmt.Sprintf("%q", fmt.Sprintf("rabbit@node%d", i))
			}
			fmt.Fprintf(w, `{"disk_nodes":[%s],"running_nodes":[]}`, strings.Join(ns, ","))
			return 0
		},
		"apt-get install -y --only-upgrade": func(io.Writer, []string) int {
			version = "3.13.7-1~24.04"
			return 0
		},
		"rabbitmqctl enable_feature_flag all": func(io.Writer, []string) int {
			flagsOn = true
			return 0
		},
		"rabbitmqctl -q list_feature_flags": func(w io.Writer, _ []string) int {
			state := "disabled"
			if flagsOn {
				state = "enabled"
			}
			fmt.Fprintf(w, "message_containers\t%s\tstable\nkhepri_db\tdisabled\texperimental\n", state)
			return 0
		},
	}}
	return sr, &version, &flagsOn
}

func rabbitApplier(t *testing.T, sr *scriptRun) *PackageApplier {
	a, _ := releaseApplier(t, sr, looks("apt-get", "dpkg-query", "rabbitmqctl", "add-apt-repository"), "")
	a.Plan.Mode = PackagesPrepareRabbitMQ
	return a
}

func TestPrepareRabbitMQ(t *testing.T) {
	sr, version, flagsOn := rabbitHost(t, 1)
	st := rabbitApplier(t, sr).Apply(context.Background())
	if st.State != RunDone || !strings.Contains(st.Message, "RabbitMQ is on 3.13.7 with its feature flags enabled") ||
		*version != "3.13.7-1~24.04" || !*flagsOn {
		t.Fatalf("run = %+v", st)
	}
	order := []string{"rabbitmqctl -q export_definitions", "add-apt-repository -y " + rabbitMQPPA, "apt-get update",
		"apt-get install -y --only-upgrade", "rabbitmqctl -q status", "rabbitmqctl enable_feature_flag all"}
	i := 0
	for _, c := range sr.calls {
		if i < len(order) && strings.HasPrefix(c, order[i]) {
			i++
		}
	}
	if i != len(order) {
		t.Errorf("steps out of order or missing at %q: %q", order[min(i, len(order)-1)], sr.calls)
	}
}

func TestPrepareRabbitMQRefusesACluster(t *testing.T) {
	sr, _, _ := rabbitHost(t, 3)
	st := rabbitApplier(t, sr).Apply(context.Background())
	if st.State != RunFailed || !strings.Contains(st.Message, "cluster of 3") || st.Outcome == nil ||
		st.Outcome.Changed != ChangedNo || sr.ran("add-apt-repository") || sr.ran("rabbitmqctl -q export_definitions") {
		t.Fatalf("run = %+v", st)
	}
}

func TestPrepareRabbitMQAlreadyReady(t *testing.T) {
	sr, version, flagsOn := rabbitHost(t, 1)
	*version, *flagsOn = "3.13.7-1", true
	st := rabbitApplier(t, sr).Apply(context.Background())
	if st.State != RunDone || !strings.Contains(st.Message, "already ready") || sr.ran("add-apt-repository") {
		t.Fatalf("run = %+v", st)
	}
}

func TestPrepareRabbitMQOffered(t *testing.T) {
	newReleaseHost(t, noble)
	info := rabbitChecker("3.12.1-1ubuntu1", "", 0).release(context.Background(), PackageReport{})
	if !info.PrepareRabbitMQ {
		t.Errorf("3.12 on 24.04: not offered: %+v", info)
	}
	info = rabbitChecker("3.13.7-1", "", 69).release(context.Background(), PackageReport{})
	if info.PrepareRabbitMQ || len(info.Blockers) != 1 {
		t.Errorf("broker down: %+v", info)
	}
}
