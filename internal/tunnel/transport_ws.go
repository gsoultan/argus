package tunnel

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

/*
yamux over a WebSocket, for a control plane behind a proxy that forwards
browser traffic only.

A Cloudflare Tunnel -- and most reverse proxies set up for a web app --
forward an HTTP upgrade only when it is to WebSocket. The HTTPS fallback's own
upgrade token is dropped, and the agent never gets a tunnel. Over a WebSocket
the same yamux session goes through.

It is the last thing an agent tries, and only after the plain upgrade was
answered by something other than the control plane, so an agent that reaches
the control plane directly never pays for it. What it costs where it is used is
the framing: a few bytes per message, and messages are as large as yamux's
writes. Compression is off: what crosses a tunnel is either already
compressed (recordings, desktop frames) or too small to gain from it, and
deflate on every frame costs CPU at both ends for nothing.

No QUIC port is offered over this transport. The agent got here because
something in front of the control plane carries HTTP only, and the same thing
will not carry UDP to the same name: offering the port would only make the
agent try it and wait out the timeout.
*/

// TransportWebSocket is yamux carried in WebSocket messages.
const TransportWebSocket Transport = "websocket"

// IsWebSocketUpgrade reports whether r asks for a WebSocket.
func IsWebSocketUpgrade(r *http.Request) bool {
	return headerHasToken(r.Header, "Connection", "upgrade") &&
		headerHasToken(r.Header, "Upgrade", "websocket")
}

// AcceptWebSocket runs the control plane's side of a WebSocket tunnel. Headers
// already set on w, such as the grant key, go out with the handshake.
func AcceptWebSocket(w http.ResponseWriter, r *http.Request) (Session, error) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return nil, err
	}
	// yamux frames its own messages and bounds its own windows; the
	// WebSocket only carries bytes.
	c.SetReadLimit(-1)
	conn := websocket.NetConn(context.Background(), c, websocket.MessageBinary)
	cfg := yamuxConfig()
	cfg.AcceptBacklog = 4
	s, err := yamux.Server(conn, cfg)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return wsSession{muxSession{s: s, conn: conn}}, nil
}

type wsSession struct{ muxSession }

func (s wsSession) Transport() Transport { return TransportWebSocket }

// DialWebSocket connects through the control plane's HTTPS address over a
// WebSocket. Used when DialUpgrade's own upgrade did not reach it.
func DialWebSocket(ctx context.Context, o DialOptions) (Session, error) {
	u, err := url.Parse(o.ControlURL)
	if err != nil {
		return nil, err
	}
	target := url.URL{Scheme: "wss", Host: u.Host, Path: UpgradePath}
	if u.Scheme == "http" {
		target.Scheme = "ws"
	}
	tlsCfg := clientTLS(o.TLS, u.Hostname())
	// HTTP/1.1 only: the WebSocket handshake is an HTTP/1.1 upgrade.
	tlsCfg.NextProtos = []string{"http/1.1"}
	hc := &http.Client{Transport: &http.Transport{
		TLSClientConfig:   tlsCfg,
		DialContext:       (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		ForceAttemptHTTP2: false,
	}}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+o.Credential)
	h.Set("Argus-Hostname", o.Hostname)
	h.Set("Argus-Version", o.Version)

	// The context bounds the handshake only; the connection outlives it.
	c, resp, err := websocket.Dial(ctx, target.String(), &websocket.DialOptions{
		HTTPClient:      hc,
		HTTPHeader:      h,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			body := ""
			if resp.Body != nil {
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
				body = string(b)
			}
			return nil, ErrRefused{Reason: fmt.Sprintf("%d %s", resp.StatusCode, body)}
		}
		return nil, err
	}
	c.SetReadLimit(-1)
	conn := websocket.NetConn(context.Background(), c, websocket.MessageBinary)
	s, err := yamux.Client(conn, yamuxConfig())
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return wsSession{muxSession{s: s, conn: conn, grantKey: resp.Header.Get(GrantKeyHeader)}}, nil
}

// headerHasToken reports whether a comma-separated header holds token,
// ignoring case: "Connection: keep-alive, Upgrade" asks for an upgrade.
func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}
