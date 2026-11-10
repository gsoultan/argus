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
	"path/filepath"
	"strings"

	"github.com/gsoultan/argus/internal/tunnel"
)

// fileSession serves one files stream. Each operation is run by run: the
// agent's own "file-op", as the account, so the account's permissions decide
// what it may read and write.
type fileSession struct {
	// who is the account, for the log; home is its home directory.
	who, home string
	actor     string
	log       *slog.Logger
	run       func(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error
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
			opErr = f.jsonOp(ctx, st, []string{"list", req.Path})
		case "mkdir", "delete":
			opErr = f.jsonOp(ctx, st, []string{req.Op, req.Path})
		case "rename":
			opErr = f.jsonOp(ctx, st, []string{"rename", req.Path, req.To})
		case "preview":
			from := "start"
			if req.From == "end" {
				from = "end"
			}
			opErr = f.jsonOp(ctx, st, []string{"preview", req.Path, from})
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

// jsonOp runs one operation whose helper prints its whole answer as JSON --
// a listing, a new folder, a rename, a delete, a preview -- and passes it on.
func (f *fileSession) jsonOp(ctx context.Context, st tunnel.Stream, args []string) error {
	var out bytes.Buffer
	if err := f.run(ctx, args, nil, &out); err != nil {
		path, _ := f.resolve(args[1])
		return answer(st, tunnel.FileAnswer{Error: err.Error(), Path: path})
	}
	var ans tunnel.FileAnswer
	if json.Unmarshal(out.Bytes(), &ans) != nil {
		return answer(st, tunnel.FileAnswer{Error: "the agent could not read the answer to " + args[0]})
	}
	if args[0] != "list" && args[0] != "preview" {
		f.log.Info("tunnel: file "+args[0], "path", ans.Path, "to", ans.To,
			"principal", f.who, "for", f.actor)
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
		"principal", f.who, "for", f.actor)
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
				if err := writeUploadFrame(pw, p); err != nil {
					// The helper stopped reading; keep draining to the end
					// so the stream stays in step.
					continue
				}
			case tunnel.FrameFileEnd:
				// The closing frame is what tells the helper the file is
				// whole; a stream that breaks before it is never written.
				_ = writeUploadFrame(pw, nil)
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
	args := []string{"upload", req.Path, "-framed"}
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
		"principal", f.who, "for", f.actor)
	return answer(st, tunnel.FileAnswer{OK: true, Path: path, Size: n,
		SHA256: hex.EncodeToString(h.Sum(nil))})
}

// resolve is the path the helper will use, for the answer; the helper
// resolves it again for itself, as the account.
func (f *fileSession) resolve(p string) (string, error) {
	home := f.home
	if filepath.VolumeName(home) != "" {
		// Windows: C:\Users\ana, and paths with either slash.
		switch {
		case p == "" || p == "~":
			return home, nil
		case strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`):
			return filepath.Join(home, p[2:]), nil
		case filepath.VolumeName(p) != "" || strings.HasPrefix(p, `\`) || strings.HasPrefix(p, "/"):
			return p, nil
		}
		return filepath.Join(home, p), nil
	}
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
