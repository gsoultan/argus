package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
One attempt of a Monitoring check (tunnel/checkrun.go), run here because
this server is near what it checks: a network device on the same LAN as this
server, which argus-control cannot reach itself.

The grant names the kind and its settings. A kind this agent does not know is
refused with "unknown check kind", so argus-control can say this agent needs
updating instead of calling the device down.
*/

// CheckRunService is the Monitoring attempt's tunnel service.
const CheckRunService = tunnel.CheckRunService

// CheckRunConfig is what the attempts need.
type CheckRunConfig struct {
	Verifier *tunnel.GrantVerifier
	Log      *slog.Logger
	// SNMP stands in for the network in tests.
	SNMP *tunnel.SNMPProbe
}

// CheckRun is the Monitoring attempt service.
func CheckRun(cfg CheckRunConfig) TunnelService {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	snmp := tunnel.SNMPProbe{}
	if cfg.SNMP != nil {
		snmp = *cfg.SNMP
	}
	return func(ctx context.Context, call *Call) {
		if call.GrantKeyErr != nil {
			call.Refuse(call.GrantKeyErr.Error())
			return
		}
		g, err := cfg.Verifier.Verify(call.GrantKey, call.Header.Grant)
		if err != nil {
			log.Warn("tunnel: refused a monitoring check", "error", err)
			call.Refuse(err.Error())
			return
		}
		if !g.ForService(CheckRunService) || g.Check == nil {
			call.Refuse(tunnel.ErrGrant{Reason: "it is not for a monitoring check"}.Error())
			return
		}
		q := *g.Check
		if err := q.Validate(); err != nil {
			if errors.Is(err, tunnel.ErrUnknownCheckKind) {
				call.Refuse(tunnel.ErrUnknownCheckKind.Error() + " " + q.Kind)
				return
			}
			call.Refuse(err.Error())
			return
		}
		if err := call.Accept(""); err != nil {
			return
		}
		var res tunnel.AttemptResult
		switch q.Kind {
		case tunnel.CheckKindSNMP:
			res = snmp.Probe(ctx, *q.SNMP)
		case tunnel.CheckKindSNMPDiscover:
			dr := snmp.Discover(ctx, *q.Discover)
			res = tunnel.AttemptResult{Kind: q.Kind, OK: dr.Found, Discovery: &dr}
		}
		if h, err := os.Hostname(); err == nil {
			res.From = h
		}
		log.Info("tunnel: ran a monitoring check", "kind", q.Kind, "ok", res.OK, "fail", res.Fail)
		b, _ := json.Marshal(res)
		_ = tunnel.WriteFrame(call.Stream, tunnel.FrameAttemptResult, b)
		_ = call.Stream.Close()
	}
}
