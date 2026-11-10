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
	reEnrollToken = regexp.MustCompile(`^` + enrollTokenPrefix + `(?:[a-z0-9]{12}\.)?[A-Za-z0-9_-]{16,128}$`)
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
	version = strings.TrimPrefix(version, "v")
	if version != "latest" && !reAgentVer.MatchString(version) {
		writeErr(w, http.StatusBadRequest, "version must look like 1.2.3, or be latest")
		return
	}
	version = a.installVersion(version)
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
	// pin=off: the console found that the address servers use presents a
	// public certificate other than this process's -- a Cloudflare Tunnel --
	// so the agent enrols with ordinary verification. Only the person
	// running the command is affected by it, and they chose the command.
	if q.Get("pin") != "off" && a.Enroll.Pin != nil {
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

// installVersion is the agent version a new server installs: the newest
// release found with agent packages, or this control plane's own version if
// that is newer, rather than whatever the console asked for. The console asks
// for its own version, so a control plane that was not upgraded used to put
// its old agent on every new server, which then had to be updated straight
// away. A version newer than both is kept; "latest" stays "latest" when no
// release is known, and the script finds the newest itself.
func (a *API) installVersion(asked string) string {
	offer, _, _ := a.agentTarget()
	if offer != "" && (asked == "latest" || versionLess(asked, offer)) {
		return offer
	}
	return asked
}

// agentInstallScript is POSIX sh, for every distribution the packages exist
// for. Package names follow what .goreleaser.yaml produces.
//
// VERSION is the newest release the control plane knows has agent packages,
// or "latest" when it knows none (a build from source that cannot reach
// GitHub). With "latest", or when the release asked for has no package for
// this server, the script finds the newest release that does itself. A
// release tag may or may not start with "v" (1.2.0 was published as "1.2.0",
// with no files), so a tag is read from the release, never assumed, and a
// release with nothing attached is passed over rather than downloaded from.
const agentInstallScript = `#!/bin/sh
# Argus agent install. Made by the Argus console for one server.
set -eu

TOKEN='@TOKEN@'
VERSION='@VERSION@'
CONTROL='@CONTROL@'
PIN='@PIN@'
RELEASES="https://github.com/gsoultan/argus/releases"
API="https://api.github.com/repos/gsoultan/argus/releases?per_page=30"

say() { printf '\n==> %s\n' "$*"; }
die() { printf '\nArgus install stopped: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" = 0 ] || die "run it with sudo"
command -v curl >/dev/null 2>&1 || die "curl is not installed"

# Release packages are named argus-agent_<version>_linux_<arch>.<format>,
# goreleaser's default, with Go's names for the processor in every format.
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "this processor ($(uname -m)) has no agent package" ;;
esac

if command -v apt-get >/dev/null 2>&1; then
  FMT=deb
elif command -v dnf >/dev/null 2>&1 || command -v yum >/dev/null 2>&1; then
  FMT=rpm
elif command -v apk >/dev/null 2>&1; then
  FMT=apk
else
  die "no apt, dnf, yum or apk here; install the package by hand from ${RELEASES}"
fi

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
cd "$TMP"

# get downloads release tag $1's package for this server, and its checksums.
get() {
  F="argus-agent_${VERSION}_linux_${ARCH}.${FMT}"
  BASE="${RELEASES}/download/$1"
  curl -fsLO "${BASE}/${F}" && curl -fsLO "${BASE}/checksums.txt"
}

# newest prints "<version> <tag>" for the highest release that has this
# server's package attached: drafts are not listed, and pre-releases, whose
# versions carry a "-", do not match. It prints nothing if GitHub cannot be
# asked (rate limited, say).
newest() {
  curl -fsSL -H 'Accept: application/vnd.github+json' "$API" 2>/dev/null |
    grep -o "/releases/download/[^/\"]*/argus-agent_[0-9]*\.[0-9]*\.[0-9]*_linux_${ARCH}\.${FMT}\"" |
    sed 's|^/releases/download/\([^/]*\)/argus-agent_\([0-9.]*\)_linux_.*|\2 \1|' |
    sort -t . -k1,1n -k2,2n -k3,3n | tail -n 1 || true
}

GOT=
if [ "$VERSION" != latest ]; then
  for t in "v${VERSION}" "${VERSION}"; do
    if get "$t"; then GOT=$t; break; fi
  done
  [ -n "$GOT" ] || say "Release ${VERSION} has no agent package for this server; looking for the newest that does"
fi

if [ -z "$GOT" ]; then
  N=$(newest)
  if [ -n "$N" ]; then
    VERSION=${N%% *}
    GOT=${N#* }
  else
    # The list could not be had: GitHub sends /releases/latest to its tag.
    URL=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "${RELEASES}/latest") ||
      die "could not reach ${RELEASES} to find the newest release"
    GOT=${URL##*/}
    VERSION=${GOT#v}
  fi
  case "$VERSION" in
    [0-9]*.[0-9]*.[0-9]*) ;;
    *) die "could not tell the newest release (found '${GOT}')" ;;
  esac
  get "$GOT" ||
    die "could not download ${BASE}/${F}. Check that release ${GOT} has its files attached and this server can reach github.com."
fi

# The package is the one the release published, or nothing is installed.
grep "  ${F}\$" checksums.txt > "${F}.sha256" ||
  die "release ${GOT} lists no checksum for ${F}"
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum -c "${F}.sha256" >/dev/null 2>&1 ||
    die "${F} does not match the checksum release ${GOT} publishes; nothing was installed"
fi

say "Installing the Argus agent ${VERSION}"
case "$FMT" in
  deb) DEBIAN_FRONTEND=noninteractive apt-get install -y "./${F}" >/dev/null ;;
  rpm) if command -v dnf >/dev/null 2>&1; then dnf install -y "./${F}" >/dev/null; else yum install -y "./${F}" >/dev/null; fi ;;
  apk) apk add --allow-untrusted "./${F}" >/dev/null ;;
esac

say "Linking this server to Argus"
if [ -n "$PIN" ]; then
  # The pin is the control plane's guess at what this server meets at
  # CONTROL, made from the control plane's own network. Behind a Cloudflare
  # Tunnel with split DNS, a server outside that network meets Cloudflare's
  # certificate instead. A failed handshake does not spend the token, so the
  # address's public certificate is tried next, verified the ordinary way.
  if ! argus-agent enroll --control-url "$CONTROL" --pin "$PIN" --token "$TOKEN"; then
    say "Trying ${CONTROL}'s public certificate instead of the pin"
    argus-agent enroll --control-url "$CONTROL" --token "$TOKEN"
  fi
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
  # Appended after a Match block, ForceCommand would apply only to the logins
  # that block matches. Match all ends the block first.
  if grep -qiE '^[[:space:]]*Match[[:space:]]' "$CONF"; then
    printf '\nMatch all\n' >> "$CONF"
  fi
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
