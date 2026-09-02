package agent

import (
	"bufio"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"
)

// OriginDetector decides whether a session arrived through the gateway.
//
// Getting this wrong in the permissive direction is a security hole: a session
// misclassified as brokered is not recorded here, on the assumption the gateway
// already did it. So the rule is that only positive cryptographic proof counts
// as brokered, and everything else — including "cannot tell" — is Direct.
type OriginDetector struct {
	// GatewayKeyFingerprints are the SHA256 fingerprints of the keys the
	// gateway authenticates with. This is the authoritative signal.
	GatewayKeyFingerprints map[string]bool

	// GatewayAddrs is a weak secondary signal, used only when no auth info is
	// available. See the warning on Detect.
	GatewayAddrs []string
}

// Detect classifies the current session.
//
// The authoritative signal is the key that authenticated, exposed by sshd's
// `ExposeAuthInfo yes` via $SSH_USER_AUTH. That file is written by sshd as
// root, so the connecting user cannot forge it, and it survives NAT, port
// forwarding and load balancers.
//
// Source address is NOT trustworthy for this. Behind a port forward or a NAT
// gateway every client appears to come from the same address, so an
// address-based rule would classify real bypasses as brokered — and anyone able
// to route through that address could suppress their own recording. It is kept
// only as a last resort for hosts where ExposeAuthInfo is unavailable, and it
// is reported as such.
func (d OriginDetector) Detect() (origin Origin, reason string) {
	if fp, ok := authenticatedKeyFingerprint(); ok {
		if d.GatewayKeyFingerprints[fp] {
			return Brokered, "authenticated with a known gateway key"
		}
		return Direct, "authenticated with a key the gateway does not use: " + fp
	}

	// No auth info. sshd is either old or missing `ExposeAuthInfo yes`.
	if len(d.GatewayAddrs) > 0 {
		client, _, _ := strings.Cut(os.Getenv("SSH_CONNECTION"), " ")
		for _, gw := range d.GatewayAddrs {
			if gw != "" && client == gw {
				// Deliberately still Direct. Address matching cannot
				// distinguish the gateway from anything else sharing its
				// route, so treating this as brokered would let an attacker
				// behind the same NAT opt out of being recorded. Record it and
				// let the control plane reconcile the duplicate.
				return Direct, "source address matches the gateway, but that is not proof — recording anyway"
			}
		}
	}
	return Direct, "no gateway proof available"
}

// authenticatedKeyFingerprint reads $SSH_USER_AUTH, which sshd populates when
// `ExposeAuthInfo yes` is set. Its contents look like:
//
//	publickey ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA...
//
// One line per successful authentication method.
func authenticatedKeyFingerprint() (string, bool) {
	path := os.Getenv("SSH_USER_AUTH")
	if path == "" {
		return "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		method, rest, found := strings.Cut(line, " ")
		if !found || method != "publickey" {
			continue
		}
		pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(rest))
		if err != nil {
			continue
		}
		return ssh.FingerprintSHA256(pub), true
	}
	return "", false
}

// LoadGatewayFingerprints reads gateway public keys from a file, one per line
// in authorized_keys format.
func LoadGatewayFingerprints(path string) (map[string]bool, error) {
	out := map[string]bool{}
	if path == "" {
		return out, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	for len(data) > 0 {
		pub, _, _, rest, err := ssh.ParseAuthorizedKey(data)
		if err != nil {
			break
		}
		out[ssh.FingerprintSHA256(pub)] = true
		data = rest
	}
	return out, nil
}
