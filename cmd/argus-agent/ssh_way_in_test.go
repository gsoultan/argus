package main

import (
	"reflect"
	"testing"

	"github.com/gsoultan/argus/internal/agent"
)

func TestAfterUninstallSomeoneMustStillBeAbleToSSHIn(t *testing.T) {
	keys := func(users ...string) []agent.UnmanagedKey {
		var out []agent.UnmanagedKey
		for _, u := range users {
			out = append(out, agent.UnmanagedKey{User: u})
		}
		return out
	}
	base := func(kv ...string) map[string]string {
		m := map[string]string{
			"pubkeyauthentication":   "yes",
			"passwordauthentication": "no",
			"authorizedkeysfile":     defaultAuthorizedKeysFile,
			"authorizedkeyscommand":  "none",
			"permitrootlogin":        "without-password",
		}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	for _, tc := range []struct {
		name     string
		settings map[string]string
		keys     []agent.UnmanagedKey
		want     []string
	}{
		{"keys only, two accounts", base(), keys("ana", "ana", "deploy"),
			[]string{"ana with 2 key(s)", "deploy with 1 key(s)"}},
		{"nothing at all: locked out", base(), nil, nil},
		{"only root's key, and root may not log in", base("permitrootlogin", "no"),
			keys("root"), nil},
		{"root's key counts when root may log in", base(), keys("root"),
			[]string{"root with 1 key(s)"}},
		{"keys switched off", base("pubkeyauthentication", "no"), keys("ana"), nil},
		{"passwords", base("passwordauthentication", "yes"), nil,
			[]string{"password logins"}},
		{"a key command (cloud sign-in)",
			base("authorizedkeyscommand", "/usr/share/ec2-instance-connect/eic_run_authorized_keys %u"),
			nil, []string{"keys from AuthorizedKeysCommand /usr/share/ec2-instance-connect/eic_run_authorized_keys"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sshWaysIn(tc.settings, tc.keys); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseSSHDT(t *testing.T) {
	m := parseSSHDT("port 22\npasswordauthentication no\nauthorizedkeysfile .ssh/authorized_keys .ssh/authorized_keys2\n")
	if m["passwordauthentication"] != "no" || m["authorizedkeysfile"] != defaultAuthorizedKeysFile {
		t.Errorf("parsed %v", m)
	}
}
