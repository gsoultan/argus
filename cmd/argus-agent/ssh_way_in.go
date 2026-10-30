package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gsoultan/argus/internal/agent"
)

// defaultAuthorizedKeysFile is sshd's own default, as sshd -T prints it.
const defaultAuthorizedKeysFile = ".ssh/authorized_keys .ssh/authorized_keys2"

// sshWaysIn lists how someone could still log in over SSH once the agent is
// gone, from sshd's effective settings (sshd -T, keys lowercased) and the
// authorized_keys the posture scan found. Empty means nobody could: the
// person uninstalling would be locking everyone out of plain SSH.
//
// Certificates are not counted. TrustedUserCAKeys trusts a CA that Argus
// signed with, and nothing signs with it once Argus is gone from the host.
func sshWaysIn(settings map[string]string, keys []agent.UnmanagedKey) []string {
	var ways []string
	if settings["passwordauthentication"] == "yes" ||
		(settings["kbdinteractiveauthentication"] == "yes" && settings["usepam"] == "yes") {
		ways = append(ways, "password logins")
	}
	if settings["pubkeyauthentication"] == "no" {
		return ways
	}
	if cmd := settings["authorizedkeyscommand"]; cmd != "" && cmd != "none" {
		ways = append(ways, "keys from AuthorizedKeysCommand "+firstField(cmd))
	}
	if f := settings["authorizedkeysfile"]; f != "" && f != defaultAuthorizedKeysFile {
		// Somewhere this cannot look on its own; take sshd's word for it.
		ways = append(ways, "keys in AuthorizedKeysFile "+f)
	}
	perUser := map[string]int{}
	for _, k := range keys {
		if k.User == "root" && rootKeyLoginRefused(settings["permitrootlogin"]) {
			continue
		}
		perUser[k.User]++
	}
	users := make([]string, 0, len(perUser))
	for u := range perUser {
		users = append(users, u)
	}
	sort.Strings(users)
	for _, u := range users {
		ways = append(ways, fmt.Sprintf("%s with %d key(s)", u, perUser[u]))
	}
	return ways
}

// rootKeyLoginRefused is PermitRootLogin's answer for a key login with no
// forced command, which is what a person logging in uses.
func rootKeyLoginRefused(v string) bool {
	return v == "no" || strings.HasPrefix(v, "forced-commands")
}

// parseSSHDT reads sshd -T: one lowercased keyword and its value per line.
// Repeated keywords keep the first, as sshd does for most of them.
func parseSSHDT(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		if _, seen := m[k]; !seen {
			m[k] = strings.TrimSpace(v)
		}
	}
	return m
}
