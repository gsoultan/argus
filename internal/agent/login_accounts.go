package agent

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

/*
Every account that can log in to this host over SSH, and how.

The posture scan already found the keys Argus did not issue. That answered
"which keys are there" but not "who can get in": an account with a password
and no key was invisible, and so was an account whose only way in was a key
Argus issued. This lists each account with a login shell and says whether it
has a password sshd would accept and how many keys it holds, so the console
can show the whole list rather than a count.

It reads what sshd reads -- passwd, shadow, authorized_keys, sshd_config --
and nothing it reports is a secret: a password is "set", "none" or "locked",
never its hash.
*/

// Password states of a login account.
const (
	PasswordSet     = "set"     // a hash sshd would check
	PasswordNone    = "none"    // empty: no password at all
	PasswordLocked  = "locked"  // "!" or "*": no password login
	PasswordUnknown = "unknown" // shadow could not be read
)

// LoginAccount is one account that has a login shell on this host.
type LoginAccount struct {
	User string `json:"user"`
	// Password is PasswordSet, PasswordNone, PasswordLocked or
	// PasswordUnknown.
	Password string `json:"password"`
	// Keys is how many keys its authorized_keys hold, Argus's or not.
	Keys int `json:"keys"`
	// CanLogin says whether sshd as configured would let it in with either:
	// a key while key logins are on, or a password while password logins are.
	CanLogin bool `json:"can_login"`
}

// SSHDAuth is what sshd_config says about how people may sign in, with
// OpenSSH's defaults for anything it does not set.
type SSHDAuth struct {
	// Passwords is whether sshd accepts a password: PasswordAuthentication,
	// or keyboard-interactive through PAM, which asks for the same thing.
	Passwords bool `json:"passwords"`
	// Keys is PubkeyAuthentication.
	Keys bool `json:"keys"`
	// ExposeAuthInfo is whether sshd tells a session how it signed in, which
	// the recorder needs to say which key or that it was a password.
	ExposeAuthInfo bool `json:"expose_auth_info"`
	// PermitRootLogin as sshd reads it.
	PermitRootLogin string `json:"permit_root_login"`
}

// readSSHDAuth reads sshd's sign-in settings from path and the files it
// includes. The first value wins, as sshd takes it, and Match blocks are
// skipped: they change settings for some logins, not for every one.
func readSSHDAuth(path string) SSHDAuth {
	got := map[string]string{}
	readSSHDSettings(path, got, 0)
	val := func(k, def string) string {
		if v, ok := got[k]; ok {
			return strings.ToLower(v)
		}
		return def
	}
	return SSHDAuth{
		Passwords: val("passwordauthentication", "yes") == "yes" ||
			(val("kbdinteractiveauthentication", val("challengeresponseauthentication", "yes")) == "yes" &&
				val("usepam", "no") == "yes"),
		Keys:            val("pubkeyauthentication", "yes") == "yes",
		ExposeAuthInfo:  val("exposeauthinfo", "no") == "yes",
		PermitRootLogin: val("permitrootlogin", "prohibit-password"),
	}
}

// readSSHDSettings adds path's settings to got, following Include. depth
// bounds a loop of files that include each other.
func readSSHDSettings(path string, got map[string]string, depth int) {
	if depth > 8 {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.IndexAny(line, " \t=")
		if i < 0 {
			continue
		}
		key := strings.ToLower(line[:i])
		value := strings.Trim(strings.TrimLeft(line[i:], " \t="), `"`)
		switch key {
		case "match":
			// Everything after the first Match belongs to a block.
			return
		case "include":
			for _, pattern := range strings.Fields(value) {
				if !filepath.IsAbs(pattern) {
					pattern = filepath.Join(filepath.Dir(sshdConfigPath), pattern)
				}
				matches, _ := filepath.Glob(pattern)
				sort.Strings(matches)
				for _, m := range matches {
					readSSHDSettings(m, got, depth+1)
				}
			}
		default:
			if _, seen := got[key]; !seen {
				got[key] = value
			}
		}
	}
}

// readShadow maps each account to its password state. A file that cannot be
// read gives nil, and every account's password is then unknown.
func readShadow(path string) map[string]string {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.SplitN(sc.Text(), ":", 3)
		if len(fields) < 2 {
			continue
		}
		out[fields[0]] = passwordState(fields[1])
	}
	return out
}

// passwordState reads shadow(5)'s password field.
func passwordState(hash string) string {
	switch {
	case hash == "":
		return PasswordNone
	case strings.HasPrefix(hash, "!"), strings.HasPrefix(hash, "*"):
		return PasswordLocked
	}
	return PasswordSet
}

// scanLoginAccounts lists the accounts with a login shell, with what each
// could sign in with.
func scanLoginAccounts(cfg ScanConfig, auth SSHDAuth) []LoginAccount {
	accounts, err := readAccounts(cfg.PasswdFile)
	if err != nil {
		return nil
	}
	shadow := readShadow(cfg.ShadowFile)
	out := make([]LoginAccount, 0, len(accounts))
	for _, acct := range accounts {
		la := LoginAccount{User: acct.name, Password: PasswordUnknown}
		if shadow != nil {
			if s, ok := shadow[acct.name]; ok {
				la.Password = s
			}
		}
		for _, name := range []string{"authorized_keys", "authorized_keys2"} {
			la.Keys += countKeys(filepath.Join(acct.home, ".ssh", name))
		}
		la.CanLogin = canLogin(la, auth)
		out = append(out, la)
	}
	return out
}

// canLogin is whether sshd would let la in. An unknown password counts as
// one, so an account is never left off for want of reading shadow.
func canLogin(la LoginAccount, auth SSHDAuth) bool {
	if la.User == "root" && auth.PermitRootLogin == "no" {
		return false
	}
	byKey := auth.Keys && la.Keys > 0 &&
		!(la.User == "root" && strings.HasPrefix(auth.PermitRootLogin, "forced-commands"))
	byPassword := auth.Passwords && (la.Password == PasswordSet || la.Password == PasswordUnknown) &&
		!(la.User == "root" && auth.PermitRootLogin != "yes")
	return byKey || byPassword
}

// countKeys counts the keys in one authorized_keys file.
func countKeys(path string) int {
	keys, err := parseAuthorizedKeysFile(path, "", map[string]bool{})
	if err != nil {
		return 0
	}
	return len(keys)
}
