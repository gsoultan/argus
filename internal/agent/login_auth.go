package agent

import (
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

/*
How a direct login signed in: a password, or which key.

sshd says so to the session when ExposeAuthInfo is on: SSH_USER_AUTH names a
file listing each method that succeeded, one per line, and a key method
carries the key ("publickey ssh-ed25519 AAAA..."). The shim runs as the person
logging in, so it reads that file and passes it on; the agent does not take it
on trust. A key is looked up in the account's authorized_keys as the agent
reads them now, and only a key found there is named by its comment and shown
as found. A key that is not there is still reported, by fingerprint, as one
the agent could not find.
*/

// maxUserAuth bounds what a shim may send as SSH_USER_AUTH. A real one is a
// few hundred bytes; certificates make it longer, not kilobytes longer.
const maxUserAuth = 16 << 10

// LoginAuth is how one direct login signed in.
type LoginAuth struct {
	// Method is what sshd accepted: "publickey", "password",
	// "keyboard-interactive", or several joined with "+". Empty when sshd
	// did not say (ExposeAuthInfo off).
	Method string `json:"method"`
	// KeyFingerprint and KeyType name the key, for a key login.
	KeyFingerprint string `json:"key_fingerprint,omitempty"`
	KeyType        string `json:"key_type,omitempty"`
	// KeyComment is the comment on that key in authorized_keys, or the
	// certificate's key ID; set only when the agent found it there.
	KeyComment string `json:"key_comment,omitempty"`
	// KeyFound is whether the key is in the account's authorized_keys.
	KeyFound bool `json:"key_found,omitempty"`
}

// parseUserAuth reads an SSH_USER_AUTH file. It returns nil when there is
// nothing usable in it.
func parseUserAuth(text string) *LoginAuth {
	if len(text) > maxUserAuth {
		text = text[:maxUserAuth]
	}
	var (
		methods []string
		out     LoginAuth
	)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		method, rest, _ := strings.Cut(line, " ")
		if !validAuthMethod(method) {
			continue
		}
		if len(methods) == 0 || methods[len(methods)-1] != method {
			methods = append(methods, method)
		}
		if method != "publickey" || out.KeyFingerprint != "" {
			continue
		}
		pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(rest))
		if err != nil {
			continue
		}
		out.KeyType = pub.Type()
		if cert, ok := pub.(*ssh.Certificate); ok {
			// A certificate is named by the key it certifies, which is what
			// a person holds, and by its key ID.
			out.KeyFingerprint = ssh.FingerprintSHA256(cert.Key)
			out.KeyComment = "certificate " + cert.KeyId
			continue
		}
		out.KeyFingerprint = ssh.FingerprintSHA256(pub)
	}
	if len(methods) == 0 {
		return nil
	}
	out.Method = strings.Join(methods, "+")
	return &out
}

func validAuthMethod(m string) bool {
	switch m {
	case "publickey", "password", "keyboard-interactive", "hostbased",
		"gssapi-with-mic", "gssapi-keyex", "none":
		return true
	}
	return false
}

// loginAuth works out how m signed in, checking a key against the account's
// authorized_keys. Nil when the shim had nothing to say.
func (c *Collector) loginAuth(m SessionStart) *LoginAuth {
	a := parseUserAuth(m.UserAuth)
	if a == nil || a.KeyFingerprint == "" {
		return a
	}
	isCert := strings.HasPrefix(a.KeyComment, "certificate ")
	if !isCert {
		// The shim's word is not a comment; only the file's is.
		a.KeyComment = ""
	}
	for _, k := range accountKeys(c.passwdFile(), m.Principal) {
		if k.Fingerprint == a.KeyFingerprint {
			a.KeyFound = true
			if !isCert {
				a.KeyComment = k.Comment
			}
			break
		}
	}
	return a
}

// passwdFile is where accounts are read from; tests point it elsewhere.
func (c *Collector) passwdFile() string {
	if c.PasswdFile != "" {
		return c.PasswdFile
	}
	return systemPasswd
}

// accountKeys is every key in one account's authorized_keys.
func accountKeys(passwdFile, user string) []UnmanagedKey {
	accounts, err := readAccounts(passwdFile)
	if err != nil {
		return nil
	}
	var out []UnmanagedKey
	for _, acct := range accounts {
		if acct.name != user {
			continue
		}
		for _, name := range []string{"authorized_keys", "authorized_keys2"} {
			keys, err := parseAuthorizedKeysFile(filepath.Join(acct.home, ".ssh", name),
				acct.name, map[string]bool{})
			if err == nil {
				out = append(out, keys...)
			}
		}
	}
	if adminKeysFile != "" {
		if keys, err := parseAuthorizedKeysFile(adminKeysFile, "Administrators", map[string]bool{}); err == nil {
			out = append(out, keys...)
		}
	}
	return out
}

// Describe says how a login signed in, in a phrase for the audit log.
func (a *LoginAuth) Describe() string {
	if a == nil || a.Method == "" {
		return "sshd did not say how it signed in (ExposeAuthInfo is off)"
	}
	if a.KeyFingerprint == "" {
		return "signed in with " + a.Method
	}
	key := a.KeyType + " key " + a.KeyFingerprint
	if a.KeyComment != "" {
		key += " (" + a.KeyComment + ")"
	}
	if !a.KeyFound {
		key += ", not in the account's authorized_keys now"
	}
	return "signed in with " + a.Method + " using " + key
}
