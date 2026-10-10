package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tlsconfig"
)

// EnrollmentFile is where an enrolled agent keeps what it reports with,
// relative to its state directory. Root-only: the credential in it is this
// host's identity to the control plane.
const EnrollmentFile = "enrollment.json"

// enrolledCAFile is the control plane's CA, kept beside the enrollment when
// the control plane handed one over.
const enrolledCAFile = "control-ca.pem"

// Enrollment is what `argus-agent enroll` writes and the daemon reads.
type Enrollment struct {
	ControlURL string    `json:"controlUrl"`
	Hostname   string    `json:"hostname"`
	Credential string    `json:"credential"`
	EnrolledAt time.Time `json:"enrolledAt"`
	// Exactly one way of trusting the control plane, or none for the system
	// trust store. CAFile survives a certificate renewal; Pin does not, and is
	// kept only when the CA the control plane sent could not verify it.
	CAFile string `json:"caFile,omitempty"`
	Pin    string `json:"pin,omitempty"`
}

// EnrollOptions is what the person at the terminal supplied.
type EnrollOptions struct {
	ControlURL string
	Token      string
	// Pin is from the console, beside the token. Optional when the control
	// plane's certificate is publicly trusted or CAFile is given.
	Pin string
	// CAFile overrides both the pin and anything the control plane sends.
	CAFile   string
	Hostname string
	StateDir string
	// Force replaces an existing enrollment. Without it, enrolling twice is
	// refused: the first credential would be lost while still live.
	Force bool
	// Transport overrides the HTTP transport, for tests. Its TLS is still
	// built from the options above.
	Transport *http.Transport
}

// Enroll exchanges a token for this host's credential and writes it to the
// state directory. Returns where it was written.
func Enroll(ctx context.Context, opts EnrollOptions) (Enrollment, error) {
	u, err := url.Parse(opts.ControlURL)
	if err != nil || u.Host == "" {
		return Enrollment{}, fmt.Errorf("control URL %q is not a URL", opts.ControlURL)
	}
	if u.Scheme != "https" {
		// The credential would cross in the clear, and so would every report.
		return Enrollment{}, fmt.Errorf("control URL must be https://, got %q", opts.ControlURL)
	}
	if opts.Token == "" {
		return Enrollment{}, errors.New("an enrollment token is required; make one under Servers > Add a server")
	}
	if opts.Hostname == "" {
		if opts.Hostname, err = os.Hostname(); err != nil {
			return Enrollment{}, fmt.Errorf("hostname: %w", err)
		}
	}
	path := filepath.Join(opts.StateDir, EnrollmentFile)
	if _, err := os.Stat(path); err == nil && !opts.Force {
		return Enrollment{}, fmt.Errorf("%s already exists: this host is enrolled. "+
			"Revoke it in the console and pass --force to enroll again", path)
	}

	var tlsCfg *tls.Config
	switch {
	case opts.CAFile != "":
		tlsCfg, err = tlsconfig.Client(tlsconfig.ClientOptions{CAFile: opts.CAFile})
	case opts.Pin != "":
		tlsCfg, err = tlsconfig.Pinned(opts.Pin)
	default:
		tlsCfg, err = tlsconfig.Client(tlsconfig.ClientOptions{})
	}
	if err != nil {
		return Enrollment{}, err
	}
	transport := opts.Transport
	if transport == nil {
		transport = http.DefaultTransport.(*http.Transport).Clone()
	}
	transport.TLSClientConfig = tlsCfg
	client := &http.Client{Timeout: 30 * time.Second, Transport: transport}

	body, _ := json.Marshal(map[string]string{"token": opts.Token, "hostname": opts.Hostname})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(opts.ControlURL, "/")+"/api/v1/agents/enroll", bytes.NewReader(body))
	if err != nil {
		return Enrollment{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return Enrollment{}, fmt.Errorf("reach the control plane: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct{ Error string }
		_ = json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(raw))
		}
		return Enrollment{}, fmt.Errorf("the control plane refused enrollment (%d): %s",
			resp.StatusCode, e.Error)
	}
	var out struct {
		Credential string `json:"credential"`
		Hostname   string `json:"hostname"`
		CAPEM      string `json:"caPem"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Credential == "" {
		return Enrollment{}, fmt.Errorf("unexpected enrollment response: %s", raw)
	}

	enr := Enrollment{
		ControlURL: strings.TrimRight(opts.ControlURL, "/"),
		Hostname:   out.Hostname,
		Credential: out.Credential,
		EnrolledAt: time.Now().UTC(),
	}
	if err := os.MkdirAll(opts.StateDir, 0o700); err != nil {
		return Enrollment{}, err
	}

	// Decide how every later request trusts the control plane. Whatever is
	// kept has to verify the server just spoken to, or the first heartbeat
	// would fail with the credential already spent.
	switch {
	case opts.CAFile != "":
		enr.CAFile = opts.CAFile
	case opts.Pin == "":
		// System trust verified this connection; it will verify the next.
	case out.CAPEM != "" && resp.TLS != nil &&
		tlsconfig.VerifiesServer([]byte(out.CAPEM), *resp.TLS, u.Hostname()) == nil:
		caPath := filepath.Join(opts.StateDir, enrolledCAFile)
		if err := writePrivate(caPath, []byte(out.CAPEM)); err != nil {
			return Enrollment{}, err
		}
		enr.CAFile = caPath
	case resp.TLS != nil && verifiesWithSystemRoots(*resp.TLS, u.Hostname()):
		// A public certificate after all; the pin was belt and braces.
	default:
		// Works now, and stops working when the control plane's key changes.
		// Kept rather than failing, because the credential is already spent.
		enr.Pin = opts.Pin
	}

	data, _ := json.MarshalIndent(enr, "", "  ")
	if err := writePrivate(path, data); err != nil {
		return Enrollment{}, err
	}
	return enr, nil
}

// LoadEnrollment reads what Enroll wrote. A missing file is (nil, nil): the
// agent was never enrolled, which is a supported way to run.
func LoadEnrollment(stateDir string) (*Enrollment, error) {
	raw, err := os.ReadFile(filepath.Join(stateDir, EnrollmentFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var e Enrollment
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, fmt.Errorf("%s: %w", EnrollmentFile, err)
	}
	if e.ControlURL == "" || e.Credential == "" {
		return nil, fmt.Errorf("%s is incomplete; enroll again", EnrollmentFile)
	}
	return &e, nil
}

// TLS is how the daemon trusts the control plane for this enrollment.
func (e *Enrollment) TLS() (*tls.Config, error) {
	switch {
	case e.CAFile != "":
		return tlsconfig.Client(tlsconfig.ClientOptions{CAFile: e.CAFile})
	case e.Pin != "":
		return tlsconfig.Pinned(e.Pin)
	default:
		return tlsconfig.Client(tlsconfig.ClientOptions{})
	}
}

// verifiesWithSystemRoots reports whether the system trust store alone would
// have accepted this server.
func verifiesWithSystemRoots(cs tls.ConnectionState, serverName string) bool {
	if len(cs.PeerCertificates) == 0 {
		return false
	}
	inter := x509.NewCertPool()
	for _, c := range cs.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
		Intermediates: inter, DNSName: serverName,
	})
	return err == nil
}

// writePrivate writes a file only root can read, replacing it atomically so a
// crash never leaves half a credential.
func writePrivate(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
