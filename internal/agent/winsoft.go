package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Software on Windows servers.

Windows has no repository Argus searches, so a Windows server installs from
the library (an MSI or an EXE someone added, by its SHA-256) or with a reviewed
PowerShell script, and removes what its own "Apps" list shows: the programs in
the Uninstall keys of the registry, both views.

Reading the list and working out a removal happen in the agent, which runs as
LocalSystem; the change itself is a package run like on Linux, done by
`argus-agent packages-apply` in a process of its own, detached from the
service so it goes on when the agent restarts.

A removal only ever runs what Windows itself would: msiexec /x for a Windows
Installer product, or the program's own QuietUninstallString. A program whose
only uninstaller asks questions is not offered, because nothing would answer
them. Nothing here goes through cmd.exe or PowerShell's parser.

This file has no build tag so it is tested everywhere; the registry reader is
in winsoft_windows.go.
*/

// WinEntry is one Uninstall key as the registry holds it.
type WinEntry struct {
	// Key is the key's name under ...\Uninstall; View is "64" or "32".
	Key  string
	View string

	DisplayName          string
	DisplayVersion       string
	Publisher            string
	InstallDate          string // yyyymmdd
	EstimatedSizeKB      int64
	UninstallString      string
	QuietUninstallString string
	WindowsInstaller     bool
	SystemComponent      bool
	ParentKeyName        string
	ReleaseType          string
}

// winApp is an entry the console may list.
type winApp struct {
	WinEntry
	ID string
}

// winInventory reads the Uninstall keys; the real one is in
// winsoft_windows.go, tests pass their own.
type winInventory func() ([]WinEntry, error)

// hostWinInventory is this host's reader, nil off Windows.
var hostWinInventory winInventory

var reGUID = regexp.MustCompile(`^\{[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}\}$`)

// winID is the name the console uses for a program: the product code for a
// Windows Installer product, which stays the same across machines, else a
// hash of where the key is.
func winID(e WinEntry) string {
	if code := msiProductCode(e); code != "" {
		return "msi-" + strings.ToLower(strings.Trim(code, "{}"))
	}
	h := sha256.Sum256([]byte(e.View + `\` + strings.ToLower(e.Key)))
	return "app-" + hex.EncodeToString(h[:8])
}

// msiProductCode is the entry's product code when Windows Installer owns it.
func msiProductCode(e WinEntry) string {
	if reGUID.MatchString(e.Key) && (e.WindowsInstaller || msiFromCommand(e.UninstallString) != "") {
		return e.Key
	}
	return ""
}

var reMsiCommand = regexp.MustCompile(`(?i)^"?(?:[a-z]:\\windows\\system32\\)?msiexec(?:\.exe)?"?\s+/[ix]\s*(\{[0-9a-f-]{36}\})\s*$`)

// msiFromCommand reads "MsiExec.exe /X{GUID}" (or /I, which many products
// write though it means the same here).
func msiFromCommand(s string) string {
	m := reMsiCommand.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return ""
	}
	return strings.ToUpper(m[1])
}

// winApps is what the console lists: programs, not their updates or the
// parts Windows hides itself.
func winApps(entries []WinEntry) []winApp {
	seen := map[string]bool{}
	var out []winApp
	for _, e := range entries {
		e.DisplayName = strings.TrimSpace(e.DisplayName)
		switch {
		case e.DisplayName == "", e.SystemComponent, e.ParentKeyName != "":
			continue
		case strings.EqualFold(e.ReleaseType, "Update"), strings.EqualFold(e.ReleaseType, "Hotfix"),
			strings.EqualFold(e.ReleaseType, "Security Update"):
			continue
		}
		id := winID(e)
		if seen[id] {
			continue // the same product in both views
		}
		seen[id] = true
		out = append(out, winApp{WinEntry: e, ID: id})
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].DisplayName) < strings.ToLower(out[j].DisplayName)
	})
	return out
}

var reWinProtected = regexp.MustCompile(`(?i)^(microsoft edge|microsoft edge webview2|microsoft \.net|microsoft windows desktop runtime|` +
	`microsoft asp\.net core|microsoft visual c\+\+ \d{4}.*redistributable|windows |microsoft update health|openssh)`)

// winProtection is why the console must not offer to remove a program.
func winProtection(e WinEntry) string {
	n := strings.ToLower(e.DisplayName)
	switch {
	case strings.HasPrefix(n, "argus"):
		return "the Argus agent"
	case reWinProtected.MatchString(e.DisplayName):
		return "Windows, or other programs, need it"
	}
	if _, err := winUninstallArgv(e); err != nil {
		return err.Error()
	}
	return ""
}

// winUninstallArgv is what removes a program without asking anyone.
func winUninstallArgv(e WinEntry) ([]string, error) {
	if code := msiProductCode(e); code != "" {
		return []string{"msiexec.exe", "/x", code, "/qn", "/norestart"}, nil
	}
	if code := msiFromCommand(e.UninstallString); code != "" {
		return []string{"msiexec.exe", "/x", code, "/qn", "/norestart"}, nil
	}
	q := strings.TrimSpace(e.QuietUninstallString)
	if q == "" {
		return nil, errors.New("its uninstaller asks questions; remove it from Windows Settings")
	}
	argv := splitWinCommand(q)
	if len(argv) == 0 {
		return nil, errors.New("its uninstall command cannot be read")
	}
	exe := strings.ToLower(filepath.Base(strings.ReplaceAll(argv[0], `\`, "/")))
	switch {
	case exe == "cmd" || exe == "powershell" || exe == "cmd.exe", exe == "powershell.exe", exe == "pwsh.exe", exe == "wscript.exe", exe == "cscript.exe",
		exe == "mshta.exe":
		return nil, errors.New("its uninstall command runs a shell, which Argus does not run for it")
	case !winAbsolute(argv[0]):
		return nil, errors.New("its uninstall command does not name a program by its full path")
	case !strings.HasSuffix(exe, ".exe"):
		return nil, errors.New("its uninstall command is not a program")
	}
	for _, a := range argv {
		if strings.ContainsAny(a, "\x00\r\n") {
			return nil, errors.New("its uninstall command cannot be read")
		}
	}
	return argv, nil
}

func winAbsolute(p string) bool {
	return len(p) > 3 && ((p[0] >= 'A' && p[0] <= 'Z') || (p[0] >= 'a' && p[0] <= 'z')) && p[1] == ':' && (p[2] == '\\' || p[2] == '/')
}

// splitWinCommand splits a command line the way the C runtime does for
// argv: double quotes group, backslashes escape only before a quote. An
// unquoted program path with spaces (C:\Program Files\X\un.exe /S, which
// some installers write) is read up to the first ".exe".
func splitWinCommand(s string) []string {
	s = strings.TrimSpace(s)
	var argv []string
	if s != "" && s[0] != '"' {
		if i := strings.Index(strings.ToLower(s), ".exe"); i > 0 && strings.Contains(s[:i], " ") {
			argv = append(argv, s[:i+4])
			s = strings.TrimSpace(s[i+4:])
		}
	}
	var cur strings.Builder
	inQuote, have := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\':
			n := 0
			for i < len(s) && s[i] == '\\' {
				n++
				i++
			}
			if i < len(s) && s[i] == '"' {
				cur.WriteString(strings.Repeat(`\`, n/2))
				if n%2 == 1 {
					cur.WriteByte('"')
				} else {
					inQuote = !inQuote
				}
				have = true
			} else {
				cur.WriteString(strings.Repeat(`\`, n))
				i--
				have = true
			}
		case c == '"':
			inQuote, have = !inQuote, true
		case (c == ' ' || c == '\t') && !inQuote:
			if have {
				argv = append(argv, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteByte(c)
			have = true
		}
	}
	if have {
		argv = append(argv, cur.String())
	}
	return argv
}

// winInstalled is the list as the console reads it.
func winInstalled(apps []winApp) []tunnel.InstalledPackage {
	out := make([]tunnel.InstalledPackage, 0, len(apps))
	for _, a := range apps {
		p := tunnel.InstalledPackage{Name: a.ID, Label: a.DisplayName, Version: a.DisplayVersion,
			Origin: a.Publisher, SizeKB: a.EstimatedSizeKB, Manual: true, Protected: winProtection(a.WinEntry)}
		if t, err := time.Parse("20060102", strings.TrimSpace(a.InstallDate)); err == nil {
			p.InstalledAt = t.Unix()
		}
		out = append(out, p)
	}
	return out
}

// WinSoftwareAsker answers the software service on Windows.
type WinSoftwareAsker struct {
	Inventory winInventory
}

// Ask answers one question.
func (s WinSoftwareAsker) Ask(_ context.Context, q tunnel.GrantSoftware) tunnel.SoftwareResult {
	res := tunnel.SoftwareResult{Manager: tunnel.ManagerWindows}
	if err := q.Validate(); err != nil {
		res.Error = err.Error()
		return res
	}
	entries, err := s.Inventory()
	if err != nil {
		res.Error = "cannot read the installed programs: " + err.Error()
		return res
	}
	apps := winApps(entries)
	switch q.Op {
	case tunnel.SoftwareInstalled:
		res.Installed = winInstalled(apps)
		res.Total, res.Manual = len(res.Installed), len(res.Installed)
		if len(res.Installed) > tunnel.MaxSoftwareInstalled {
			res.Installed, res.Truncated = res.Installed[:tunnel.MaxSoftwareInstalled], true
		}
		res.OK = true
	case tunnel.SoftwareSearch:
		res.Error = "Windows has no repository Argus searches; install from an MSI or EXE in the library, or a reviewed script"
	case tunnel.SoftwarePreview:
		pv, _ := winPreview(apps, q.Action, q.Items)
		res.Preview, res.OK = &pv, true
	}
	return res
}

// winPreview is what removing the items would do: those programs go, and
// nothing else is touched. Installs come from library files, not previews.
func winPreview(apps []winApp, action string, items []tunnel.SoftwareItem) (tunnel.SoftwarePreviewResult, []winApp) {
	pv := tunnel.SoftwarePreviewResult{Action: action, Items: items, Changes: []tunnel.SoftwareChange{}}
	if action != tunnel.SoftwareRemove {
		pv.Blocked = "Windows has no repository Argus installs from; install from an MSI or EXE in the library, or a reviewed script"
		pv.Hash = changesHash(pv)
		return pv, nil
	}
	byID := map[string]winApp{}
	for _, a := range apps {
		byID[a.ID] = a
	}
	var targets []winApp
	for _, it := range items {
		a, ok := byID[it.Name]
		if !ok {
			pv.Blocked = fmt.Sprintf("%s is not installed on this server any more", label(it))
			continue
		}
		if why := winProtection(a.WinEntry); why != "" && pv.Blocked == "" {
			pv.Blocked = fmt.Sprintf("%s cannot be removed from the console: %s", a.DisplayName, why)
		}
		pv.Changes = append(pv.Changes, tunnel.SoftwareChange{Kind: tunnel.ChangeRemove, Name: a.ID,
			Label: a.DisplayName, From: a.DisplayVersion, Asked: true})
		targets = append(targets, a)
	}
	pv.PurgeAllowed = false
	pv.Warnings = []string{"Each program's own uninstaller runs, quietly. What it leaves behind (settings, data folders) is up to it."}
	pv.Hash = changesHash(pv)
	return pv, targets
}

func label(it tunnel.SoftwareItem) string {
	if it.Label != "" {
		return it.Label
	}
	return it.Name
}

/* ── Library files ───────────────────────────────────────────────────────── */

// FileOrder is a library file to install, as a run carries it.
type FileOrder struct {
	// Kind is msi or exe.
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	// Args are what the library says to pass: MSI properties
	// (NAME=value), or an EXE's silent switches.
	Args []string `json:"args,omitempty"`
	// LocalPath is where the daemon put the file; set in the plan only.
	LocalPath string `json:"local_path,omitempty"`
}

var (
	reMsiProperty        = regexp.MustCompile(`^[A-Z][A-Z0-9_.]{0,63}=[^"\s]{0,200}$`)
	reExeArg             = regexp.MustCompile(`^[A-Za-z0-9/\-][A-Za-z0-9/\\\-_.:=+,@]{0,199}$`)
	maxLibraryFile int64 = 2 << 30
)

func (o *FileOrder) validate() error {
	switch {
	case o == nil:
		return errors.New("the run carries no file")
	case o.Kind != "msi" && o.Kind != "exe":
		return fmt.Errorf("%q is not a file Argus installs", o.Kind)
	case !strings.HasPrefix(o.Path, "/api/v1/report/"):
		return errors.New("the run does not say where to fetch the file")
	case !reHash.MatchString(o.SHA256), o.Size <= 0, o.Size > maxLibraryFile:
		return errors.New("the run does not say which file it is")
	case len(o.Args) > 20:
		return errors.New("the file has too many arguments")
	}
	for _, a := range o.Args {
		if (o.Kind == "msi" && !reMsiProperty.MatchString(a)) || (o.Kind == "exe" && !reExeArg.MatchString(a)) {
			return fmt.Errorf("%q is not an argument Argus passes", a)
		}
	}
	return nil
}

// fetchLibraryFile downloads o into dir and keeps it only when it is the
// file the console named.
func fetchLibraryFile(ctx context.Context, fetch AppFetch, dir, id string, o *FileOrder) (string, error) {
	if fetch == nil {
		return "", errors.New("this agent cannot fetch files; update it")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "file-"+id+"."+o.Kind)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := fetch(ctx, o.Path, io.MultiWriter(f, h), o.Size)
	cerr := f.Close()
	switch {
	case err != nil:
		_ = os.Remove(path)
		return "", fmt.Errorf("the download failed: %w; nothing was installed", err)
	case cerr != nil:
		_ = os.Remove(path)
		return "", cerr
	case n != o.Size || hex.EncodeToString(h.Sum(nil)) != o.SHA256:
		_ = os.Remove(path)
		return "", errors.New("the downloaded file is not the one the console showed (its SHA-256 differs); nothing was installed")
	}
	return path, nil
}

/* ── The run, in the helper ──────────────────────────────────────────────── */

// Windows Installer's exit codes that are not failures.
const (
	winOK             = 0
	winRebootRequired = 3010
	winRebootStarted  = 1641
	winNotInstalled   = 1605
)

func winExitMeaning(code int) (ok, reboot bool, why string) {
	switch code {
	case winOK:
		return true, false, ""
	case winRebootRequired, winRebootStarted:
		return true, true, ""
	case 1602:
		return false, false, "someone cancelled it on the server"
	case 1603:
		return false, false, "the installer hit a fatal error (exit 1603); its log says why"
	case 1618:
		return false, false, "another installation is in progress on the server; ask again once it ends"
	case 1619:
		return false, false, "the installer could not open the package"
	case 1638:
		return false, false, "another version of this product is already installed"
	}
	return false, false, fmt.Sprintf("it ended with exit code %d", code)
}

func (a *PackageApplier) applyWindows(ctx context.Context) PackageRunStatus {
	switch a.Plan.Mode {
	case PackagesRemove:
		return a.applyWinRemove(ctx)
	case PackagesFileInstall:
		return a.applyWinFile(ctx)
	case PackagesScript:
		return a.applyWinScript(ctx)
	}
	return a.fail("operating system updates from the console are for Linux servers")
}

func (a *PackageApplier) winApps() ([]winApp, error) {
	if a.WinInventory == nil {
		return nil, errors.New("this agent cannot read the installed programs")
	}
	e, err := a.WinInventory()
	if err != nil {
		return nil, err
	}
	return winApps(e), nil
}

func (a *PackageApplier) applyWinRemove(ctx context.Context) PackageRunStatus {
	if err := tunnel.ValidateSoftwareChange(a.Plan.Mode, a.Plan.Items, false); err != nil {
		return a.fail(err.Error())
	}
	a.update(func(s *PackageRunStatus) {
		s.Step, s.Message = "simulate", "checking the server still answers what was approved"
	})
	apps, err := a.winApps()
	if err != nil {
		return a.fail(err.Error())
	}
	pv, targets := winPreview(apps, tunnel.SoftwareRemove, a.Plan.Items)
	if pv.Hash != a.Plan.Expect {
		return a.fail("the server has changed since this was previewed: removing it now would not do what was approved. " +
			"Nothing was changed; preview it again.")
	}
	if pv.Blocked != "" {
		return a.fail(pv.Blocked)
	}
	before := a.winPicture(ctx)
	reboot := false
	var failed []string
	for i, t := range targets {
		argv, _ := winUninstallArgv(t.WinEntry)
		a.update(func(s *PackageRunStatus) {
			s.Step, s.Planned = "remove", len(targets)
			s.Message = fmt.Sprintf("removing %s (%d of %d)", t.DisplayName, i+1, len(targets))
		})
		fmt.Fprintf(a.lw, "> %s\n", strings.Join(argv, " "))
		code, err := a.Run(ctx, a.lw, nil, argv[0], argv[1:]...)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", t.DisplayName, err))
			continue
		}
		ok, rb, why := winExitMeaning(code)
		if code == winNotInstalled {
			ok = true
		}
		reboot = reboot || rb
		if !ok {
			failed = append(failed, t.DisplayName+": "+why)
		}
	}
	a.lw.Flush()
	after := a.winPicture(ctx)
	report := &tunnel.ScriptReport{}
	before.diff(after, report)
	more := func(s *PackageRunStatus) {
		s.Upgraded, s.RebootRequired = reportChanges(report), reboot
	}
	if len(failed) > 0 {
		return a.finish(RunFailed, "not removed: "+strings.Join(failed, "; "), more)
	}
	return a.finish(RunDone, fmt.Sprintf("removed %s", strings.Join(itemLabels(a.Plan.Items), ", ")), more)
}

func itemLabels(items []tunnel.SoftwareItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, label(it))
	}
	return out
}

func reportChanges(r *tunnel.ScriptReport) []PackageChange {
	changed := []PackageChange{}
	for _, c := range r.Packages {
		changed = append(changed, PackageChange{Name: c.Name, From: c.From, To: c.To})
	}
	return changed
}

func (a *PackageApplier) applyWinFile(ctx context.Context) PackageRunStatus {
	o := a.Plan.File
	if err := o.validate(); err != nil {
		return a.fail(err.Error())
	}
	if o.LocalPath == "" {
		return a.fail("the file was not downloaded; nothing was installed")
	}
	defer os.Remove(o.LocalPath)
	a.update(func(s *PackageRunStatus) {
		s.Step, s.Message = "check", "checking the file is the one the console showed"
	})
	if sum, err := fileSHA256(o.LocalPath); err != nil || sum != o.SHA256 {
		return a.fail("the file on the server is not the one the console showed; nothing was installed")
	}
	before := a.winPicture(ctx)
	var argv []string
	logPath := filepath.Join(filepath.Dir(o.LocalPath), "msi-"+a.Plan.ID+".log")
	if o.Kind == "msi" {
		argv = append([]string{"msiexec.exe", "/i", o.LocalPath, "/qn", "/norestart", "/l*", logPath}, o.Args...)
	} else {
		argv = append([]string{o.LocalPath}, o.Args...)
	}
	a.update(func(s *PackageRunStatus) { s.Step, s.Message = "install", "installing "+o.Name })
	fmt.Fprintf(a.lw, "> %s\n", strings.Join(argv, " "))
	code, err := a.Run(ctx, a.lw, nil, argv[0], argv[1:]...)
	ok, reboot, why := winExitMeaning(code)
	if b, rerr := os.ReadFile(logPath); rerr == nil {
		if !ok {
			// The end of Windows Installer's own log says why it failed.
			fmt.Fprintf(a.lw, "\n--- Windows Installer log (end) ---\n%s\n", tail([]byte(utf16ToString(b)), 8<<10))
		}
		_ = os.Remove(logPath)
	}
	a.lw.Flush()
	after := a.winPicture(ctx)
	report := &tunnel.ScriptReport{Exit: code}
	before.diff(after, report)
	more := func(s *PackageRunStatus) {
		s.Script, s.Upgraded, s.RebootRequired = report, reportChanges(report), reboot
	}
	switch {
	case err != nil:
		return a.finish(RunFailed, "the installer could not be started: "+err.Error(), more)
	case !ok:
		return a.finish(RunFailed, o.Name+" was not installed: "+why, more)
	}
	return a.finish(RunDone, fmt.Sprintf("%s finished: %s", o.Name, scriptSummary(report)), more)
}

// winScriptPrelude is put before a reviewed PowerShell script: stop at the
// first error, as bash -e does on Linux, and keep progress bars out of the
// log. It is not part of what was reviewed and never changes.
const winScriptPrelude = "$ErrorActionPreference = 'Stop'\r\n$ProgressPreference = 'SilentlyContinue'\r\n"

func (a *PackageApplier) applyWinScript(ctx context.Context) PackageRunStatus {
	o := a.Plan.Script
	if err := o.validate(); err != nil {
		return a.fail(err.Error())
	}
	var secrets []string
	for _, n := range o.Secret {
		if v := o.Params[n]; len(v) >= 3 {
			secrets = append(secrets, v)
		}
	}
	mask := func(s string) string {
		for _, v := range secrets {
			s = strings.ReplaceAll(s, v, "••••••")
		}
		return s
	}
	a.lw.flush = func(s string) { a.flushLog(mask(s)) }
	a.update(func(s *PackageRunStatus) { s.Step, s.Message = "verify", "checking the script is the one reviewed" })
	appr, err := a.verifyScript(o)
	if err != nil {
		return a.fail(err.Error() + " Nothing was run.")
	}
	if appr.RunAs != "" {
		return a.fail("scripts on Windows run as LocalSystem; this version names an account to run as. Nothing was run.")
	}
	timeout := o.timeout()
	if max := time.Duration(appr.TimeoutSec) * time.Second; max > 0 && timeout > max {
		timeout = max
	}
	a.update(func(s *PackageRunStatus) { s.Step, s.Message = "before", "noting what is on the server before" })
	before := a.winPicture(ctx)
	dir := filepath.Join(a.Plan.StateDir, packagesDir, "script-"+a.Plan.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return a.fail("cannot prepare the script: " + err.Error())
	}
	defer os.RemoveAll(dir)
	verb := map[string]string{tunnel.ScriptInstall: "installing", tunnel.ScriptUninstall: "uninstalling"}[o.Action]
	a.update(func(s *PackageRunStatus) {
		s.Step, s.Message = "script", fmt.Sprintf("%s with %s v%d", verb, scriptName(appr), appr.Version)
	})
	code, err := a.runPowerShell(ctx, dir, "run", o.Text, o.Params, timeout, a.lw)
	a.lw.Flush()
	report := &tunnel.ScriptReport{Exit: code}
	if o.Check != "" {
		a.update(func(s *PackageRunStatus) { s.Step, s.Message = "check", "running the check script" })
		var out bytes.Buffer
		cc, cerr := a.runPowerShell(ctx, dir, "check", o.Check, o.Params, scriptCheckLimit, &out)
		report.Checked = true
		switch {
		case cerr != nil:
			report.CheckError = cerr.Error()
		case cc == 0:
			report.Installed = true
			report.CheckVersion = scriptFirstLine(mask(out.String()), 100)
		}
	}
	a.update(func(s *PackageRunStatus) { s.Step, s.Message = "after", "noting what changed" })
	after := a.winPicture(ctx)
	before.diff(after, report)
	more := func(s *PackageRunStatus) {
		s.Script, s.Upgraded, s.RebootRequired = report, reportChanges(report), code == winRebootRequired
	}
	switch {
	case err != nil:
		return a.finish(RunFailed, "the script could not be started: "+err.Error(), more)
	case code != 0 && code != winRebootRequired:
		return a.finish(RunFailed, fmt.Sprintf("the script ended with exit code %d", code), more)
	case report.Checked && o.Action == tunnel.ScriptInstall && !report.Installed:
		return a.finish(RunFailed, "the script finished, but its check says it is not installed", more)
	case report.Checked && o.Action == tunnel.ScriptUninstall && report.Installed:
		return a.finish(RunFailed, "the script finished, but its check says it is still installed", more)
	}
	return a.finish(RunDone, fmt.Sprintf("%s v%d finished: %s", scriptName(appr), appr.Version, scriptSummary(report)), more)
}

// runPowerShell runs text with Windows PowerShell: no profile, nobody to
// answer a prompt, parameters in the environment only.
func (a *PackageApplier) runPowerShell(ctx context.Context, dir, name, text string, params map[string]string,
	timeout time.Duration, out io.Writer) (int, error) {
	file := filepath.Join(dir, name+".ps1")
	// A BOM, so Windows PowerShell 5.1 reads the file as UTF-8.
	body := "\ufeff" + winScriptPrelude + text
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		return -1, err
	}
	env := []string{"ARGUS_RUN_ID=" + a.Plan.ID}
	for k, v := range params {
		env = append(env, tunnel.ScriptEnv(k)+"="+v)
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return a.Run(rctx, out, env, "powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive",
		"-ExecutionPolicy", "Bypass", "-File", file)
}

/* ── Before and after, on Windows ────────────────────────────────────────── */

// Where a Windows report looks for new folders. Tests move them.
var winScriptFolders = func() []string {
	var out []string
	for _, v := range []string{"ProgramFiles", "ProgramFiles(x86)", "ProgramData"} {
		if d := os.Getenv(v); d != "" {
			out = append(out, d)
		}
	}
	return out
}

func (a *PackageApplier) winPicture(ctx context.Context) serverPicture {
	p := serverPicture{packages: map[string]tunnel.InstalledPackage{}, units: map[string]bool{},
		ports: map[int]bool{}, folders: map[string]bool{}}
	if apps, err := a.winApps(); err == nil {
		for _, w := range apps {
			p.packages[w.DisplayName] = tunnel.InstalledPackage{Name: w.DisplayName, Version: w.DisplayVersion}
		}
	}
	if out, code, err := output(ctx, a.Run, nil, "sc.exe", "query", "type=", "service", "state=", "all"); err == nil && code == 0 {
		for _, s := range parseScQuery(out) {
			p.units[s] = true
		}
	}
	if out, code, err := output(ctx, a.Run, nil, "netstat.exe", "-an", "-p", "TCP"); err == nil && code == 0 {
		for _, port := range parseNetstatListen(out) {
			p.ports[port] = true
		}
	}
	for _, d := range winScriptFolders() {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				p.folders[filepath.Join(d, e.Name())] = true
			}
		}
	}
	return p
}

// parseScQuery reads `sc query`: one "SERVICE_NAME: x" line per service.
func parseScQuery(out []byte) []string {
	var names []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if n, ok := strings.CutPrefix(l, "SERVICE_NAME:"); ok {
			if n = strings.TrimSpace(n); n != "" && !rePerUserService.MatchString(n) {
				names = append(names, n)
			}
		}
	}
	return names
}

// rePerUserService is a per-user service instance (CDPUserSvc_1a2b3c), which
// comes and goes with logons.
var rePerUserService = regexp.MustCompile(`_[0-9a-f]{4,}$`)

// parseNetstatListen reads `netstat -an -p TCP`: Proto Local Foreign State.
func parseNetstatListen(out []byte) []int {
	seen := map[int]bool{}
	var ports []int
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 4 || !strings.EqualFold(f[0], "TCP") || !strings.EqualFold(f[3], "LISTENING") {
			continue
		}
		i := strings.LastIndexByte(f[1], ':')
		if i < 0 {
			continue
		}
		if p, err := strconv.Atoi(f[1][i+1:]); err == nil && !seen[p] {
			seen[p] = true
			ports = append(ports, p)
		}
	}
	sort.Ints(ports)
	return ports
}

// utf16ToString reads Windows Installer's log, which is UTF-16 with a BOM,
// or passes text that is not.
func utf16ToString(b []byte) string {
	if len(b) < 2 || b[0] != 0xff || b[1] != 0xfe {
		return string(b)
	}
	var sb strings.Builder
	for i := 2; i+1 < len(b); i += 2 {
		r := rune(b[i]) | rune(b[i+1])<<8
		if r != 0 {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}
