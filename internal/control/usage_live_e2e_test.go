package control

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/agent"
)

/*
Live usage end to end: a console's server-sent events stream, through the
control plane and a real tunnel, to an agent sampling on its own clock.

	ARGUS_TEST_DATABASE_URL=postgres://argus:argus@localhost:5433/argus?sslmode=disable \
	  go test ./internal/control/ -run UsageLiveThroughTheAgent
*/

// sseEvents reads "event:"/"data:" pairs from a stream.
func sseEvents(t *testing.T, body io.Reader, want int, until time.Duration) (names []string, data []string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(body)
		var ev string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				ev = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: ") && ev != "":
				names, data = append(names, ev), append(data, strings.TrimPrefix(line, "data: "))
				if len(names) >= want || ev == "end" {
					return
				}
				ev = ""
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(until):
		t.Fatalf("after %v: %v", until, names)
	}
	return names, data
}

func TestUsageLiveThroughTheAgent(t *testing.T) {
	old := usageLiveEvery
	usageLiveEvery = time.Second
	t.Cleanup(func() { usageLiveEvery = old })
	e := newTunnelEnv(t)
	ctx := context.Background()
	host := unique("live") + ".example"
	cred, _ := enrolled(t, e.store, e.h, host)
	asset, err := e.store.CreateAsset(ctx, AssetInput{Hostname: host, Address: "192.0.2.41", Port: 22,
		Protocol: "ssh", OS: "Ubuntu 24.04", Principals: []string{"ops"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.store.ArchiveAsset(context.Background(), asset.ID) })

	open := func() *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1/assets/"+asset.ID+"/usage/live", nil)
		req.Header.Set("Authorization", "Bearer admin-token")
		res, err := e.srv.Client().Do(req)
		if err != nil || res.StatusCode != http.StatusOK {
			t.Fatalf("open: %v %v", res, err)
		}
		t.Cleanup(func() { res.Body.Close() })
		return res
	}

	// An agent too old for live figures: the console is told so, once.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	actx, stopOld := context.WithCancel(ctx)
	go agent.RunTunnel(actx, agent.TunnelConfig{Dial: e.dial(cred, host), Services: agent.DefaultTunnelServices(), Log: log})
	e.waitTunnel(t, host, "")
	names, data := sseEvents(t, open().Body, 1, 10*time.Second)
	if len(names) != 1 || names[0] != "end" || !strings.Contains(data[0], "agent too old") {
		t.Fatalf("old agent: %v %v", names, data)
	}
	stopOld()

	// A current one: samples every second, each a whole System-card usage.
	services := agent.DefaultTunnelServices()
	n := 0.0
	services[agent.UsageLiveService] = agent.UsageLive(agent.UsageLiveConfig{Log: log,
		Sample: func(*agent.UsageSampler) (agent.Usage, bool) {
			n++
			busy := n
			return agent.Usage{At: time.Now(), CPUBusy: &busy, MemTotal: 1000, MemAvailable: 400,
				Disks: []agent.Disk{{Mount: "/", FS: "ext4", Total: 100, Used: 96, Avail: 4}}}, true
		}})
	actx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go agent.RunTunnel(actx, agent.TunnelConfig{Dial: e.dial(cred, host), Services: services, Log: log})
	e.waitTunnel(t, host, "")
	// The replaced tunnel may still be listed for a moment; ask until the new one answers.
	var got []liveUsage
	for deadline := time.Now().Add(10 * time.Second); len(got) < 2 && time.Now().Before(deadline); {
		names, data = sseEvents(t, open().Body, 2, 10*time.Second)
		got = got[:0]
		for i, nm := range names {
			if nm == "usage" {
				var u liveUsage
				if err := json.Unmarshal([]byte(data[i]), &u); err != nil {
					t.Fatal(err)
				}
				got = append(got, u)
			}
		}
	}
	if len(got) < 2 {
		t.Fatalf("no samples: %v %v", names, data)
	}
	u := got[len(got)-1]
	if u.Usage == nil || u.Usage.CPUBusy == nil || u.Usage.MemTotal != 1000 || len(u.Usage.Disks) != 1 ||
		u.EverySeconds != 1 || len(u.Warnings) == 0 || u.Warnings[0].Resource != "disk" {
		t.Fatalf("sample: %+v", u)
	}
	if time.Since(u.UsageAt) > 5*time.Second {
		t.Fatalf("sample from %v is not live", u.UsageAt)
	}
}
