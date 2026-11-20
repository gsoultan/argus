package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
The website check (tunnel/sitecheck.go): this server fetches a website the
way a visitor here would and says how it went. argus-control asks only
when its own check failed, so this server is the second opinion that keeps
a problem on Argus's side from being called the site being down.

One GET with tunnel.SiteProbe, on a grant naming the address. Nothing of
the page is sent back: a status, a time, and whether the word was there.
*/

// SiteCheckService is the website check's tunnel service.
const SiteCheckService = tunnel.SiteCheckService

// SiteCheckConfig is what the website check needs.
type SiteCheckConfig struct {
	Verifier *tunnel.GrantVerifier
	Log      *slog.Logger
	// Probe stands in for the network in tests.
	Probe *tunnel.SiteProbe
	// Version names the agent in the User-Agent.
	Version string
}

// SiteCheck is the website check service.
func SiteCheck(cfg SiteCheckConfig) TunnelService {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	probe := tunnel.SiteProbe{UserAgent: "Argus-Website-Check (argus-agent " + cfg.Version + ")"}
	if cfg.Probe != nil {
		probe = *cfg.Probe
	}
	return func(ctx context.Context, call *Call) {
		if call.GrantKeyErr != nil {
			call.Refuse(call.GrantKeyErr.Error())
			return
		}
		g, err := cfg.Verifier.Verify(call.GrantKey, call.Header.Grant)
		if err != nil {
			log.Warn("tunnel: refused a website check", "error", err)
			call.Refuse(err.Error())
			return
		}
		if !g.ForService(SiteCheckService) || g.Site == nil {
			call.Refuse(tunnel.ErrGrant{Reason: "it is not for a website check"}.Error())
			return
		}
		q := *g.Site
		if err := q.Validate(); err != nil {
			call.Refuse(err.Error())
			return
		}
		if err := call.Accept(""); err != nil {
			return
		}
		res := probe.Probe(ctx, q)
		if h, err := os.Hostname(); err == nil {
			res.From = h
		}
		log.Info("tunnel: checked a website", "ok", res.OK, "status", res.Status, "fail", res.Fail, "for", g.Email)
		b, _ := json.Marshal(res)
		_ = tunnel.WriteFrame(call.Stream, tunnel.FrameSiteResult, b)
		_ = call.Stream.Close()
	}
}
