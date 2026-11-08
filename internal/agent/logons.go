package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Who is signed in on this host, for the console to show before a Remote
Desktop session opens; and, on Windows, signing out a session nobody is
using when every Remote Desktop session the host allows is taken. See
tunnel/logons.go.

The rule that makes signing out safe lives here, not only in the control
plane: a session is signed out only if it is still disconnected, is not the
console, and still belongs to the account the console showed. A grant that
asks for anything else is answered with a refusal and nothing happens.
*/

// LogonsService is the name the control plane asks for.
const LogonsService = tunnel.LogonsService

// logonsTimeout bounds one question.
const logonsTimeout = 20 * time.Second

// LogonsConfig is what the logons service needs.
type LogonsConfig struct {
	Verifier *tunnel.GrantVerifier
	Log      *slog.Logger
	// Host answers; nil is this platform's. Tests replace it.
	Host LogonsHost
}

// LogonsHost is the platform underneath.
type LogonsHost interface {
	// List is everyone signed in now.
	List(ctx context.Context) (tunnel.LogonsResult, error)
	// SignOut ends one session, waiting until it has.
	SignOut(ctx context.Context, session uint32) error
}

// Logons returns the logons service.
func Logons(cfg LogonsConfig) TunnelService {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	host := cfg.Host
	if host == nil {
		host = platformLogons()
	}
	return func(ctx context.Context, call *Call) {
		if call.GrantKeyErr != nil {
			call.Refuse(call.GrantKeyErr.Error())
			return
		}
		g, err := cfg.Verifier.Verify(call.GrantKey, call.Header.Grant)
		if err != nil {
			log.Warn("tunnel: refused a logons question", "error", err)
			call.Refuse(err.Error())
			return
		}
		if !g.ForService(LogonsService) || g.Logons == nil {
			log.Warn("tunnel: refused a logons question on a grant for another service",
				"service", g.Service, "for", g.Email)
			call.Refuse(tunnel.ErrGrant{Reason: "it is not for logons"}.Error())
			return
		}
		q := *g.Logons
		if err := q.Validate(); err != nil {
			call.Refuse(err.Error())
			return
		}
		if err := call.Accept(""); err != nil {
			return
		}
		cctx, cancel := context.WithTimeout(ctx, logonsTimeout)
		res := answerLogons(cctx, host, q)
		cancel()
		if q.Op == tunnel.LogonsSignOut {
			// Signing someone out is a change on the host: the agent's own log
			// says who asked, whatever the control plane records.
			log.Warn("tunnel: sign-out asked", "session", q.Session, "account", q.Account,
				"ok", res.OK, "error", res.Error, "for", g.Email)
		} else {
			log.Info("tunnel: answered a logons question", "ok", res.OK, "for", g.Email)
		}
		b, _ := json.Marshal(res)
		_ = tunnel.WriteFrame(call.Stream, tunnel.FrameLogonsResult, b)
		_ = call.Stream.Close()
	}
}

func answerLogons(ctx context.Context, host LogonsHost, q tunnel.GrantLogons) tunnel.LogonsResult {
	res, err := host.List(ctx)
	if err != nil {
		return tunnel.LogonsResult{Error: err.Error()}
	}
	if len(res.Logons) > tunnel.MaxLogons {
		res.Logons = res.Logons[:tunnel.MaxLogons]
	}
	if q.Op == tunnel.LogonsList {
		res.OK = true
		return res
	}
	if reason := refuseSignOut(res.Logons, q); reason != "" {
		return tunnel.LogonsResult{Error: reason}
	}
	if err := host.SignOut(ctx, q.Session); err != nil {
		return tunnel.LogonsResult{Error: "Windows did not sign the session out: " + err.Error()}
	}
	return tunnel.LogonsResult{OK: true, SignedOut: q.Session}
}

// refuseSignOut says why a session may not be signed out, or "" if it may.
func refuseSignOut(logons []tunnel.Logon, q tunnel.GrantLogons) string {
	for _, l := range logons {
		if l.Session != q.Session {
			continue
		}
		switch {
		case l.Account != q.Account:
			return "that session is now " + l.Account + "'s, not " + q.Account + "'s; nothing was signed out"
		case l.Console:
			return "that is the session on the server's own screen; Argus does not sign it out"
		case l.State != tunnel.LogonDisconnected:
			return q.Account + " is using that session right now; only a disconnected session can be signed out"
		}
		return ""
	}
	return "that session has already ended"
}
