package control

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
The automatic checks every script version gets on save.

Run on the text only, never by executing it. Nothing can prove a script that
installs software as root is safe; these catch the obvious dangers before a
person even looks, so the reviewer's attention goes where it is needed. Each
finding is:

  - blocked: the version cannot be reviewed or run until it is changed;
  - warning: the reviewer ticks it off, with a reason, to approve;
  - note:    shown to the reviewer.

The rules are data (scriptRules), listed in the console under Settings,
Software, so what is checked is never a mystery. Blocked cannot be waived
from the console.
*/

// Finding levels.
const (
	LevelBlocked = "blocked"
	LevelWarning = "warning"
	LevelNote    = "note"
)

// ScriptFinding is one thing the checks found.
type ScriptFinding struct {
	Rule  string `json:"rule"`
	Level string `json:"level"`
	Title string `json:"title"`
	Why   string `json:"why,omitempty"`
	// Part is install, uninstall or check; Line is 1-based, 0 for the
	// whole part.
	Part    string `json:"part"`
	Line    int    `json:"line,omitempty"`
	Excerpt string `json:"excerpt,omitempty"`
}

// Key names a finding for accepting it: the same finding on the same line.
func (f ScriptFinding) Key() string { return f.Part + ":" + f.Rule + ":" + strconv.Itoa(f.Line) }

// ScriptRule is one check, as Settings shows it.
type ScriptRule struct {
	ID       string `json:"id"`
	Platform string `json:"platform"`
	Level    string `json:"level"`
	Group    string `json:"group"`
	Title    string `json:"title"`
	Why      string `json:"why"`
	Pattern  string `json:"pattern"`
	re       *regexp.Regexp
	// unless skips a line that also matches it.
	unless *regexp.Regexp
}

func rule(platform, level, group, id, title, why, pattern string, unless ...string) ScriptRule {
	r := ScriptRule{ID: id, Platform: platform, Level: level, Group: group, Title: title, Why: why,
		Pattern: pattern, re: regexp.MustCompile(pattern)}
	if len(unless) > 0 {
		r.unless = regexp.MustCompile(unless[0])
	}
	return r
}

const (
	gHidden   = "Hidden code"
	gDestroy  = "Destroys the system"
	gWayIn    = "Opens a way in"
	gProtect  = "Turns off protection"
	gArgus    = "Touches Argus"
	gTracks   = "Erases tracks"
	gSecrets  = "Reads secrets"
	gDownload = "Runs what it downloads unchecked"
	gHardcode = "Hard-coded secret"
)

var (
	lx = tunnel.ScriptLinux
	wn = tunnel.ScriptWindows
)

// scriptRules are the line checks. Patterns are case-insensitive where
// the language is.
var scriptRules = []ScriptRule{
	// Linux: hidden code.
	rule(lx, LevelBlocked, gHidden, "sh-base64-exec", "Decodes base64 and runs it",
		"Code hidden in base64 cannot be reviewed. Put the commands in the script as text.",
		`base64\s+(-d|--decode|-D)\b[^|]*\|\s*(sudo\s+)?(ba|z|da|k)?sh\b`),
	rule(lx, LevelBlocked, gHidden, "sh-eval-subst", "Runs the output of a command as code",
		"eval on a command's output runs code nobody reviewed.", `\beval\s+["']?\$\(|\beval\s+["']?`+"`"),
	rule(lx, LevelBlocked, gHidden, "sh-hex-exec", "Runs escaped bytes as code",
		"Escaped bytes piped into a shell hide what runs.", `\\x[0-9a-fA-F]{2}.*\|\s*(ba|z|da)?sh\b|\beval\s+.*\\x[0-9a-fA-F]{2}`),
	// Linux: destroys the system.
	rule(lx, LevelBlocked, gDestroy, "sh-rm-root", "Deletes the root of the file system",
		"rm -r on / removes everything.", `\brm\s+(-{1,2}[A-Za-z-]+\s+)*/(\*)?(\s|;|$)`),
	rule(lx, LevelBlocked, gDestroy, "sh-rm-var", "Deletes a path that starts with a variable",
		"If the variable is empty this deletes from /. Use ${VAR:?} so an empty one stops the script.",
		`\brm\s+(-{1,2}[A-Za-z-]*[rR][A-Za-z-]*\s+)+(\S+\s+)*"?\$\{?[A-Za-z_][A-Za-z0-9_]*\}?"?(/|\s|$)`, `:\?`),
	rule(lx, LevelBlocked, gDestroy, "sh-mkfs", "Formats a file system", "mkfs erases a disk.", `\bmkfs(\.[a-z0-9]+)?\b`),
	rule(lx, LevelBlocked, gDestroy, "sh-raw-disk", "Writes to a raw disk",
		"Writing to a disk device destroys what is on it.",
		`\bof=/dev/(sd|nvme|vd|xvd|hd|mmcblk)|>\s*/dev/(sd|nvme|vd|xvd|hd)[a-z0-9]*\b|\bwipefs\b|\bsgdisk\s+(-Z|--zap)`),
	// Linux: opens a way in.
	rule(lx, LevelBlocked, gWayIn, "sh-reverse-shell", "Opens a shell to another machine",
		"A connection that hands a shell to another machine is a back door.",
		`/dev/(tcp|udp)/|\bnc(at)?\b.*\s-[a-z]*[ec]\b|\bsocat\b.*\bexec:|\bbash\s+-i\s+>&`),
	rule(lx, LevelBlocked, gWayIn, "sh-new-user", "Adds or changes a login",
		"New accounts and group changes are managed in Argus, not by install scripts.",
		`\b(useradd|adduser|usermod|passwd|chpasswd|gpasswd)\b`),
	rule(lx, LevelBlocked, gWayIn, "sh-sudoers", "Changes who can be root", "sudoers changes grant root.",
		`/etc/sudoers`),
	rule(lx, LevelBlocked, gWayIn, "sh-ssh-keys", "Changes SSH keys or the SSH server",
		"SSH access is what Argus manages; a script must not add keys or change sshd.",
		`authorized_keys|/etc/ssh/sshd_config`),
	// Linux: turns off protection.
	rule(lx, LevelBlocked, gProtect, "sh-firewall-off", "Turns the firewall off", "",
		`\bufw\s+disable\b|\biptables\s+(-F|--flush)\b|\bnft\s+flush\s+ruleset\b|systemctl\s+(stop|disable|mask)\s+(firewalld|ufw|nftables)\b`),
	rule(lx, LevelBlocked, gProtect, "sh-mac-off", "Turns SELinux, AppArmor or auditing off", "",
		`\bsetenforce\s+0\b|SELINUX=(disabled|permissive)|\baa-teardown\b|systemctl\s+(stop|disable|mask)\s+(apparmor|auditd)\b|\bauditctl\s+-D\b`),
	// Linux: touches Argus.
	rule(lx, LevelBlocked, gArgus, "sh-argus", "Touches Argus",
		"The agent, its state, its unit and its keys are off limits to scripts.",
		`(?i)argus-agent|/var/lib/argus|/etc/argus`),
	// Linux: erases tracks.
	rule(lx, LevelBlocked, gTracks, "sh-erase-logs", "Erases history or logs", "",
		`\bhistory\s+-c\b|>\s*/var/log/|\btruncate\b.*\s/var/log/|journalctl\b.*--vacuum|\brm\s+.*\s/var/log/|\bunset\s+HISTFILE\b`),
	// Linux: reads secrets.
	rule(lx, LevelBlocked, gSecrets, "sh-shadow", "Reads password hashes or private keys", "",
		`/etc/g?shadow\b|\.ssh/id_(rsa|dsa|ecdsa|ed25519)\b|/etc/ssl/private/`),
	rule(lx, LevelBlocked, gSecrets, "sh-metadata", "Reads cloud credentials",
		"The cloud metadata service hands out this machine's credentials.", `169\.254\.169\.254|metadata\.google\.internal`),
	rule(lx, LevelWarning, gSecrets, "sh-home", "Reads or writes in someone's home folder", "",
		`/home/[A-Za-z0-9._-]+/|~[A-Za-z0-9._-]+/`),
	// Linux: downloads.
	rule(lx, LevelWarning, gDownload, "sh-curl-pipe", "Runs a download without checking it",
		"Download to a file and check its SHA-256 (sha256sum -c) before running it: what a URL serves can change after review.",
		`\b(curl|wget)\b[^|;]*\|\s*(sudo\s+)?(-E\s+)?(ba|z|da|k)?sh\b|\b(ba)?sh\s+(-s\s+)?<\(\s*(curl|wget)\b|\bsource\s+<\(\s*(curl|wget)\b`),

	// Windows: hidden code.
	rule(wn, LevelBlocked, gHidden, "ps-encoded", "Runs an encoded command",
		"An encoded command cannot be reviewed. Put the commands in the script as text.",
		`(?i)\s-e(c|nc|ncodedcommand)?\s+[A-Za-z0-9+/=]{24,}`),
	rule(wn, LevelBlocked, gHidden, "ps-base64", "Decodes base64 into code",
		"Code hidden in base64 cannot be reviewed.", `(?i)FromBase64String`),
	rule(wn, LevelBlocked, gHidden, "ps-iex", "Runs a string as code",
		"Invoke-Expression runs whatever the string holds at run time, which nobody reviewed.",
		`(?i)\b(iex|Invoke-Expression)\b`, `(?i)\b(iex|Invoke-Expression)\s*\(?\s*\(?\s*(iwr|irm|Invoke-WebRequest|Invoke-RestMethod|\(?New-Object\s+(System\.)?Net\.WebClient)`),
	rule(wn, LevelBlocked, gHidden, "ps-charcodes", "Builds code from character codes", "",
		`(?i)\[char\]\s*\d+\s*\+\s*\[char\]\s*\d+|-join\s*\(\s*\d+\s*,\s*\d+.*\|\s*%\s*\{\s*\[char\]`),
	// Windows: destroys the system.
	rule(wn, LevelBlocked, gDestroy, "ps-disk", "Formats or wipes a disk", "",
		`(?i)\b(Format-Volume|Clear-Disk|Initialize-Disk|diskpart)\b|\bformat\s+[A-Za-z]:`),
	rule(wn, LevelBlocked, gDestroy, "ps-rm-drive", "Deletes a whole drive", "",
		`(?i)\b(Remove-Item|rm|del|rd|rmdir)\s+['"]?[A-Za-z]:\\?\*?['"]?\s+.*-Recurse|\brd\s+/s\s+/q\s+[A-Za-z]:\\?\s*$`),
	rule(wn, LevelBlocked, gDestroy, "ps-shadows", "Deletes backups or boot settings",
		"Deleting shadow copies and changing boot settings is what ransomware does.",
		`(?i)vssadmin(\.exe)?\s+delete\s+shadows|\bbcdedit\b|wmic\s+shadowcopy\s+delete`),
	// Windows: opens a way in.
	rule(wn, LevelBlocked, gWayIn, "ps-new-user", "Adds a local user or administrator",
		"Accounts are managed in Argus, not by install scripts.",
		`(?i)\bNew-LocalUser\b|\bAdd-LocalGroupMember\b|\bnet(1)?\s+(user\s+\S+\s+.*/add|localgroup\s+administrators\s+\S+\s*/add)`),
	rule(wn, LevelBlocked, gWayIn, "ps-remoting", "Turns on remote access",
		"PowerShell remoting and Remote Desktop are ways in that Argus does not see.",
		`(?i)\bEnable-PSRemoting\b|fDenyTSConnections|winrm\s+(quickconfig|qc)\b|Set-Item\s+WSMan:`),
	rule(wn, LevelWarning, gWayIn, "ps-firewall-port", "Opens an inbound port in the firewall",
		"Check the port is the one the software needs.",
		`(?i)New-NetFirewallRule\b.*-Direction\s+Inbound.*-LocalPort|netsh\s+advfirewall\s+firewall\s+add\s+rule.*dir=in.*localport=`),
	rule(wn, LevelBlocked, gWayIn, "ps-firewall-open", "Opens the firewall to everything inbound", "",
		`(?i)New-NetFirewallRule\b.*-Direction\s+Inbound|netsh\s+advfirewall\s+firewall\s+add\s+rule.*dir=in`,
		`(?i)-LocalPort|localport=`),
	// Windows: turns off protection.
	rule(wn, LevelBlocked, gProtect, "ps-defender", "Turns Defender off or excludes something from it", "",
		`(?i)Set-MpPreference\b.*-Disable|Add-MpPreference\b.*-Exclusion`),
	rule(wn, LevelBlocked, gProtect, "ps-firewall-off", "Turns the firewall off", "",
		`(?i)Set-NetFirewallProfile\b.*-Enabled\s+(False|\$false|0)|netsh\s+advfirewall\s+set\s+\w+\s+state\s+off`),
	rule(wn, LevelBlocked, gProtect, "ps-execpolicy", "Lets every script run, machine-wide",
		"Set the policy for this process only: -Scope Process.",
		`(?i)Set-ExecutionPolicy\s+(-ExecutionPolicy\s+)?(Unrestricted|Bypass)\b`, `(?i)-Scope\s+Process`),
	// Windows: touches Argus.
	rule(wn, LevelBlocked, gArgus, "ps-argus", "Touches Argus",
		"The Argus service and its folders are off limits to scripts.",
		`(?i)\\Argus\b|\bargus-agent\b|(Stop|Remove|Set|Restart)-Service\b.*\bargus|\bsc(\.exe)?\s+(stop|delete|config)\s+argus`),
	// Windows: erases tracks.
	rule(wn, LevelBlocked, gTracks, "ps-erase-logs", "Erases event logs", "",
		`(?i)\bwevtutil(\.exe)?\s+(cl|clear-log)\b|\b(Clear|Remove)-EventLog\b|Clear-History\b`),
	// Windows: reads secrets.
	rule(wn, LevelBlocked, gSecrets, "ps-creds", "Reads stored credentials", "",
		`(?i)\breg(\.exe)?\s+save\s+hklm\\(sam|security|system)\b|\blsass\b|mimikatz|sekurlsa|ProtectedData\]::Unprotect|169\.254\.169\.254`),
	// Windows: downloads.
	rule(wn, LevelWarning, gDownload, "ps-iex-download", "Runs a download without checking it",
		"Download to a file and check Get-FileHash or Get-AuthenticodeSignature before running it.",
		`(?i)\b(iex|Invoke-Expression)\s*\(?\s*\(?\s*(iwr|irm|Invoke-WebRequest|Invoke-RestMethod|\(?New-Object\s+(System\.)?Net\.WebClient)`),

	// Both: hard-coded secrets.
	rule("", LevelBlocked, gHardcode, "secret-assign", "A password or token is written in the script",
		"Make it a secret parameter: it is then masked in the console, the log and Activity.",
		`(?i)\b[A-Za-z_]*(password|passwd|secret|token|api_?key|apikey)\b\s*[:=]\s*['"][^'"$\s]{8,}['"]`),
	rule("", LevelBlocked, gHardcode, "secret-known", "A key or token is written in the script",
		"Make it a secret parameter.",
		`AKIA[0-9A-Z]{16}|-----BEGIN [A-Z ]*PRIVATE KEY-----|\bgh[pousr]_[A-Za-z0-9]{36}\b|\bxox[abprs]-[A-Za-z0-9-]{10,}|\bglpat-[A-Za-z0-9_-]{20}`),
}

// ScriptRules lists the rules for platform, for Settings.
func ScriptRules(platform string) []ScriptRule {
	var out []ScriptRule
	for _, r := range scriptRules {
		if r.Platform == "" || r.Platform == platform {
			out = append(out, r)
		}
	}
	return out
}

var (
	reURL      = regexp.MustCompile(`https?://[^\s'"()<>|;` + "`" + `]+`)
	reDownload = map[string]*regexp.Regexp{
		lx: regexp.MustCompile(`\b(curl|wget)\b`),
		wn: regexp.MustCompile(`(?i)\b(Invoke-WebRequest|iwr|Start-BitsTransfer|DownloadFile|curl(\.exe)?|wget)\b`),
	}
	reVerified = map[string]*regexp.Regexp{
		lx: regexp.MustCompile(`\b(sha256sum|sha512sum|shasum|gpg|gpgv|cosign|minisign)\b`),
		wn: regexp.MustCompile(`(?i)\b(Get-FileHash|Get-AuthenticodeSignature)\b`),
	}
	reExecutes = map[string]*regexp.Regexp{
		lx: regexp.MustCompile(`\b(dpkg\s+-i|rpm\s+-[iU]|apt(-get)?\s+install\s+\./|chmod\s+\+?[0-7]*x|bash\s+\S|sh\s+\S|tar\s+)`),
		wn: regexp.MustCompile(`(?i)\b(Start-Process|msiexec|Expand-Archive)\b|^\s*&\s`),
	}
)

// CheckScript runs every check on one part.
func CheckScript(ctx context.Context, platform, part, text string) []ScriptFinding {
	var out []ScriptFinding
	lines := strings.Split(text, "\n")
	for i, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		if comment(platform, line) {
			continue
		}
		for _, r := range scriptRules {
			if r.Platform != "" && r.Platform != platform {
				continue
			}
			if !r.re.MatchString(line) || (r.unless != nil && r.unless.MatchString(line)) {
				continue
			}
			out = append(out, ScriptFinding{Rule: r.ID, Level: r.Level, Title: r.Title, Why: r.Why, Part: part,
				Line: i + 1, Excerpt: clip(strings.TrimSpace(line), 200)})
		}
	}
	// Downloads run without a hash check anywhere in the part.
	if reDownload[platform].MatchString(text) && reExecutes[platform].MatchString(text) &&
		!reVerified[platform].MatchString(text) && !has(out, gDownload) {
		out = append(out, ScriptFinding{Rule: "download-unverified", Level: LevelWarning, Part: part,
			Title: "Downloads something and runs it without checking it",
			Why:   "Check a SHA-256 or a signature between downloading and running: what a URL serves can change after review."})
	}
	if hosts := scriptHosts(text); len(hosts) > 0 {
		out = append(out, ScriptFinding{Rule: "hosts", Level: LevelNote, Part: part,
			Title: "Talks to " + strings.Join(hosts, ", ")})
	}
	out = append(out, syntaxCheck(ctx, platform, part, text)...)
	sort.SliceStable(out, func(i, j int) bool { return levelRank(out[i].Level) < levelRank(out[j].Level) })
	return out
}

func has(fs []ScriptFinding, group string) bool {
	for _, f := range fs {
		for _, r := range scriptRules {
			if r.ID == f.Rule && r.Group == group {
				return true
			}
		}
	}
	return false
}

func levelRank(l string) int {
	switch l {
	case LevelBlocked:
		return 0
	case LevelWarning:
		return 1
	}
	return 2
}

func comment(platform, line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "#") && !strings.HasPrefix(t, "#!")
}

// scriptHosts lists the hosts a part names in URLs.
func scriptHosts(text string) []string {
	seen := map[string]bool{}
	var hosts []string
	for _, u := range reURL.FindAllString(text, 50) {
		p, err := url.Parse(u)
		if err != nil || p.Hostname() == "" || strings.ContainsAny(p.Hostname(), "$%{") {
			continue
		}
		h := strings.ToLower(p.Hostname())
		if !seen[h] {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}
	sort.Strings(hosts)
	return hosts
}

// syntaxCheck parses a shell script with bash -n (which reads, never runs)
// and lints it with ShellCheck when the control plane has it.
func syntaxCheck(ctx context.Context, platform, part, text string) []ScriptFinding {
	if platform != lx {
		return []ScriptFinding{{Rule: "syntax", Level: LevelNote, Part: part,
			Title: "PowerShell syntax is checked by the agent, which parses the script before it runs it"}}
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		return []ScriptFinding{{Rule: "syntax", Level: LevelNote, Part: part,
			Title: "Syntax not checked: this control plane has no bash"}}
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, bash, "-n")
	cmd.Stdin = strings.NewReader(text)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}
	var out []ScriptFinding
	if b, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(b))
		line := 0
		if m := regexp.MustCompile(`line (\d+):`).FindStringSubmatch(msg); m != nil {
			line, _ = strconv.Atoi(m[1])
		}
		out = append(out, ScriptFinding{Rule: "syntax", Level: LevelBlocked, Part: part, Line: line,
			Title: "Does not parse", Why: clip(strings.ReplaceAll(msg, "bash: ", ""), 300)})
	}
	return append(out, shellcheck(ctx, part, text)...)
}

func shellcheck(ctx context.Context, part, text string) []ScriptFinding {
	sc, err := exec.LookPath("shellcheck")
	if err != nil {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, sc, "-f", "json", "-s", "bash", "-")
	cmd.Stdin = strings.NewReader(text)
	b, _ := cmd.Output()
	var issues []struct {
		Line    int    `json:"line"`
		Level   string `json:"level"`
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &issues) != nil {
		return nil
	}
	var out []ScriptFinding
	for _, is := range issues {
		if len(out) == 20 {
			break
		}
		level := LevelNote
		if is.Level == "error" || is.Level == "warning" {
			level = LevelWarning
		}
		out = append(out, ScriptFinding{Rule: fmt.Sprintf("SC%d", is.Code), Level: level, Part: part,
			Line: is.Line, Title: "ShellCheck: " + clip(is.Message, 200)})
	}
	return out
}

/* ── What comes in ───────────────────────────────────────────────────────── */

// NormalizeScript turns pasted or uploaded bytes into the text stored: no
// BOM, UTF-8, the platform's line endings. Anything that is not a text
// script is refused.
func NormalizeScript(platform, filename string, b []byte) (string, error) {
	if filename != "" {
		ext := strings.ToLower(path.Ext(filename))
		ok := map[string][]string{lx: {".sh", ".bash"}, wn: {".ps1", ".psm1"}}[platform]
		if !slices.Contains(ok, ext) {
			return "", fmt.Errorf("%s is not a %s script: upload %s", filename, platformName(platform), strings.Join(ok, " or "))
		}
	}
	switch {
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}), bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		b = fromUTF16(b)
	case bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}):
		b = b[3:]
	}
	for _, magic := range [][]byte{{0x7F, 'E', 'L', 'F'}, []byte("MZ"), []byte("PK\x03\x04"), {0x1F, 0x8B},
		[]byte("%PDF"), []byte("BZh"), {0xFD, '7', 'z', 'X', 'Z'}, []byte("7z\xBC\xAF")} {
		if bytes.HasPrefix(b, magic) {
			return "", errors.New("that is a program or an archive, not a script; only text scripts are accepted")
		}
	}
	if bytes.IndexByte(b, 0) >= 0 || !utf8.Valid(b) {
		return "", errors.New("that is not text: a script must be UTF-8 (or UTF-16 with a byte order mark)")
	}
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	if strings.TrimSpace(s) == "" {
		return "", nil
	}
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	if platform == wn {
		s = strings.ReplaceAll(s, "\n", "\r\n")
	}
	if len(s) > tunnel.MaxScriptBytes {
		return "", fmt.Errorf("a script can be at most %d KB", tunnel.MaxScriptBytes>>10)
	}
	return s, nil
}

func fromUTF16(b []byte) []byte {
	order := binary.ByteOrder(binary.LittleEndian)
	if b[0] == 0xFE {
		order = binary.BigEndian
	}
	b = b[2:]
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u = append(u, order.Uint16(b[i:]))
	}
	return []byte(string(utf16.Decode(u)))
}

func platformName(p string) string {
	if p == wn {
		return "Windows PowerShell"
	}
	return "Linux shell"
}
