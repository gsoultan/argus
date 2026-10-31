package control

import (
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"regexp"
	"strings"
)

/*
The one-line install.

Adding a server used to mean pasting three blocks: a package download that
depended on picking the right distribution and architecture, an enroll command,
and four lines that start the agent and hook sshd. The order of the last block
is what keeps a server reachable -- hook sshd before the agent runs and every
login is refused -- and a person pasting blocks by hand can get it wrong.

GET /api/v1/install/agent.sh returns one script that does all of it in the
right order, and backs sshd_config up before it touches it, restoring it if
sshd rejects the result. The console shows it as one `curl ... | sudo sh`.

The route is unauthenticated, like /enroll: the server running it has no
console session. It holds no secret of its own. The token in it is the one the
caller passed, which is single-use and lives an hour; the pin is public. Each
value it writes into the script is checked against a strict pattern first,
because the output runs as root.
*/

var (
	reEnrollToken = regexp.MustCompile(`^` + enrollTokenPrefix + `[A-Za-z0-9_-]{16,128}$`)
	reAgentVer    = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`)
	reShellSafe   = regexp.MustCompile(`^[A-Za-z0-9:/._\-\[\]]+$`)
)

// curlPin turns "sha256:<hex>" into curl's --pinnedpubkey form.
func curlPin(pin string) string {
	raw, err := hex.DecodeString(strings.TrimPrefix(pin, "sha256:"))
	if err != nil || len(raw) != 32 {
		return ""
	}
	return "sha256//" + base64.StdEncoding.EncodeToString(raw)
}

func (a *API) getInstallScript(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	token, version := q.Get("token"), q.Get("version")
	if !reEnrollToken.MatchString(token) {
		writeErr(w, http.StatusBadRequest, "token is missing or is not an enrollment token")
		return
	}
	if !reAgentVer.MatchString(version) {
		writeErr(w, http.StatusBadRequest, "version must look like 1.2.3")
		return
	}
	u, err := CheckURL(q.Get("url"), "the control plane address", true)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	control := strings.TrimRight(u.String(), "/")
	if !reShellSafe.MatchString(control) {
		writeErr(w, http.StatusBadRequest, "the control plane address has characters a shell would read")
		return
	}

	pin := ""
	if a.Enroll.Pin != nil {
		if p, perr := a.Enroll.Pin(); perr == nil && reShellSafe.MatchString(p) {
			pin = p
		}
	}

	script := strings.NewReplacer(
		"@TOKEN@", token,
		"@VERSION@", version,
		"@CONTROL@", control,
		"@PIN@", pin,
	).Replace(agentInstallScript)

	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(script))
}

// agentInstallScript is POSIX sh, for every distribution the packages exist
// for. Package names follow what .goreleaser.yaml produces.
const agentInstallScript = `#!/bin/sh
# Argus agent install. Made by the Argus console for one server.
set -eu

TOKEN='@TOKEN@'
VERSION='@VERSION@'
CONTROL='@CONTROL@'
PIN='@PIN@'
BASE="https://github.com/gsoultan/argus/releases/download/v${VERSION}"

say() { printf '\n==> %s\n' "$*"; }
die() { printf '\nArgus install stopped: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" = 0 ] || die "run it with sudo"

case "$(uname -m)" in
  x86_64|amd64) DEB=amd64; RPM=x86_64; APK=x86_64 ;;
  aarch64|arm64) DEB=arm64; RPM=aarch64; APK=aarch64 ;;
  *) die "this processor ($(uname -m)) has no agent package" ;;
esac

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
cd "$TMP"

say "Installing the Argus agent ${VERSION}"
if command -v apt-get >/dev/null 2>&1; then
  F="argus-agent_${VERSION}_${DEB}.deb"
  curl -fsSLO "${BASE}/${F}"
  DEBIAN_FRONTEND=noninteractive apt-get install -y "./${F}" >/dev/null
elif command -v dnf >/dev/null 2>&1 || command -v yum >/dev/null 2>&1; then
  F="argus-agent-${VERSION}-1.${RPM}.rpm"
  curl -fsSLO "${BASE}/${F}"
  if command -v dnf >/dev/null 2>&1; then dnf install -y "./${F}" >/dev/null; else yum install -y "./${F}" >/dev/null; fi
elif command -v apk >/dev/null 2>&1; then
  F="argus-agent_${VERSION}_${APK}.apk"
  curl -fsSLO "${BASE}/${F}"
  apk add --allow-untrusted "./${F}" >/dev/null
else
  die "no apt, dnf, yum or apk here; install the package by hand from ${BASE}"
fi

say "Linking this server to Argus"
if [ -n "$PIN" ]; then
  argus-agent enroll --control-url "$CONTROL" --pin "$PIN" --token "$TOKEN"
else
  argus-agent enroll --control-url "$CONTROL" --token "$TOKEN"
fi

# The agent first, then sshd. Once sshd_config carries the ForceCommand every
# login goes through the agent, and a login with no agent running is refused.
say "Starting the agent"
systemctl daemon-reload
systemctl enable --now argus-agent

CONF=/etc/ssh/sshd_config
if [ -f "$CONF" ] && ! grep -q '^ForceCommand .*argus-agent' "$CONF"; then
  say "Recording logins that do not go through Argus"
  cp -p "$CONF" "$CONF.before-argus"
  argus-agent install --shim /usr/bin/argus-agent >> "$CONF"
  if sshd -t 2>/dev/null; then
    systemctl reload sshd 2>/dev/null || systemctl reload ssh 2>/dev/null || true
  else
    # Never leave sshd with a configuration it rejects: that is a lockout.
    cp -p "$CONF.before-argus" "$CONF"
    die "sshd rejected the new configuration, so it was put back as it was. The agent is running; ask your Argus admin."
  fi
fi

say "Done. This server will appear in Argus within a minute."
`
