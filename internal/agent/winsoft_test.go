package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Software on Windows, tested on any host: the registry is a list, the
programs Windows runs are a fake.
*/

const chromeGUID = "{8A69D345-D564-463C-AFF1-A69D9E530F96}"

func winFixture() []WinEntry {
	return []WinEntry{
		{Key: "7-Zip", View: "64", DisplayName: "7-Zip 23.01 (x64)", DisplayVersion: "23.01", Publisher: "Igor Pavlov",
			UninstallString: `"C:\Program Files\7-Zip\Uninstall.exe"`, QuietUninstallString: `"C:\Program Files\7-Zip\Uninstall.exe" /S`},
		{Key: chromeGUID, View: "32", DisplayName: "Acme Reporter", DisplayVersion: "4.2.0", Publisher: "Acme",
			WindowsInstaller: true, UninstallString: "MsiExec.exe /X" + chromeGUID, InstallDate: "20260301", EstimatedSizeKB: 51200},
		// The same product in the other view is listed once.
		{Key: chromeGUID, View: "64", DisplayName: "Acme Reporter", DisplayVersion: "4.2.0", WindowsInstaller: true},
		{Key: "Notepad++", View: "64", DisplayName: "Notepad++ (64-bit x64)", DisplayVersion: "8.6",
			UninstallString: `C:\Program Files\Notepad++\uninstall.exe`},
		{Key: "Evil", View: "64", DisplayName: "Evil Tool", QuietUninstallString: `cmd.exe /c rd /s /q C:\`},
		{Key: "Argus", View: "64", DisplayName: "Argus Agent", QuietUninstallString: `"C:\Program Files\Argus\un.exe" /S`},
		{Key: "{F0000000-0000-0000-0000-000000000001}", View: "64", DisplayName: "Microsoft Edge", WindowsInstaller: true},
		{Key: "KB5034441", View: "64", DisplayName: "Security Update for Windows", ReleaseType: "Security Update"},
		{Key: "Hidden", View: "64", DisplayName: "A part", SystemComponent: true},
		{Key: "Patch", View: "64", DisplayName: "A patch", ParentKeyName: "Office"},
		{Key: "NoName", View: "64"},
	}
}

func TestWindowsListsProgramsAndWhatCanBeRemoved(t *testing.T) {
	res := WinSoftwareAsker{Inventory: func() ([]WinEntry, error) { return winFixture(), nil }}.Ask(context.Background(),
		tunnel.GrantSoftware{Op: tunnel.SoftwareInstalled})
	if !res.OK || res.Manager != tunnel.ManagerWindows {
		t.Fatalf("%+v", res)
	}
	got := map[string]tunnel.InstalledPackage{}
	for _, p := range res.Installed {
		got[p.Label] = p
		if !tunnel.ValidPackageName(p.Name) {
			t.Errorf("%s has an id the console cannot send back: %q", p.Label, p.Name)
		}
	}
	if len(res.Installed) != 6 {
		t.Fatalf("listed %d: %+v", len(res.Installed), res.Installed)
	}
	acme := got["Acme Reporter"]
	if acme.Name != "msi-8a69d345-d564-463c-aff1-a69d9e530f96" || acme.Protected != "" || acme.SizeKB != 51200 ||
		acme.InstalledAt == 0 || acme.Origin != "Acme" {
		t.Errorf("acme = %+v", acme)
	}
	for name, want := range map[string]string{
		"7-Zip 23.01 (x64)":      "",
		"Notepad++ (64-bit x64)": "asks questions",
		"Evil Tool":              "runs a shell",
		"Argus Agent":            "the Argus agent",
		"Microsoft Edge":         "need it",
	} {
		if p := got[name]; (want == "") != (p.Protected == "") || !strings.Contains(p.Protected, want) {
			t.Errorf("%s protected = %q, want %q", name, p.Protected, want)
		}
	}
	// No repository search.
	if r := (WinSoftwareAsker{Inventory: func() ([]WinEntry, error) { return nil, nil }}).Ask(context.Background(),
		tunnel.GrantSoftware{Op: tunnel.SoftwareSearch, Query: "chrome"}); r.OK || !strings.Contains(r.Error, "library") {
		t.Errorf("search = %+v", r)
	}
}

func TestWindowsUninstallCommands(t *testing.T) {
	cases := []struct {
		e    WinEntry
		want string
	}{
		{WinEntry{Key: chromeGUID, WindowsInstaller: true}, "msiexec.exe /x " + chromeGUID + " /qn /norestart"},
		{WinEntry{Key: "x", UninstallString: "MsiExec.exe /I" + strings.ToLower(chromeGUID)}, "msiexec.exe /x " + chromeGUID + " /qn /norestart"},
		{WinEntry{Key: "x", QuietUninstallString: `"C:\Program Files\7-Zip\Uninstall.exe" /S`}, `C:\Program Files\7-Zip\Uninstall.exe /S`},
		{WinEntry{Key: "x", QuietUninstallString: `C:\Program Files\Vendor App\unins000.exe /SILENT /NORESTART`},
			`C:\Program Files\Vendor App\unins000.exe /SILENT /NORESTART`},
	}
	for _, c := range cases {
		argv, err := winUninstallArgv(c.e)
		if err != nil || strings.Join(argv, " ") != c.want {
			t.Errorf("%+v: %q %v, want %q", c.e, argv, err, c.want)
		}
	}
	for _, q := range []string{`powershell.exe -c "Remove-Item C:\ -Recurse"`, `uninstall.exe /S`, `"C:\x\setup.bat" /S`,
		`"C:\Windows\System32\cmd.exe" /c del x`} {
		if _, err := winUninstallArgv(WinEntry{Key: "x", QuietUninstallString: q}); err == nil {
			t.Errorf("%s was accepted", q)
		}
	}
	if got := splitWinCommand(`"C:\a b\c.exe" "x \"y\"" z\\`); strings.Join(got, "|") != `C:\a b\c.exe|x "y"|z\\` {
		t.Errorf("split = %q", got)
	}
}

// winHost is a Windows server whose programs change as installers run.
type winHost struct {
	mu      sync.Mutex
	entries []WinEntry
	calls   []string
	envs    [][]string
	exit    int
	onRun   func(cmd string)
}

func (h *winHost) inventory() ([]WinEntry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]WinEntry(nil), h.entries...), nil
}

func (h *winHost) run(_ context.Context, out io.Writer, env []string, name string, args ...string) (int, error) {
	cmd := strings.Join(append([]string{name}, args...), " ")
	h.mu.Lock()
	h.calls = append(h.calls, cmd)
	h.envs = append(h.envs, env)
	on := h.onRun
	h.mu.Unlock()
	switch {
	case strings.HasPrefix(cmd, "sc.exe"):
		_, _ = io.WriteString(out, "SERVICE_NAME: Spooler\nSERVICE_NAME: CDPUserSvc_4a7b1\n")
		return 0, nil
	case strings.HasPrefix(cmd, "netstat.exe"):
		_, _ = io.WriteString(out, "  Proto  Local Address  Foreign Address  State\n  TCP    0.0.0.0:135  0.0.0.0:0  LISTENING\n")
		return 0, nil
	}
	if on != nil {
		on(cmd)
	}
	if out != nil {
		_, _ = io.WriteString(out, "ran "+name+"\n")
	}
	return h.exit, nil
}

func (h *winHost) remove(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var keep []WinEntry
	for _, e := range h.entries {
		if e.Key != key {
			keep = append(keep, e)
		}
	}
	h.entries = keep
}

func winApplier(t *testing.T, h *winHost, req PackageRunRequest) *PackageApplier {
	t.Helper()
	old := winScriptFolders
	winScriptFolders = func() []string { return nil }
	t.Cleanup(func() { winScriptFolders = old })
	req.ID = "run-00000009"
	return &PackageApplier{
		Plan:           PackagePlan{PackageRunRequest: req, StateDir: t.TempDir(), Unit: "argus-packages-test"},
		PackageChecker: PackageChecker{Run: h.run, Look: func(string) (string, error) { return "", os.ErrNotExist }, Now: time.Now},
		BootID:         func() string { return "" },
		Windows:        true, WinInventory: h.inventory,
	}
}

func TestWindowsRemovesWhatWasPreviewedAndNothingElse(t *testing.T) {
	h := &winHost{entries: winFixture()}
	apps := winApps(winFixture())
	item := tunnel.SoftwareItem{Name: "msi-8a69d345-d564-463c-aff1-a69d9e530f96", Label: "Acme Reporter"}
	pv, _ := winPreview(apps, tunnel.SoftwareRemove, []tunnel.SoftwareItem{item})
	if pv.Blocked != "" || len(pv.Changes) != 1 || pv.Changes[0].Label != "Acme Reporter" {
		t.Fatalf("preview = %+v", pv)
	}
	h.onRun = func(cmd string) {
		if strings.HasPrefix(cmd, "msiexec.exe /x "+chromeGUID) {
			h.remove(chromeGUID)
		}
	}
	a := winApplier(t, h, PackageRunRequest{Mode: PackagesRemove, Items: []tunnel.SoftwareItem{item}, Expect: pv.Hash})
	st := a.Apply(context.Background())
	if st.State != RunDone || len(st.Upgraded) != 1 || st.Upgraded[0].Name != "Acme Reporter" || st.Upgraded[0].To != "" {
		t.Fatalf("%s %q %+v\n%s", st.State, st.Message, st.Upgraded, strings.Join(h.calls, "\n"))
	}

	// A preview of something since changed stops before running anything.
	h2 := &winHost{entries: winFixture()}
	h2.entries[1].DisplayVersion = "4.3.0"
	a = winApplier(t, h2, PackageRunRequest{Mode: PackagesRemove, Items: []tunnel.SoftwareItem{item}, Expect: pv.Hash})
	if st := a.Apply(context.Background()); st.State != RunFailed || !strings.Contains(st.Message, "changed since") {
		t.Fatalf("%s %q", st.State, st.Message)
	}
	for _, c := range h2.calls {
		if strings.HasPrefix(c, "msiexec") {
			t.Errorf("ran %s", c)
		}
	}

	// A protected program is blocked in the preview and in the run.
	argus := tunnel.SoftwareItem{Name: winID(WinEntry{Key: "Argus", View: "64"})}
	pv, _ = winPreview(apps, tunnel.SoftwareRemove, []tunnel.SoftwareItem{argus})
	if !strings.Contains(pv.Blocked, "the Argus agent") {
		t.Errorf("argus preview = %+v", pv)
	}
}

func TestWindowsInstallsALibraryFileOnlyWhenItIsTheOneShown(t *testing.T) {
	h := &winHost{entries: winFixture()}
	h.onRun = func(cmd string) {
		if strings.HasPrefix(cmd, "msiexec.exe /i") {
			h.mu.Lock()
			h.entries = append(h.entries, WinEntry{Key: "{11111111-2222-3333-4444-555555555555}", View: "64",
				DisplayName: "Field Reports", DisplayVersion: "1.0.0", WindowsInstaller: true})
			h.mu.Unlock()
		}
	}
	body := []byte("MSI bytes")
	sum := sha256.Sum256(body)
	dir := t.TempDir()
	fetch := func(_ context.Context, path string, w io.Writer, limit int64) (int64, error) {
		n, err := io.Copy(w, bytes.NewReader(body))
		return n, err
	}
	o := &FileOrder{Kind: "msi", Name: "Field Reports", Path: "/api/v1/report/software-file/run-00000009",
		SHA256: hex.EncodeToString(sum[:]), Size: int64(len(body)), Args: []string{"ALLUSERS=1"}}
	path, err := fetchLibraryFile(context.Background(), fetch, dir, "run-00000009", o)
	if err != nil {
		t.Fatal(err)
	}
	f := *o
	f.LocalPath = path
	a := winApplier(t, h, PackageRunRequest{Mode: PackagesFileInstall, File: &f})
	st := a.Apply(context.Background())
	if st.State != RunDone || st.Script == nil || len(st.Script.Packages) != 1 || st.Script.Packages[0].Name != "Field Reports" {
		t.Fatalf("%s %q %+v\n%s", st.State, st.Message, st.Script, strings.Join(h.calls, "\n"))
	}
	if c := h.calls[2]; !strings.HasPrefix(c, "msiexec.exe /i "+path+" /qn /norestart /l* ") || !strings.HasSuffix(c, " ALLUSERS=1") {
		t.Errorf("ran %q", c)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the file was left on the server")
	}

	// A different file is refused before anything runs.
	o2 := *o
	o2.SHA256 = strings.Repeat("0", 64)
	if _, err := fetchLibraryFile(context.Background(), fetch, dir, "run-00000010", &o2); err == nil ||
		!strings.Contains(err.Error(), "SHA-256") {
		t.Errorf("err = %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "file-*")); len(left) != 0 {
		t.Errorf("left %v", left)
	}

	// Arguments are switches and properties, never a command.
	for _, bad := range []FileOrder{
		{Kind: "exe", Args: []string{"/S & del x"}},
		{Kind: "msi", Args: []string{`TARGETDIR="C:\x"`}},
		{Kind: "bat"},
	} {
		b := bad
		b.Path, b.SHA256, b.Size = o.Path, o.SHA256, 1
		if b.validate() == nil {
			t.Errorf("%+v was accepted", b)
		}
	}

	// A failing installer says why in Windows Installer's words.
	h.exit = 1618
	f2 := *o
	path, _ = fetchLibraryFile(context.Background(), fetch, dir, "run-00000011", o)
	f2.LocalPath = path
	a = winApplier(t, h, PackageRunRequest{Mode: PackagesFileInstall, File: &f2})
	if st := a.Apply(context.Background()); st.State != RunFailed || !strings.Contains(st.Message, "another installation") {
		t.Errorf("%s %q", st.State, st.Message)
	}
}

const installPs1 = "$v = $env:ARGUS_PARAM_VERSION\nStart-Process msiexec -ArgumentList '/i', \"C:\\x\\acme-$v.msi\", '/qn' -Wait\n"

func TestWindowsRunsAReviewedPowerShellScript(t *testing.T) {
	e := newScriptEnv(t)
	appr := e.appr
	appr.Platform, appr.InstallSHA, appr.CheckSHA = tunnel.ScriptWindows, tunnel.ScriptHash(installPs1), ""
	o := e.order(t, appr, installPs1)
	o.Check = ""
	h := &winHost{entries: winFixture()}
	var script string
	h.onRun = func(cmd string) {
		if strings.HasPrefix(cmd, "powershell.exe") {
			f := cmd[strings.LastIndex(cmd, " ")+1:]
			b, _ := os.ReadFile(f)
			script = string(b)
		}
	}
	a := winApplier(t, h, PackageRunRequest{Mode: PackagesScript, Script: o})
	a.Plan.StateDir = e.dir
	st := a.Apply(context.Background())
	if st.State != RunDone {
		t.Fatalf("%s %q\n%s", st.State, st.Message, strings.Join(h.calls, "\n"))
	}
	cmd := ""
	for i, c := range h.calls {
		if strings.HasPrefix(c, "powershell.exe") {
			cmd = c
			if env := strings.Join(h.envs[i], " "); !strings.Contains(env, "ARGUS_PARAM_VERSION=2.1.0") ||
				!strings.Contains(env, "ARGUS_PARAM_TOKEN=") {
				t.Errorf("env = %s", env)
			}
		}
	}
	if !strings.Contains(cmd, "-NoProfile -NonInteractive -ExecutionPolicy Bypass -File ") {
		t.Errorf("ran %q", cmd)
	}
	if !strings.HasPrefix(script, "\ufeff"+winScriptPrelude) || !strings.HasSuffix(script, installPs1) {
		t.Errorf("script = %q", script)
	}
	// The script and its parameters are gone afterwards.
	if left, _ := filepath.Glob(filepath.Join(e.dir, packagesDir, "script-*")); len(left) != 0 {
		t.Errorf("left %v", left)
	}

	// A Linux script never runs on Windows.
	h = &winHost{entries: winFixture()}
	a = winApplier(t, h, PackageRunRequest{Mode: PackagesScript, Script: e.order(t, e.appr, installSh)})
	a.Plan.StateDir = e.dir
	a.Plan.Script.Check = checkSh
	if st := a.Apply(context.Background()); st.State != RunFailed || !strings.Contains(st.Message, "not Windows") {
		t.Errorf("%s %q", st.State, st.Message)
	}
}

// winPlatform is a Windows-like platform for the daemon.
type winPlatform struct {
	mu       sync.Mutex
	launched []string
}

func (*winPlatform) supported() string { return "no updates here" }
func (*winPlatform) installs() []string {
	return []string{PackagesRemove, PackagesScript, PackagesFileInstall}
}
func (*winPlatform) check(context.Context) (PackageReport, error) { return PackageReport{}, nil }
func (w *winPlatform) launch(_ context.Context, plan, _ string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.launched = append(w.launched, plan)
	return nil
}
func (*winPlatform) active(string) bool           { return true }
func (*winPlatform) repair(context.Context) error { return nil }
func (*winPlatform) bootID() string               { return "" }

func TestTheDaemonFetchesALibraryFileBeforeStartingTheRun(t *testing.T) {
	plat := &winPlatform{}
	p := &Packages{StateDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), platform: plat, now: time.Now}
	b := p.Beat()
	if b.Supported || strings.Join(b.Installs, ",") != "remove,script,file_install" {
		t.Fatalf("beat = %+v", b)
	}
	body := []byte("EXE bytes")
	sum := sha256.Sum256(body)
	done := make(chan struct{})
	p.Fetch = func(_ context.Context, _ string, w io.Writer, _ int64) (int64, error) {
		n, err := io.Copy(w, bytes.NewReader(body))
		return n, err
	}
	p.Changed = func() {}
	go func() {
		for {
			plat.mu.Lock()
			n := len(plat.launched)
			plat.mu.Unlock()
			if n > 0 {
				close(done)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	p.Handle(context.Background(), &PackageOrders{Run: &PackageRunRequest{ID: "run-00000012", Mode: PackagesFileInstall,
		File: &FileOrder{Kind: "exe", Name: "Vendor tool", Path: "/api/v1/report/software-file/run-00000012",
			SHA256: hex.EncodeToString(sum[:]), Size: int64(len(body)), Args: []string{"/S"}}}})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("never launched: %+v", p.Beat().Run)
	}
	raw, err := os.ReadFile(plat.launched[0])
	if err != nil || !strings.Contains(string(raw), `"local_path"`) {
		t.Fatalf("plan = %s %v", raw, err)
	}
	if st := p.Beat().Run; st == nil || st.Unit == "" || st.State != RunStarting {
		t.Errorf("run = %+v", st)
	}
	// An update run is still refused here.
	p.Handle(context.Background(), &PackageOrders{Run: &PackageRunRequest{ID: "run-00000013", Mode: PackagesAll}})
}
