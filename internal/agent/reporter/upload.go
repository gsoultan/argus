package reporter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.baseURL+"/api/v1/report/recording/"+url.PathEscape(id), f)
	if err != nil {
		return err
	}
	req.ContentLength = info.Size()
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Argus-Chain-Head", chainHead)
	req.Header.Set("X-Argus-Format", strings.TrimPrefix(extOf(path), "."))

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

func extOf(path string) string {
	if i := strings.LastIndexByte(path, '.'); i >= 0 && !strings.ContainsAny(path[i:], `/\`) {
		return path[i:]
	}
	return ""
}
