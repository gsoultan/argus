package main

import (
	"os"
	"path/filepath"

	"github.com/gsoultan/argus/internal/control"
)

// controlHostDir is where argus-control leaves the request for the helper
// on its own server; argus-control-host-enroll.path watches it.
func controlHostDir(loc configLocation) string {
	return filepath.Join(loc.dataDir(), "host")
}

// controlHostOptions is what the loop needs to ask the helper on this
// server to enroll, or why it cannot here.
func controlHostOptions(cfg config, loc configLocation, api *control.API) (control.ControlHostOptions, string) {
	if why := control.ControlHostSupport(); why != "" {
		return control.ControlHostOptions{}, why
	}
	if cfg.TLS != nil && cfg.TLS.ClientCAFile != "" {
		return control.ControlHostOptions{}, "argus-control asks agents for a client certificate " +
			"(tls.client_ca_file), and the helper on its own server enrolls with a token alone"
	}
	if cfg.TLS != nil && api.Enroll.Pin == nil {
		return control.ControlHostOptions{}, "argus-control's certificate is not its own " +
			"(behind_tls_proxy), so the helper on its own server has nothing to pin"
	}
	url, err := control.ControlHostURL(cfg.Listen, cfg.TLS != nil)
	if err != nil {
		return control.ControlHostOptions{}, err.Error()
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		return control.ControlHostOptions{}, "this server has no hostname to enroll under"
	}
	return control.ControlHostOptions{
		Dir: controlHostDir(loc), Hostname: host, ControlURL: url, Pin: api.Enroll.Pin,
	}, ""
}
