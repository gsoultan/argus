//go:build !windows

package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
File transfer through the tunnel: the console's scp.

	browser ──wss──▶ argus-control ──"files" stream──▶ agent ──▶ file-op, as the account

Opened on a grant like a terminal, but one for "files", so neither can be
spent on the other. Each operation runs `argus-agent file-op` as the account
-- in a transient unit on a systemd host, because the agent's own sandbox has
a read-only /home -- so the account's permissions decide what it may read and
write, as they would for scp. The agent counts and hashes what passes, and the
control plane writes each transfer into the audit chain.
*/

// Files is the files service.
func Files(cfg FilesConfig) TunnelService {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return func(ctx context.Context, call *Call) {
		if call.GrantKeyErr != nil {
			call.Refuse(call.GrantKeyErr.Error())
			return
		}
		g, err := cfg.Verifier.Verify(call.GrantKey, call.Header.Grant)
		if err != nil {
			log.Warn("tunnel: refused file transfer", "error", err)
			call.Refuse(err.Error())
			return
		}
		if !g.ForService(FilesService) {
			log.Warn("tunnel: refused file transfer on a grant for another service",
				"service", g.Service, "for", g.Email)
			call.Refuse(tunnel.ErrGrant{Reason: "it is not for file transfer"}.Error())
			return
		}
		acct, err := lookupAccount(g.Principal)
		if err != nil {
			call.Refuse(err.Error())
			return
		}
		if acct.uid != os.Geteuid() && os.Geteuid() != 0 {
			call.Refuse("the agent is not running as root, so it cannot transfer files as " + acct.name)
			return
		}
		if call.Accept("") != nil {
			_ = call.Stream.Close()
			return
		}
		log.Info("tunnel: file transfer opened", "principal", acct.name, "for", g.Email,
			"client", g.ClientAddr)
		f := &fileSession{cfg: cfg, acct: acct, actor: g.Email, log: log}
		f.serve(ctx, call.Stream)
		_ = call.Stream.Close()
	}
}

type fileSession struct {
	cfg   FilesConfig
	acct  loginAccount
	actor string
	log   *slog.Logger
}

func answer(w io.Writer, a tunnel.FileAnswer) error {
	b, _ := json.Marshal(a)
	return tunnel.WriteFrame(w, tunnel.FrameFileAnswer, b)
}

func (f *fileSession) serve(ctx context.Context, st tunnel.Stream) {
	fr := tunnel.NewFrameReader(st)
	for ctx.Err() == nil {
		typ, p, err := fr.Next()
		if err != nil {
			return
		}
		if typ != tunnel.FrameFileRequest {
			// Data with no upload open: the two sides disagree about where
			// they are, and nothing after this can be trusted to line up.
			return
		}
		var req tunnel.FileRequest
		if json.Unmarshal(p, &req) != nil {
			return
		}
		var opErr error
		switch req.Op {
		case "list":
			opErr = f.list(ctx, st, req)
		case "download":
			opErr = f.download(ctx, st, req)
		case "upload":
			opErr = f.upload(ctx, st, fr, req)
		default:
			opErr = answer(st, tunnel.FileAnswer{Error: "unknown operation " + req.Op})
		}
		if opErr != nil {
			return
		}
	}
}

func (f *fileSession) list(ctx context.Context, st tunnel.Stream, req tunnel.FileRequest) error {
	var out bytes.Buffer
	if err := f.run(ctx, []string{"list", req.Path}, nil, &out); err != nil {
		return answer(st, tunnel.FileAnswer{Error: err.Error()})
	}
	var ans tunnel.FileAnswer
	if json.Unmarshal(out.Bytes(), &ans) != nil {
		return answer(st, tunnel.FileAnswer{Error: "the listing could not be read"})
	}
	return answer(st, ans)
}

// download streams the file as it is read. The size is not known ahead --
// the account reads it, not the agent -- so the end answer carries it, with
// the hash of exactly the bytes sent.
func (f *fileSession) download(ctx context.Context, st tunnel.Stream, req tunnel.FileRequest) error {
	path, _ := f.resolve(req.Path)
	if err := answer(st, tunnel.FileAnswer{OK: true, Path: path}); err != nil {
		return err
	}
	w := &frameWriter{w: st, typ: tunnel.FrameFileData, h: sha256.New()}
	err := f.run(ctx, []string{"download", req.Path}, nil, w)
	if w.err != nil {
		return w.err
	}
	if err != nil {
		return answer(st, tunnel.FileAnswer{Error: err.Error(), Path: path})
	}
	f.log.Info("tunnel: file downloaded", "path", path, "bytes", w.n,
		"principal", f.acct.name, "for", f.actor)
	return answer(st, tunnel.FileAnswer{OK: true, Path: path, Size: w.n,
		SHA256: hex.EncodeToString(w.h.Sum(nil))})
}

func (f *fileSession) upload(ctx context.Context, st tunnel.Stream, fr *tunnel.FrameReader,
	req tunnel.FileRequest) error {

	path, _ := f.resolve(req.Path)
	if err := answer(st, tunnel.FileAnswer{OK: true, Path: path}); err != nil {
		return err
	}
	pr, pw := io.Pipe()
	h := sha256.New()
	var n int64
	// The stream is read here while the helper writes the file, so a large
	// upload is never held in memory.
	feedErr := make(chan error, 1)
	go func() {
		for {
			typ, p, err := fr.Next()
			if err != nil {
				_ = pw.CloseWithError(err)
				feedErr <- err
				return
			}
			switch typ {
			case tunnel.FrameFileData:
				n += int64(len(p))
				if n > tunnel.MaxUpload {
					err := fmt.Errorf("the upload is larger than %d bytes", tunnel.MaxUpload)
					_ = pw.CloseWithError(err)
					feedErr <- err
					return
				}
				h.Write(p)
				if _, err := pw.Write(p); err != nil {
					// The helper stopped reading; keep draining to the end
					// so the stream stays in step.
					continue
				}
			case tunnel.FrameFileEnd:
				_ = pw.Close()
				feedErr <- nil
				return
			default:
				err := errors.New("unexpected frame during an upload")
				_ = pw.CloseWithError(err)
				feedErr <- err
				return
			}
		}
	}()
	args := []string{"upload", req.Path}
	if req.Overwrite {
		args = append(args, "-f")
	}
	runErr := f.run(ctx, args, pr, io.Discard)
	_ = pr.Close()
	if ferr := <-feedErr; ferr != nil {
		// The stream itself failed or broke the protocol: end it.
		_ = answer(st, tunnel.FileAnswer{Error: ferr.Error(), Path: path})
		return ferr
	}
	if runErr != nil {
		return answer(st, tunnel.FileAnswer{Error: runErr.Error(), Path: path})
	}
	f.log.Info("tunnel: file uploaded", "path", path, "bytes", n,
		"principal", f.acct.name, "for", f.actor)
	return answer(st, tunnel.FileAnswer{OK: true, Path: path, Size: n,
		SHA256: hex.EncodeToString(h.Sum(nil))})
}

// resolve is the path the helper will use, for the answer; the helper
// resolves it again for itself, as the account.
func (f *fileSession) resolve(p string) (string, error) {
	home := f.acct.home
	switch {
	case p == "" || p == "~":
		return home, nil
	case strings.HasPrefix(p, "~/"):
		return home + "/" + p[2:], nil
	case strings.HasPrefix(p, "/"):
		return p, nil
	}
	return home + "/" + p, nil
}

// run starts one file-op as the account and waits for it.
func (f *fileSession) run(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	return runAs(ctx, helperRun{acct: f.acct, helper: f.cfg.Helper, spawn: f.cfg.Spawn,
		env: f.cfg.HelperEnv, args: append([]string{"file-op"}, args...), stdin: stdin,
		stdout: stdout, actor: f.actor, home: true})
}

// helperRun is one run of this executable as another account.
type helperRun struct {
	acct   loginAccount
	helper string
	spawn  SpawnMode
	env    []string
	args   []string
	stdin  io.Reader
	stdout io.Writer
	actor  string
	// home runs it from the account's home directory.
	home bool
}

// runAs starts this executable (or h.helper) as h.acct and waits for it: in a
// transient unit on a systemd host, because the agent's own sandbox would be
// inherited otherwise, and as a direct child with the account's credentials
// elsewhere.
func runAs(ctx context.Context, h helperRun) error {
	helper := h.helper
	if helper == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		helper = exe
	}
	mode := h.spawn
	if mode == SpawnAuto {
		mode = SpawnDirect
		if os.Geteuid() == 0 && systemdRunning() {
			mode = SpawnSystemd
		}
	}
	var cmd *exec.Cmd
	if mode == SpawnSystemd {
		// --pipe hands the unit our stdin and stdout; the unit runs outside
		// the agent's sandbox, as the account.
		sargs := []string{"--pipe", "--wait", "--quiet", "--collect",
			"--service-type=exec", "--uid=" + h.acct.name,
			"--setenv=ARGUS_SESSION_FOR=" + h.actor}
		if h.home {
			sargs = append(sargs, "-p", "WorkingDirectory=~")
		}
		for _, e := range h.env {
			sargs = append(sargs, "--setenv="+e)
		}
		cmd = exec.CommandContext(ctx, "systemd-run", append(append(sargs, helper), h.args...)...)
	} else {
		cmd = exec.CommandContext(ctx, helper, h.args...)
		cmd.Dir = "/"
		if h.home {
			cmd.Dir = h.acct.home
		}
		cmd.Env = append([]string{
			"HOME=" + h.acct.home, "USER=" + h.acct.name, "LOGNAME=" + h.acct.name,
			"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
			"ARGUS_SESSION_FOR=" + h.actor,
		}, h.env...)
		if h.acct.uid != os.Geteuid() {
			cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
				Uid: uint32(h.acct.uid), Gid: uint32(h.acct.gid), Groups: h.acct.groups}}
		}
	}
	var stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = h.stdin, h.stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return errors.New(lastLine(msg))
		}
		return fmt.Errorf("the helper failed: %w", err)
	}
	return nil
}

func lastLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// frameWriter sends what it is written as data frames, counting and
// hashing it on the way.
type frameWriter struct {
	w   io.Writer
	typ byte
	h   hash.Hash
	n   int64
	err error
}

func (w *frameWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	total := len(p)
	for len(p) > 0 {
		c := min(len(p), tunnel.FileChunk)
		if err := tunnel.WriteFrame(w.w, w.typ, p[:c]); err != nil {
			w.err = err
			return total - len(p), err
		}
		_, _ = w.h.Write(p[:c])
		w.n += int64(c)
		p = p[c:]
	}
	return total, nil
}
