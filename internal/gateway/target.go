package gateway

import (
	"fmt"
	"strings"
)

// Request is a parsed connection intent, derived from the SSH username.
//
// Encoding the target in the username is what lets Argus work with a stock
// ssh client — no wrapper script, no custom binary, no ProxyCommand. `scp`,
// `sftp`, `rsync` and Ansible all keep working because none of them know
// anything unusual is happening.
//
//	ssh ops:pay-01@argus.example.com
//	     └┬┘ └──┬──┘
//	      │     └── target host, resolved against the asset inventory
//	      └──────── principal to assume on that host
type Request struct {
	// Principal is the account to open on the target ("ops", "deploy", "root").
	Principal string
	// Target is the host as the user typed it; the inventory resolves it to an
	// address.
	Target string
}

// Separators accepted between principal and target. `:` is the primary form;
// `+` and `/` exist because some tooling mangles colons in usernames, and
// accepting all three costs nothing.
const separators = ":+/"

// ParseUsername splits an SSH username into a connection request.
//
// A username with no separator is rejected rather than guessed at. Defaulting
// the principal would mean a typo silently opens a session as the wrong
// account, which is exactly the kind of ambiguity a PAM tool must not have.
func ParseUsername(username string) (Request, error) {
	idx := strings.IndexAny(username, separators)
	if idx < 0 {
		return Request{}, fmt.Errorf(
			"username %q has no target: connect as principal:host@gateway (e.g. ops:pay-01@gateway)",
			username)
	}

	principal := strings.TrimSpace(username[:idx])
	target := strings.TrimSpace(username[idx+1:])

	if principal == "" {
		return Request{}, fmt.Errorf("username %q has an empty principal", username)
	}
	if target == "" {
		return Request{}, fmt.Errorf("username %q has an empty target", username)
	}
	// A second separator means the user typed something ambiguous; refuse
	// rather than silently taking the first split.
	if strings.ContainsAny(target, separators) {
		return Request{}, fmt.Errorf("username %q has more than one separator", username)
	}

	return Request{Principal: principal, Target: target}, nil
}
