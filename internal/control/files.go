package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/gsoultan/argus/internal/tunnel"
	"github.com/gsoultan/argus/internal/wskeep"
)

/*
File transfer in the browser, through an agent's tunnel: the console's scp.

	browser ──wss──▶ argus-control ──"files" stream──▶ agent ──▶ file-op, as the account

Opened with the same single-use ticket as a terminal, minted by the same
checks: whoever may open a shell as an account may move files as it, and an
elevated account still needs its approval first. The grant the agent is
handed is for "files", so it cannot be spent on a shell.

Every transfer is written into the audit chain with the person, the host, the
account, the path, the size and the SHA-256 of the bytes that crossed -- the
control plane carries them, so it hashes them itself rather than taking the
agent's word, and says so when the two disagree.

To the browser, JSON control messages and binary file data:

	→ {type:list, path}                ← {type:listing, path, entries, truncated}
	→ {type:download, path}            ← {type:download-start, path}, binary…, {type:download-end, size, sha256}
	→ {type:upload, path, overwrite}   ← {type:upload-ready, path}; → binary…, {type:upload-end}
	                                   ← {type:upload-done, path, size, sha256}
	                                   ← {type:error, data} ends only that operation
*/

type filesMessage struct {
	Type      string             `json:"type"`
	Path      string             `json:"path,omitempty"`
	Overwrite bool               `json:"overwrite,omitempty"`
	Data      string             `json:"data,omitempty"`
	Entries   []tunnel.FileEntry `json:"entries,omitempty"`
	Truncated bool               `json:"truncated,omitempty"`
	Size      int64              `json:"size,omitempty"`
	SHA256    string             `json:"sha256,omitempty"`
}

// handleFiles serves GET /api/v1/files.
func (a *API) handleFiles(w http.ResponseWriter, r *http.Request) {
	target, principal := r.URL.Query().Get("target"), r.URL.Query().Get("principal")
	if target == "" || principal == "" {
		writeErr(w, http.StatusBadRequest, "target and principal are required")
		return
	}
	if a.signer == nil || a.Tunnels == nil || a.Tunnels.GrantKey == nil {
		writeErr(w, http.StatusServiceUnavailable,
			"this control plane cannot transfer files through agents: set signing_secret")
		return
	}
	token := r.URL.Query().Get("ticket")
	if token == "" {
		writeErr(w, http.StatusUnauthorized, "no ticket supplied")
		return
	}
	t, err := a.signer.RedeemTicket(r.Context(), token, target, principal)
	if err != nil {
		a.log.Warn("file transfer refused", "target", target, "principal", principal, "error", err)
		writeErr(w, http.StatusUnauthorized, "unauthorized: "+err.Error())
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: originPatterns(a.AllowedOrigins),
	})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	// A data message is one chunk; the browser sends tunnel.FileChunk.
	conn.SetReadLimit(int64(tunnel.FileChunk) + 4096)

	a.relayFiles(r.Context(), conn, t.Email, target, principal, a.clientAddr(r))
}

func (a *API) relayFiles(ctx context.Context, conn *websocket.Conn,
	email, host, principal, client string) {

	var wmu sync.Mutex
	send := func(typ websocket.MessageType, b []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return conn.Write(wctx, typ, b)
	}
	sendJSON := func(m filesMessage) error {
		b, _ := json.Marshal(m)
		return send(websocket.MessageText, b)
	}
	fail := func(why string) {
		_ = sendJSON(filesMessage{Type: "error", Data: why})
		_ = conn.Close(websocket.StatusNormalClosure, "refused")
	}

	g := tunnel.NewGrant(email, host, principal, client)
	g.Service = agentFilesService
	grant, err := tunnel.SignGrant(a.Tunnels.GrantKey, g)
	if err != nil {
		fail("could not sign the transfer grant")
		return
	}
	octx, cancel := context.WithTimeout(ctx, terminalOpenWait)
	st, answer, err := a.Tunnels.OpenWith(octx, host,
		tunnel.Header{Service: agentFilesService, Grant: grant})
	cancel()
	switch {
	case errors.Is(err, ErrNoTunnel):
		fail("the agent on " + host + " is not connected to this control plane, so files " +
			"cannot move through it; check the agent is running and shows as connected " +
			"under Servers")
		return
	case tunnel.IsRefused(err):
		a.log.Warn("files: the agent refused", "host", host, "principal", principal,
			"email", email, "error", err)
		fail("the agent on " + host + " refused file transfer: " + answer.Error)
		return
	case err != nil:
		fail("could not reach the agent on " + host + ": " + err.Error())
		return
	}
	defer st.Close()

	a.auditFiles(ctx, "files.opened", "notice", email, host,
		"File transfer opened as "+principal+" through the agent's tunnel.")
	if sendJSON(filesMessage{Type: "ready"}) != nil {
		return
	}
	go wskeep.Run(ctx, conn)

	fr := tunnel.NewFrameReader(st)
	agentAnswer := func() (tunnel.FileAnswer, error) {
		for {
			typ, p, err := fr.Next()
			if err != nil {
				return tunnel.FileAnswer{}, err
			}
			if typ != tunnel.FrameFileAnswer {
				return tunnel.FileAnswer{}, fmt.Errorf("unexpected frame %q from the agent", typ)
			}
			var ans tunnel.FileAnswer
			if err := json.Unmarshal(p, &ans); err != nil {
				return tunnel.FileAnswer{}, err
			}
			return ans, nil
		}
	}
	request := func(req tunnel.FileRequest) error {
		b, _ := json.Marshal(req)
		return tunnel.WriteFrame(st, tunnel.FrameFileRequest, b)
	}
	agentGone := func() {
		_ = sendJSON(filesMessage{Type: "error",
			Data: "the connection to the agent ended; open the file browser again"})
		_ = conn.Close(websocket.StatusGoingAway, "agent gone")
	}

	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			// File data with no upload open.
			_ = sendJSON(filesMessage{Type: "error", Data: "file data arrived with no upload open"})
			continue
		}
		var m filesMessage
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.Type {
		case "list":
			if request(tunnel.FileRequest{Op: "list", Path: m.Path}) != nil {
				agentGone()
				return
			}
			ans, err := agentAnswer()
			if err != nil {
				agentGone()
				return
			}
			if !ans.OK {
				_ = sendJSON(filesMessage{Type: "error", Path: ans.Path, Data: ans.Error})
				continue
			}
			_ = sendJSON(filesMessage{Type: "listing", Path: ans.Path, Entries: ans.Entries,
				Truncated: ans.Truncated})

		case "download":
			if !a.download(ctx, st, fr, request, agentAnswer, send, sendJSON, m, email, host, principal) {
				agentGone()
				return
			}

		case "upload":
			if !a.upload(ctx, conn, st, request, agentAnswer, sendJSON, m, email, host, principal) {
				agentGone()
				return
			}
		}
	}
}

// download relays one file to the browser. False means the stream is lost.
func (a *API) download(ctx context.Context, st tunnel.Stream, fr *tunnel.FrameReader,
	request func(tunnel.FileRequest) error, agentAnswer func() (tunnel.FileAnswer, error),
	send func(websocket.MessageType, []byte) error, sendJSON func(filesMessage) error,
	m filesMessage, email, host, principal string) bool {

	if request(tunnel.FileRequest{Op: "download", Path: m.Path}) != nil {
		return false
	}
	start, err := agentAnswer()
	if err != nil {
		return false
	}
	if !start.OK {
		_ = sendJSON(filesMessage{Type: "error", Path: start.Path, Data: start.Error})
		return true
	}
	_ = sendJSON(filesMessage{Type: "download-start", Path: start.Path})
	h := sha256.New()
	var n int64
	for {
		typ, p, err := fr.Next()
		if err != nil {
			return false
		}
		if typ == tunnel.FrameFileData {
			h.Write(p)
			n += int64(len(p))
			// The browser going away mid-file must not desynchronise the
			// stream: keep reading to the agent's end answer.
			_ = send(websocket.MessageBinary, p)
			continue
		}
		if typ != tunnel.FrameFileAnswer {
			return false
		}
		var end tunnel.FileAnswer
		if json.Unmarshal(p, &end) != nil {
			return false
		}
		sum := hex.EncodeToString(h.Sum(nil))
		if !end.OK {
			a.auditFiles(ctx, "file.download", "warning", email, host, fmt.Sprintf(
				"Download of %s as %s stopped after %d bytes: %s", start.Path, principal, n, end.Error))
			_ = sendJSON(filesMessage{Type: "error", Path: start.Path, Data: end.Error})
			return true
		}
		detail := fmt.Sprintf("Downloaded %s from %s as %s through the console: %d bytes, sha256 %s.",
			start.Path, host, principal, n, sum)
		severity := "notice"
		if end.SHA256 != sum || end.Size != n {
			severity = "warning"
			detail += fmt.Sprintf(" The agent reported %d bytes, sha256 %s, which does not "+
				"match what crossed the control plane.", end.Size, end.SHA256)
		}
		a.auditFiles(ctx, "file.download", severity, email, host, detail)
		_ = sendJSON(filesMessage{Type: "download-end", Path: start.Path, Size: n, SHA256: sum})
		return true
	}
}

// upload relays one file from the browser. False means the stream is lost.
func (a *API) upload(ctx context.Context, conn *websocket.Conn, st tunnel.Stream,
	request func(tunnel.FileRequest) error, agentAnswer func() (tunnel.FileAnswer, error),
	sendJSON func(filesMessage) error, m filesMessage, email, host, principal string) bool {

	if request(tunnel.FileRequest{Op: "upload", Path: m.Path, Overwrite: m.Overwrite}) != nil {
		return false
	}
	ready, err := agentAnswer()
	if err != nil {
		return false
	}
	if !ready.OK {
		_ = sendJSON(filesMessage{Type: "error", Path: ready.Path, Data: ready.Error})
		return true
	}
	if sendJSON(filesMessage{Type: "upload-ready", Path: ready.Path}) != nil {
		return false
	}
	h := sha256.New()
	var n int64
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			// The browser left mid-file. The agent writes through a temporary
			// file, so ending the stream leaves the destination untouched.
			return false
		}
		if typ == websocket.MessageBinary {
			n += int64(len(data))
			if n > tunnel.MaxUpload {
				return false
			}
			h.Write(data)
			for len(data) > 0 {
				c := min(len(data), tunnel.FileChunk)
				if tunnel.WriteFrame(st, tunnel.FrameFileData, data[:c]) != nil {
					return false
				}
				data = data[c:]
			}
			continue
		}
		var end filesMessage
		if json.Unmarshal(data, &end) != nil || end.Type != "upload-end" {
			return false
		}
		break
	}
	if tunnel.WriteFrame(st, tunnel.FrameFileEnd, nil) != nil {
		return false
	}
	done, err := agentAnswer()
	if err != nil {
		return false
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if !done.OK {
		a.auditFiles(ctx, "file.upload", "warning", email, host, fmt.Sprintf(
			"Upload of %s as %s failed after %d bytes: %s", ready.Path, principal, n, done.Error))
		_ = sendJSON(filesMessage{Type: "error", Path: ready.Path, Data: done.Error})
		return true
	}
	detail := fmt.Sprintf("Uploaded %s to %s as %s through the console: %d bytes, sha256 %s.",
		ready.Path, host, principal, n, sum)
	if m.Overwrite {
		detail += " Replacing the file if it existed."
	}
	severity := "notice"
	if done.SHA256 != sum || done.Size != n {
		severity = "warning"
		detail += fmt.Sprintf(" The agent reported %d bytes, sha256 %s, which does not "+
			"match what crossed the control plane.", done.Size, done.SHA256)
	}
	a.auditFiles(ctx, "file.upload", severity, email, host, detail)
	_ = sendJSON(filesMessage{Type: "upload-done", Path: ready.Path, Size: n, SHA256: sum})
	return true
}

func (a *API) auditFiles(ctx context.Context, action, severity, email, host, detail string) {
	if _, err := a.store.AppendAudit(ctx, AuditEvent{
		Action: action, Severity: severity, ActorEmail: email, Target: host, Detail: detail,
	}); err != nil {
		a.log.Error("audit append failed", "error", err, "action", action)
	}
}

// agentFilesService is the agent's name for it; see agent.FilesService.
const agentFilesService = "files"
