package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

/*
Finding out that a newer agent is out.

The console offers each agent a version to update to. Before this it was only
the control plane's own version, so a new agent reached servers only after
someone upgraded the control plane. The release watcher asks GitHub for the
project's releases every few hours and keeps the newest one that has agent
packages and checksums to download; the console offers whichever is newer,
that or its own version.

With agent_releases_url pointing at a mirror, GitHub's list says nothing
about what the mirror holds, so it is not asked unless agent_releases_api
names the mirror's own list (in GitHub's shape); without one the console
offers its own version, as before. A check that fails (no internet, rate limited) changes nothing: the
last release found stays, and the console still offers its own version.
*/

// DefaultAgentReleasesAPI lists the project's releases.
const DefaultAgentReleasesAPI = "https://api.github.com/repos/gsoultan/argus/releases?per_page=30"

// AgentRelease is a published release with agent packages to update to.
type AgentRelease struct {
	Version     string    `json:"version"`
	PublishedAt time.Time `json:"publishedAt"`
	// Page is the release's page, for its notes.
	Page string `json:"page"`
}

// ReleaseWatcher keeps the newest agent release.
type ReleaseWatcher struct {
	// API lists releases the way GitHub does; empty is DefaultAgentReleasesAPI.
	API    string
	Client *http.Client
	// Every is how often it asks; zero is six hours.
	Every time.Duration
	Log   *slog.Logger

	// asking is held through a check someone asked for, so a burst of
	// clicks asks GitHub once.
	asking sync.Mutex

	mu      sync.Mutex
	latest  *AgentRelease
	control *AgentRelease // the newest with control plane packages
	checked time.Time
	failure string
}

// LatestControl is the newest release with control plane packages, for the
// console's Updates page. Nil until one is found.
func (w *ReleaseWatcher) LatestControl() *AgentRelease {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.control == nil {
		return nil
	}
	r := *w.control
	return &r
}

// Latest is the newest release found, and when the watcher last asked.
func (w *ReleaseWatcher) Latest() (*AgentRelease, time.Time, string) {
	if w == nil {
		return nil, time.Time{}, ""
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.latest == nil {
		return nil, w.checked, w.failure
	}
	r := *w.latest
	return &r, w.checked, w.failure
}

// Run checks now and then every Every until ctx ends.
func (w *ReleaseWatcher) Run(ctx context.Context) {
	every := w.Every
	if every <= 0 {
		every = 6 * time.Hour
	}
	for {
		if err := w.Check(ctx); err != nil && ctx.Err() == nil {
			w.log().Warn("cannot check for a newer agent release; the console offers its own version",
				"error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

func (w *ReleaseWatcher) log() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.Default()
}

// Check asks for the releases once.
func (w *ReleaseWatcher) Check(ctx context.Context) error {
	r, c, err := w.fetch(ctx)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.checked = time.Now().UTC()
	if err != nil {
		w.failure = err.Error()
		return err
	}
	w.failure = ""
	if r != nil && (w.latest == nil || r.Version != w.latest.Version) {
		w.log().Info("newest agent release", "version", r.Version)
	}
	if r != nil {
		w.latest = r
	}
	if c != nil {
		w.control = c
	}
	return nil
}

// manualCheckGap is how recent a check may be for one someone asks for to
// reuse it instead of asking again. GitHub allows an unauthenticated address
// sixty requests an hour.
const manualCheckGap = 15 * time.Second

// CheckNow is Check for someone who asked from the console: it asks again
// unless a check finished within manualCheckGap, and concurrent asks share
// one request. It returns that check's failure, if any.
func (w *ReleaseWatcher) CheckNow(ctx context.Context) error {
	w.asking.Lock()
	defer w.asking.Unlock()
	w.mu.Lock()
	recent, failure := !w.checked.IsZero() && time.Since(w.checked) < manualCheckGap, w.failure
	w.mu.Unlock()
	if recent {
		if failure != "" {
			return errors.New(failure)
		}
		return nil
	}
	return w.Check(ctx)
}

type ghRelease struct {
	Tag        string    `json:"tag_name"`
	Draft      bool      `json:"draft"`
	Prerelease bool      `json:"prerelease"`
	Published  time.Time `json:"published_at"`
	Page       string    `json:"html_url"`
	Assets     []struct {
		Name string `json:"name"`
	} `json:"assets"`
}

func (w *ReleaseWatcher) fetch(ctx context.Context) (*AgentRelease, *AgentRelease, error) {
	api := w.API
	if api == "" {
		api = DefaultAgentReleasesAPI
	}
	client := w.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "argus-control")
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("%s answered %s", api, resp.Status)
	}
	var list []ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&list); err != nil {
		return nil, nil, fmt.Errorf("reading the releases: %w", err)
	}
	return newestAgentRelease(list), newestRelease(list, "argus-control_"), nil
}

// newestAgentRelease picks the highest release an agent can update to: not
// a draft or a pre-release, tagged v<version> (the agent downloads from
// <url>/v<version>/), and carrying agent packages and checksums.txt. A
// release whose build failed has no packages, so it is passed over rather
// than offered to agents that would fail to download it.
func newestAgentRelease(list []ghRelease) *AgentRelease {
	return newestRelease(list, "argus-agent_")
}

// newestRelease is newestAgentRelease for the packages named prefix.
func newestRelease(list []ghRelease, prefix string) *AgentRelease {
	var best *AgentRelease
	for _, r := range list {
		if r.Draft || r.Prerelease || !strings.HasPrefix(r.Tag, "v") {
			continue
		}
		v := strings.TrimPrefix(r.Tag, "v")
		if !reAgentVer.MatchString(v) || strings.Contains(v, "-") {
			continue
		}
		sums, pkgs := false, false
		for _, a := range r.Assets {
			switch {
			case a.Name == "checksums.txt":
				sums = true
			case strings.HasPrefix(a.Name, prefix+v+"_linux_"):
				pkgs = true
			}
		}
		if !sums || !pkgs {
			continue
		}
		if best == nil || versionLess(best.Version, v) {
			best = &AgentRelease{Version: v, PublishedAt: r.Published, Page: r.Page}
		}
	}
	return best
}

// versionLess reports whether release a comes before b: 1.9.0 < 1.10.0, and
// 1.2.0-rc.1 < 1.2.0. Anything not shaped like a release sorts first.
func versionLess(a, b string) bool {
	pa, ra, oka := splitVersion(a)
	pb, rb, okb := splitVersion(b)
	if !oka || !okb {
		return !oka && okb
	}
	for i := range 3 {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	switch {
	case ra == rb:
		return false
	case ra == "":
		return false
	case rb == "":
		return true
	}
	return ra < rb
}

func splitVersion(v string) ([3]int, string, bool) {
	var out [3]int
	v = strings.TrimPrefix(v, "v")
	if !reAgentVer.MatchString(v) {
		return out, "", false
	}
	core, pre, _ := strings.Cut(v, "-")
	for i, p := range strings.SplitN(core, ".", 3) {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, "", false
		}
		out[i] = n
	}
	return out, pre, true
}
