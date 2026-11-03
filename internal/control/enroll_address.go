package control

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/gsoultan/argus/internal/tlsconfig"
)

/*
Is the pin right for the address the install command uses?

The pin is of the certificate this process serves. The command the console
shows fetches from the address servers use, which is the console's own
address unless Settings says otherwise. When something in front of that
address ends TLS itself -- a Cloudflare Tunnel, a load balancer -- curl meets
that thing's certificate, the pin cannot match, and the command fails with
"public key does not match pinned public key". An agent could not get through
such a proxy anyway: its tunnel and its reports need this process directly.

So before handing out a pin, the control plane connects to that address the
way a server would and looks at the key it is shown. The same key: the pin is
right. Another key from a publicly trusted certificate (Cloudflare's): no pin
is needed, curl and the agent verify it the ordinary way, and agents connect
through it over a WebSocket (see tunnel/transport_ws.go). Another key that
nothing public vouches for: the command would fail, and the console says what
to set instead of showing it. No answer at all (a control plane that cannot reach its
own public name, which is common behind NAT) says nothing either way, and the
pin is handed out as before.

The address probed is the configured one, or the Host the console was opened
at. Never one from the request body: the probe is a TLS handshake and no more,
but it should not be a way to ask this host to dial anywhere.
*/

// pinProbeTimeout bounds the probe. Minting a token waits for it.
const pinProbeTimeout = 4 * time.Second

// PinProbeResult is what a probe saw at an address.
type PinProbeResult struct {
	// Pin is of the key the server presented.
	Pin string
	// Public says the system trust store accepts the certificate for the
	// address's name, as curl and the agent would without a pin.
	Public bool
}

// probeServerPin connects to hostport and reports the key the server there
// presents and whether a public CA vouches for it. It trusts nothing: it only
// reports what it saw.
func probeServerPin(ctx context.Context, hostport string) (PinProbeResult, error) {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return PinProbeResult{}, err
	}
	d := tls.Dialer{Config: &tls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS12,
		// What is compared is the key, after the handshake; a chain check
		// would only stop the probe from seeing it.
		InsecureSkipVerify: true, //nolint:gosec // reads the key, trusts nothing
	}}
	conn, err := d.DialContext(ctx, "tcp", hostport)
	if err != nil {
		return PinProbeResult{}, err
	}
	defer conn.Close()
	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return PinProbeResult{}, errors.New("no certificate presented")
	}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	_, verr := certs[0].Verify(x509.VerifyOptions{Intermediates: inter, DNSName: host})
	return PinProbeResult{Pin: tlsconfig.Pin(certs[0]), Public: verr == nil}, nil
}

// enrolAddress is where an install command made from r reaches this control
// plane: the configured address servers use, or the address the console was
// opened at, as the console itself works it out.
func (a *API) enrolAddress(r *http.Request) string {
	if a.FleetURL != "" {
		return a.FleetURL
	}
	if r.Host == "" {
		return ""
	}
	return "https://" + r.Host
}

// enrolPin returns the pin an install command made from r should carry. When
// the address it uses presents a key other than this process's, it returns no
// pin and the problem, in words for the person adding the server.
func (a *API) enrolPin(r *http.Request) (pin, address, problem string) {
	if a.Enroll.Pin == nil {
		return "", "", ""
	}
	own, err := a.Enroll.Pin()
	if err != nil {
		a.log.Error("could not compute the control plane's certificate pin", "error", err)
		return "", "", ""
	}
	if own == "" {
		return "", "", ""
	}
	address = a.enrolAddress(r)
	u, err := CheckURL(address, "the address servers use", true)
	if err != nil {
		return own, address, ""
	}
	hostport := u.Host
	if u.Port() == "" {
		hostport = net.JoinHostPort(u.Hostname(), "443")
	}

	probe := a.PinProbe
	if probe == nil {
		probe = probeServerPin
	}
	ctx, cancel := context.WithTimeout(r.Context(), pinProbeTimeout)
	defer cancel()
	seen, err := probe(ctx, hostport)
	if err != nil {
		// Unknown is not wrong: keep the pin, as before there was a probe.
		a.log.Info("could not check the enrollment address's certificate; keeping the pin",
			"address", address, "error", err)
		return own, address, ""
	}
	if seen.Pin == own {
		return own, address, ""
	}
	if seen.Public {
		// A proxy in front with a public certificate. Ordinary verification
		// is the check there, and agents get through it over a WebSocket.
		a.log.Info("the enrollment address presents a public certificate; no pin needed",
			"address", address)
		return "", address, ""
	}
	a.log.Warn("the enrollment address presents another certificate; not handing out a pin",
		"address", address, "presented", seen.Pin, "own", own)
	return "", address, address + " answers with a certificate that is neither this control " +
		"plane's own nor one a public authority vouches for, so something in front of it ends " +
		"TLS there and servers cannot trust it. Give that proxy a publicly trusted certificate, " +
		"or in Settings, under Addresses, set \"Address servers use\" to an address your " +
		"servers reach this control plane on directly, then add the server again."
}
