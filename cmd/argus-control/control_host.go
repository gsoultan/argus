package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/gsoultan/argus/internal/control"
	"github.com/gsoultan/argus/internal/control/servertls"
	"github.com/gsoultan/argus/internal/tlsconfig"
)

// defaultControlHostListen is where the helper reaches argus-control when
// the main listener cannot be pinned.
const defaultControlHostListen = "127.0.0.1:8449"

// controlHostSupport is control.ControlHostSupport, replaced in tests.
var controlHostSupport = control.ControlHostSupport

// controlHostDir is where argus-control leaves the request for the helper
// on its own server; argus-control-host-enroll.path watches it.
func controlHostDir(loc configLocation) string {
	return filepath.Join(loc.dataDir(), "host")
}

// controlHostPlan is how the helper on this server reaches argus-control.
type controlHostPlan struct {
	opts control.ControlHostOptions
	// Listen is set when the helper needs a listener of its own: a loopback
	// address served with CertFile and KeyFile, which the helper pins. That
	// is the case whenever argus-control does not present its own
	// certificate (plain HTTP, or behind_tls_proxy, as behind cloudflared),
	// and where it asks agents for a client certificate.
	Listen, CertFile, KeyFile string
}

// controlHostOptions is what the loop needs to ask the helper on this
// server to enroll, or why it cannot here.
func controlHostOptions(cfg config, loc configLocation, api *control.API) (controlHostPlan, string) {
	if why := controlHostSupport(); why != "" {
		return controlHostPlan{}, why
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		return controlHostPlan{}, "this server has no hostname to enroll under"
	}
	opts := control.ControlHostOptions{Dir: controlHostDir(loc), Hostname: host}

	// argus-control serves its own certificate: the helper pins that. Not
	// where it asks agents for a client certificate (tls.client_ca_file),
	// which the helper, enrolled with a token alone, has none of.
	if cfg.TLS != nil && cfg.TLS.ClientCAFile == "" && api.Enroll.Pin != nil {
		if url, err := control.ControlHostURL(cfg.Listen, true); err == nil {
			opts.ControlURL, opts.Pin = url, api.Enroll.Pin
			return controlHostPlan{opts: opts}, ""
		}
	}

	// Otherwise a loopback listener of the helper's own, with a key kept
	// beside the request so its pin outlives restarts.
	addr := cfg.ControlHostListen
	if addr == "" {
		addr = defaultControlHostListen
	}
	if err := loopbackOnly(addr); err != nil {
		return controlHostPlan{}, err.Error()
	}
	certFile, keyFile, err := servertls.LoopbackCert(filepath.Join(opts.Dir, "tls"))
	if err != nil {
		return controlHostPlan{}, "the certificate for the helper's own listener could not be made: " + err.Error()
	}
	url, err := control.ControlHostURL(addr, true)
	if err != nil {
		return controlHostPlan{}, err.Error()
	}
	opts.ControlURL = url
	opts.Pin = func() (string, error) { return tlsconfig.PinFile(certFile) }
	return controlHostPlan{opts: opts, Listen: addr, CertFile: certFile, KeyFile: keyFile}, ""
}

// loopbackOnly refuses a control_host_listen anything but this server could
// reach: it exists for the helper alone.
func loopbackOnly(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("control_host_listen %q: %w", addr, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("control_host_listen %q is not a loopback address: "+
			"only the helper on this server connects to it", addr)
	}
	return nil
}
