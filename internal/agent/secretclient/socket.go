package secretclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// The local socket lets a process on this host that cannot read the agent's
// credential (anything not running as root) ask the agent for a value. Who
// may connect is the socket file's mode: 0600 root only, or 0660 with the
// group named in SocketGroup when that group exists. Every read carries the
// caller's uid and pid, as the kernel gives them, into the server's read log,
// so a read through the socket is as attributable as one by the agent.

// SocketGroup is the group whose members may use the socket.
const SocketGroup = "argus-secrets"

// Peer is who connected, from the kernel.
type Peer struct {
	UID, PID int
	Known    bool
}

// Socket serves reads on a unix socket.
type Socket struct {
	Client Reader
	Log    *slog.Logger
	// PeerOf asks the kernel who is on a connection.
	PeerOf func(net.Conn) (Peer, error)
}

// Reader is anything that reads a value: the Client itself, or a
// SocketClient for a process that is not root.
type Reader interface {
	Read(ctx context.Context, ref Ref, purpose string) (Value, error)
}

type peerKey struct{}

// socketRequest is one read.
type socketRequest struct {
	Ref     string `json:"ref"`
	Purpose string `json:"purpose,omitempty"`
}

type socketAnswer struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
	Field   string `json:"field,omitempty"`
	Value   []byte `json:"value"`
}

// Listen makes the socket at path, ready for Serve: mode 0660 for gid when
// gid >= 0, else 0600.
func Listen(path string, gid int) (net.Listener, error) {
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	mode := os.FileMode(0o600)
	if gid >= 0 {
		if err := os.Chown(path, 0, gid); err != nil {
			_ = ln.Close()
			return nil, fmt.Errorf("give the secrets socket to its group: %w", err)
		}
		mode = 0o660
	}
	if err := os.Chmod(path, mode); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

// Serve answers on ln until ctx ends.
func (s *Socket) Serve(ctx context.Context, ln net.Listener) error {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/read", s.read)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 30 * time.Second, ErrorLog: nil,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			p := Peer{}
			if s.PeerOf != nil {
				if got, err := s.PeerOf(c); err == nil {
					p = got
				}
			}
			return context.WithValue(ctx, peerKey{}, p)
		}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	<-done
	return err
}

func (s *Socket) read(w http.ResponseWriter, r *http.Request) {
	p, _ := r.Context().Value(peerKey{}).(Peer)
	if !p.Known {
		// The socket's mode let them in, but the read log must name them.
		writeSocketErr(w, http.StatusForbidden, "the agent cannot tell who is asking on this system")
		return
	}
	var req socketRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeSocketErr(w, http.StatusBadRequest, "send {\"ref\":\"folder/name#field\"}")
		return
	}
	ref, err := ParseRef(req.Ref)
	if err != nil {
		writeSocketErr(w, http.StatusBadRequest, err.Error())
		return
	}
	purpose := fmt.Sprintf("socket uid=%d pid=%d", p.UID, p.PID)
	if exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", p.PID)); err == nil {
		purpose += " exe=" + exe
	}
	if q := strings.TrimSpace(req.Purpose); q != "" {
		if len(q) > 200 {
			q = q[:200]
		}
		purpose += ": " + q
	}
	v, err := s.Client.Read(r.Context(), ref, purpose)
	if err != nil {
		if s.Log != nil {
			s.Log.Info("secret read refused on the socket", "ref", ref.Name, "uid", p.UID, "pid", p.PID, "error", err)
		}
		writeSocketErr(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(socketAnswer{Name: v.Name, Version: v.Version, Field: v.Field, Value: v.Bytes})
	clear(v.Bytes)
}

func writeSocketErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// SocketClient reads through the agent's socket.
type SocketClient struct {
	Path string
}

// Read asks the agent.
func (c SocketClient) Read(ctx context.Context, ref Ref, purpose string) (Value, error) {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", c.Path)
	}}
	defer tr.CloseIdleConnections()
	cl := &http.Client{Transport: tr, Timeout: 30 * time.Second}
	s := ref.Name
	if ref.Field != "" {
		s += "#" + ref.Field
	}
	if ref.Version > 0 {
		s += fmt.Sprintf("@%d", ref.Version)
	} else if ref.Label != "" {
		s += "@" + ref.Label
	}
	body, _ := json.Marshal(socketRequest{Ref: s, Purpose: purpose})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://argus-agent/v1/read", strings.NewReader(string(body)))
	if err != nil {
		return Value{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := cl.Do(req)
	if err != nil {
		return Value{}, fmt.Errorf("the agent's secrets socket %s: %w", c.Path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if resp.StatusCode != http.StatusOK {
		return Value{}, errors.New(answerError(raw, resp.Status))
	}
	var a socketAnswer
	if err := json.Unmarshal(raw, &a); err != nil {
		return Value{}, errors.New("the agent answered something this command cannot read")
	}
	return Value{Name: a.Name, Version: a.Version, Field: a.Field, Bytes: a.Value}, nil
}
