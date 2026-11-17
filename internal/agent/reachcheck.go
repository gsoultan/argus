package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
The reach check (tunnel/reachcheck.go): this server connects to a database
port on another, the way an application here would, and says what
happened. One TCP connection; for Redis one PING and one reply line. No
password is sent or held.
*/

const reachTimeout = 5 * time.Second

// ReachCheckService is the reach check's tunnel service.
const ReachCheckService = tunnel.ReachCheckService

// ReachCheckConfig is what the reach check needs.
type ReachCheckConfig struct {
	Verifier *tunnel.GrantVerifier
	Log      *slog.Logger
	// Dial stands in for the network in tests.
	Dial func(ctx context.Context, addr string) (net.Conn, error)
}

// reach makes the one connection.
func reach(ctx context.Context, dial func(context.Context, string) (net.Conn, error), g tunnel.GrantReach) tunnel.ReachResult {
	start := time.Now()
	res := tunnel.ReachResult{}
	if h, err := os.Hostname(); err == nil {
		res.From = h
	}
	cctx, cancel := context.WithTimeout(ctx, reachTimeout)
	defer cancel()
	addr := net.JoinHostPort(g.Host, strconv.Itoa(g.Port))
	c, err := dial(cctx, addr)
	res.MS = time.Since(start).Milliseconds()
	if err != nil {
		switch {
		case errors.Is(err, syscall.ECONNREFUSED):
			res.Detail = "connection refused: nothing listens on " + addr + " for this machine, or a firewall rejects it"
		case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
			res.Detail = "no answer in " + reachTimeout.String() + ": a firewall on the way drops the connection"
		default:
			res.Detail = err.Error()
		}
		return res
	}
	defer c.Close()
	res.Connected = true
	if g.Engine != tunnel.EngineRedis {
		return res
	}
	_ = c.SetDeadline(time.Now().Add(reachTimeout))
	if _, err := c.Write([]byte("*1\r\n$4\r\nPING\r\n")); err != nil {
		res.Detail = "connected, then the connection closed: " + err.Error()
		return res
	}
	line, err := bufio.NewReaderSize(c, 512).ReadString('\n')
	res.MS = time.Since(start).Milliseconds()
	line = strings.TrimSpace(line)
	switch {
	case err != nil && line == "":
		res.Detail = "connected, but Redis did not answer: " + err.Error()
	case strings.HasPrefix(line, "-NOAUTH"):
		res.Answer = "signin"
	case strings.HasPrefix(line, "-DENIED"):
		res.Answer, res.Detail = "protected", "protected mode refused it: the default user has no password"
	case line == "+PONG":
		res.Answer = "open"
	default:
		res.Detail = "unexpected answer: " + tail([]byte(line), 120)
	}
	return res
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// ReachCheck is the reach check service.
func ReachCheck(cfg ReachCheckConfig) TunnelService {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	dial := cfg.Dial
	if dial == nil {
		dial = func(ctx context.Context, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		}
	}
	return func(ctx context.Context, call *Call) {
		if call.GrantKeyErr != nil {
			call.Refuse(call.GrantKeyErr.Error())
			return
		}
		g, err := cfg.Verifier.Verify(call.GrantKey, call.Header.Grant)
		if err != nil {
			log.Warn("tunnel: refused a reach check", "error", err)
			call.Refuse(err.Error())
			return
		}
		if !g.ForService(ReachCheckService) || g.Reach == nil {
			call.Refuse(tunnel.ErrGrant{Reason: "it is not for a reach check"}.Error())
			return
		}
		q := *g.Reach
		if err := q.Validate(); err != nil {
			call.Refuse(err.Error())
			return
		}
		if err := call.Accept(""); err != nil {
			return
		}
		res := reach(ctx, dial, q)
		log.Info("tunnel: checked a connection", "to", net.JoinHostPort(q.Host, strconv.Itoa(q.Port)),
			"connected", res.Connected, "answer", res.Answer, "for", g.Email)
		b, _ := json.Marshal(res)
		_ = tunnel.WriteFrame(call.Stream, tunnel.FrameReachResult, b)
		_ = call.Stream.Close()
	}
}
