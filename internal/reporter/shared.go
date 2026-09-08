package reporter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Shared state that must be consistent across every gateway.
//
// These calls are synchronous and blocking, unlike the fire-and-forget
// reporting in this package. That difference is deliberate: reporting a session
// late is a cosmetic problem, whereas guessing at whether a ticket was already
// used is an authentication decision. If the control plane cannot answer, the
// caller must refuse rather than assume.

// RedeemTicket burns a ticket centrally and reports whether it was already
// used. An error means "cannot tell", which callers must treat as a refusal.
func (c *Client) RedeemTicket(ctx context.Context, id, email, target, principal string,
	expiresAt time.Time) (alreadyUsed bool, err error) {

	if !c.Enabled() {
		return false, fmt.Errorf("no control plane configured")
	}
	var out struct {
		AlreadyUsed bool `json:"alreadyUsed"`
	}
	if err := c.call(ctx, http.MethodPost, "/api/v1/terminal/redeem", map[string]any{
		"id": id, "email": email, "target": target,
		"principal": principal, "expiresAt": expiresAt,
	}, &out); err != nil {
		return false, err
	}
	return out.AlreadyUsed, nil
}

// HostKeyPin is a target's recorded identity.
type HostKeyPin struct {
	Host        string    `json:"host"`
	Fingerprint string    `json:"fingerprint"`
	KeyType     string    `json:"keyType"`
	PinnedAt    time.Time `json:"pinnedAt"`
	PinnedBy    string    `json:"pinnedBy"`
}

// HostKeyPin fetches the pin for host. A nil pin with no error means the host
// has never been pinned.
func (c *Client) HostKeyPin(ctx context.Context, host string) (*HostKeyPin, error) {
	if !c.Enabled() {
		return nil, fmt.Errorf("no control plane configured")
	}
	var out struct {
		Pinned bool        `json:"pinned"`
		Pin    *HostKeyPin `json:"pin"`
	}
	if err := c.call(ctx, http.MethodGet,
		"/api/v1/hostkeys/pin?host="+host, nil, &out); err != nil {
		return nil, err
	}
	if !out.Pinned {
		return nil, nil
	}
	return out.Pin, nil
}

// PinHostKey records a first-contact pin. A conflict means another gateway
// already pinned a different key, which is a mismatch and not something to
// resolve automatically.
func (c *Client) PinHostKey(ctx context.Context, p HostKeyPin) error {
	if !c.Enabled() {
		return fmt.Errorf("no control plane configured")
	}
	return c.call(ctx, http.MethodPost, "/api/v1/hostkeys/pin", p, nil)
}

// call performs a synchronous request against the control plane.
func (c *Client) call(ctx context.Context, method, path string, body, out any) error {
	var reader *bytes.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	} else {
		reader = bytes.NewReader(nil)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		return ErrConflict
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("control plane returned %d for %s", resp.StatusCode, path)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// ErrConflict means the control plane refused because the state already exists
// and differs — a host key that does not match its pin, for example.
var ErrConflict = fmt.Errorf("conflicts with recorded state")

// GatewayPolicy is what a brokered session may do, as the control plane holds
// it. Field names match internal/gateway.Policy and the console's own type.
type GatewayPolicy struct {
	AllowLocalForward  bool `json:"allowLocalForward"`
	AllowRemoteForward bool `json:"allowRemoteForward"`
	AllowAgentForward  bool `json:"allowAgentForward"`
	AllowX11Forward    bool `json:"allowX11Forward"`

	ProxySftpSubsystem        bool `json:"proxySftpSubsystem"`
	FailClosedOnRecordingLoss bool `json:"failClosedOnRecordingLoss"`
	RequireEbpfForRoot        bool `json:"requireEbpfForRoot"`

	// ElevatedPrincipals need an approved access request. Defined by the
	// control plane so the two services cannot disagree about what "elevated"
	// means; an older gateway that does not know the field falls back to its
	// own default rather than to an empty list.
	ElevatedPrincipals []string `json:"elevatedPrincipals"`
}

// GatewayPolicy fetches the policy in force.
//
// An error is returned rather than a default, and the caller keeps whatever it
// already had. That is the safe direction: handing back a zero-valued policy on
// a network blip would switch off SFTP decoding and fail-closed recording — a
// transient failure would quietly reduce what gets recorded.
func (c *Client) GatewayPolicy(ctx context.Context) (*GatewayPolicy, error) {
	if !c.Enabled() {
		return nil, fmt.Errorf("no control plane configured")
	}
	var out GatewayPolicy
	if err := c.call(ctx, http.MethodGet, "/api/v1/gateway/policy", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Authorization is the control plane's answer about one session.
type Authorization struct {
	Allowed   bool       `json:"allowed"`
	Reason    string     `json:"reason,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// Authorize asks whether a person may open a session as a principal on a host.
//
// An error means the question could not be answered, which is not the same as
// "no" and must not be treated as "yes" either. The gateway refuses elevated
// sessions on an error, because a control plane that cannot be reached is
// exactly when someone would like root without an approval on file.
func (c *Client) Authorize(ctx context.Context, email, target, principal string) (*Authorization, error) {
	q := url.Values{}
	q.Set("email", email)
	q.Set("target", target)
	q.Set("principal", principal)

	var out Authorization
	if err := c.call(ctx, http.MethodGet,
		"/api/v1/report/authorize?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
