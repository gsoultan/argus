package reporter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/gsoultan/argus/internal/recorder"
)

// UploadError is a recording the control plane would not take, with its
// status, so the caller can tell "later" from "never".
type UploadError struct {
	Status  int
	Message string
}

func (e *UploadError) Error() string {
	return fmt.Sprintf("control plane returned %d: %s", e.Status, e.Message)
}

// Retry reports whether sending the same file again could succeed: the
// session's report has not landed yet, storage is down, or the control plane
// is busy. Anything else is a refusal of these bytes.
func (e *UploadError) Retry() bool {
	switch e.Status {
	case http.StatusConflict, http.StatusTooManyRequests, http.StatusServiceUnavailable,
		http.StatusBadGateway, http.StatusGatewayTimeout, http.StatusInternalServerError:
		return true
	}
	return false
}

// UploadRecording sends a sealed recording to the control plane, which
// verifies it against the chain head it was told and stores it.
//
// Not spooled like a report: the recording on disk is its own spool, and the
// caller keeps it queued until this succeeds. No overall timeout either, only
// ctx's: a desktop session's recording can take minutes on a slow link, and a
// five-second cut-off would make it impossible to ever deliver.
func (c *Client) UploadRecording(ctx context.Context, id, chainHead, path string) error {
	if !c.Enabled() {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	var body io.Reader = f
	size := info.Size()
	compressed := strings.HasSuffix(path, recorder.CompressedExt)
	ship := compressed && c.shipZstd.Load()
	if ship && !c.shipPacked.Load() {
		// zstd, but packed inside, to a control plane that reads zstd and
		// not that: it goes plain, as to one that reads no zstd at all.
		packed, err := recorder.IsPacked(f)
		if err != nil {
			return err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		ship = !packed
	}
	if compressed && !ship {
		// Kept compressed on this host, sent plain to a control plane that
		// has not said it reads zstd. The length is unknown, so it is sent
		// chunked.
		plain, err := recorder.Open(f)
		if err != nil {
			return err
		}
		defer plain.Close()
		body, size, compressed = plain, -1, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.baseURL+"/api/v1/report/recording/"+url.PathEscape(id), body)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Argus-Chain-Head", chainHead)
	req.Header.Set("X-Argus-Format",
		strings.TrimPrefix(extOf(strings.TrimSuffix(path, recorder.CompressedExt)), "."))
	if compressed {
		req.Header.Set("X-Argus-Encoding", "zstd")
	}

	hc := *c.http
	hc.Timeout = 0
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(msg, &e) == nil && e.Error != "" {
			msg = []byte(e.Error)
		}
		return &UploadError{Status: resp.StatusCode, Message: strings.TrimSpace(string(msg))}
	}
	return nil
}

// SetShipZstd records whether the control plane reads zstd recordings, from
// its heartbeat reply.
func (c *Client) SetShipZstd(ok bool) { c.shipZstd.Store(ok) }

// SetShipPacked records whether the control plane reads packed zstd
// recordings, from its heartbeat reply.
func (c *Client) SetShipPacked(ok bool) { c.shipPacked.Store(ok) }

func extOf(path string) string {
	if i := strings.LastIndexByte(path, '.'); i >= 0 && !strings.ContainsAny(path[i:], `/\`) {
		return path[i:]
	}
	return ""
}

// Download fetches path from the control plane into dst on this agent's own
// credential, at most limit bytes, and returns how many it wrote. A library
// file an install was told to fetch comes this way; the caller checks its
// hash before using it. No overall timeout, only ctx's, like an upload.
func (c *Client) Download(ctx context.Context, path string, dst io.Writer, limit int64) (int64, error) {
	if !c.Enabled() {
		return 0, errors.New("no control plane is configured")
	}
	if !strings.HasPrefix(path, "/api/v1/report/") {
		return 0, fmt.Errorf("refusing to fetch %q: not a report path", path)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	hc := *c.http
	hc.Timeout = 0
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(msg, &e) == nil && e.Error != "" {
			msg = []byte(e.Error)
		}
		return 0, &UploadError{Status: resp.StatusCode, Message: strings.TrimSpace(string(msg))}
	}
	n, err := io.Copy(dst, io.LimitReader(resp.Body, limit+1))
	if err == nil && n > limit {
		err = fmt.Errorf("the file is larger than the %d bytes expected", limit)
	}
	return n, err
}
