// Package cfapi reads a Cloudflare account's tunnels and private networking
// with a read-only API token: Cloudflare Tunnels and their connectors, WARP
// Connectors, private network routes, WARP-enrolled devices and Magic WAN
// tunnels.
//
// Read only. Nothing here creates, changes or deletes anything in Cloudflare,
// and the token is asked for read permissions only (Cloudflare Tunnel Read,
// Zero Trust Read, Magic WAN Read). Only the tunnel list is required; each
// other list is read when the token allows it and left out when it does not,
// so a token with fewer permissions still shows the tunnels.
package cfapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// DefaultBase is Cloudflare's API.
const DefaultBase = "https://api.cloudflare.com/client/v4"

const (
	perPage  = 100
	maxPages = 10
	// maxBody bounds one response.
	maxBody = 8 << 20
	// MaxDevices bounds the devices kept; the total is kept as well.
	MaxDevices = 500
)

// The lists a snapshot can hold.
const (
	ListTunnels    = "tunnels"
	ListConnectors = "warpConnectors"
	ListRoutes     = "routes"
	ListDevices    = "devices"
	ListMagic      = "magicWan"
)

// accountID is Cloudflare's account id: 32 hex digits.
var accountID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ValidAccountID reports whether s is a Cloudflare account id.
func ValidAccountID(s string) bool { return accountID.MatchString(s) }

// Snapshot is one read of an account.
type Snapshot struct {
	ReadAt time.Time `json:"readAt"`
	// Read are the lists that were read; a list missing from it was not
	// allowed by the token, and is not known rather than empty.
	Read         []string          `json:"read"`
	Errors       map[string]string `json:"errors,omitempty"`
	Tunnels      []Tunnel          `json:"tunnels"`
	Connectors   []Tunnel          `json:"warpConnectors"`
	Routes       []Route           `json:"routes"`
	Devices      []Device          `json:"devices"`
	DevicesTotal int               `json:"devicesTotal"`
	Magic        []MagicTunnel     `json:"magicWan"`
}

// Tunnel is a Cloudflare Tunnel or a WARP Connector.
type Tunnel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Status is Cloudflare's: healthy, degraded, down or inactive (never
	// run, or no connector for a long time).
	Status          string       `json:"status"`
	CreatedAt       *time.Time   `json:"created_at,omitempty"`
	ConnsActiveAt   *time.Time   `json:"conns_active_at,omitempty"`
	ConnsInactiveAt *time.Time   `json:"conns_inactive_at,omitempty"`
	RemoteConfig    bool         `json:"remote_config"`
	Connections     []Connection `json:"connections"`
}

// Connection is one connection of a connector (a running cloudflared) to a
// Cloudflare location. A connector normally holds four, to two locations.
type Connection struct {
	// ClientID is the connector's id, which cloudflared's /ready also
	// reports, so a connection is matched to the server running it.
	ClientID      string     `json:"client_id"`
	ClientVersion string     `json:"client_version"`
	ColoName      string     `json:"colo_name"`
	OriginIP      string     `json:"origin_ip"`
	OpenedAt      *time.Time `json:"opened_at,omitempty"`
	Pending       bool       `json:"is_pending_reconnect"`
}

// Route is a private network reached through a tunnel.
type Route struct {
	Network    string `json:"network"`
	TunnelID   string `json:"tunnel_id"`
	TunnelName string `json:"tunnel_name"`
	Comment    string `json:"comment"`
}

// Device is a device enrolled in WARP.
type Device struct {
	Name     string     `json:"name"`
	User     string     `json:"user,omitempty"`
	OS       string     `json:"os,omitempty"`
	Version  string     `json:"version,omitempty"`
	LastSeen *time.Time `json:"last_seen,omitempty"`
}

// MagicTunnel is a Magic WAN IPsec or GRE tunnel.
type MagicTunnel struct {
	Kind               string `json:"kind"` // ipsec, gre
	Name               string `json:"name"`
	CustomerEndpoint   string `json:"customerEndpoint"`
	CloudflareEndpoint string `json:"cloudflareEndpoint"`
	InterfaceAddress   string `json:"interfaceAddress"`
}

// Client reads accounts.
type Client struct {
	Base string
	HTTP *http.Client
}

// ErrToken is a token Cloudflare refused, or that cannot read tunnels.
var ErrToken = errors.New("Cloudflare refused the token")

// envelope is every Cloudflare response.
type envelope struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
	Result     json.RawMessage `json:"result"`
	ResultInfo *struct {
		Page       int `json:"page"`
		TotalPages int `json:"total_pages"`
		TotalCount int `json:"total_count"`
	} `json:"result_info"`
}

type apiError struct {
	status int
	msg    string
}

func (e apiError) Error() string { return e.msg }

func (c *Client) get(ctx context.Context, token, u string) (envelope, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return envelope{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return envelope{}, fmt.Errorf("cannot reach Cloudflare: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return envelope{}, err
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return envelope{}, apiError{resp.StatusCode, fmt.Sprintf("Cloudflare answered %d with something other than JSON", resp.StatusCode)}
	}
	if resp.StatusCode != http.StatusOK || !env.Success {
		msg := fmt.Sprintf("Cloudflare answered %d", resp.StatusCode)
		if len(env.Errors) > 0 {
			msg += ": " + env.Errors[0].Message
		}
		return envelope{}, apiError{resp.StatusCode, msg}
	}
	return env, nil
}

// pages reads every page of a list into out, a pointer to a slice, up to
// maxPages. total is the list's own count when it gives one.
func pages[T any](ctx context.Context, c *Client, token, u string) ([]T, int, error) {
	var all []T
	total := 0
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	for page := 1; page <= maxPages; page++ {
		env, err := c.get(ctx, token, fmt.Sprintf("%s%sper_page=%d&page=%d", u, sep, perPage, page))
		if err != nil {
			return nil, 0, err
		}
		var got []T
		if len(env.Result) > 0 && string(env.Result) != "null" {
			if err := json.Unmarshal(env.Result, &got); err != nil {
				return nil, 0, fmt.Errorf("cannot read Cloudflare's answer: %w", err)
			}
		}
		all = append(all, got...)
		if env.ResultInfo == nil {
			return all, len(all), nil
		}
		total = env.ResultInfo.TotalCount
		if env.ResultInfo.TotalPages <= page || len(got) == 0 {
			break
		}
	}
	if total < len(all) {
		total = len(all)
	}
	return all, total, nil
}

// Read reads the account. Only the tunnel list must succeed; the others are
// read when the token allows.
func (c *Client) Read(ctx context.Context, account, token string, now time.Time) (Snapshot, error) {
	if !ValidAccountID(account) {
		return Snapshot{}, errors.New("a Cloudflare account id is 32 hexadecimal characters")
	}
	base := strings.TrimRight(c.Base, "/")
	if base == "" {
		base = DefaultBase
	}
	acct := base + "/accounts/" + url.PathEscape(account)
	snap := Snapshot{ReadAt: now.UTC(), Errors: map[string]string{}, Tunnels: []Tunnel{}, Connectors: []Tunnel{},
		Routes: []Route{}, Devices: []Device{}, Magic: []MagicTunnel{}}

	var mu sync.Mutex
	var wg sync.WaitGroup
	var tunnelsErr error
	note := func(list string, err error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			snap.Errors[list] = err.Error()
			return
		}
		snap.Read = append(snap.Read, list)
	}
	run := func(f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}
	run(func() {
		ts, _, err := pages[Tunnel](ctx, c, token, acct+"/cfd_tunnel?is_deleted=false")
		if err != nil {
			tunnelsErr = err
			return
		}
		snap.Tunnels = bound(ts)
		note(ListTunnels, nil)
	})
	run(func() {
		ts, _, err := pages[Tunnel](ctx, c, token, acct+"/warp_connector?is_deleted=false")
		if err == nil {
			snap.Connectors = bound(ts)
		}
		note(ListConnectors, err)
	})
	run(func() {
		rs, _, err := pages[Route](ctx, c, token, acct+"/teamnet/routes?is_deleted=false")
		if err == nil {
			if len(rs) > 1000 {
				rs = rs[:1000]
			}
			for i := range rs {
				rs[i].Comment = cut(rs[i].Comment, 200)
			}
			snap.Routes = rs
		}
		note(ListRoutes, err)
	})
	run(func() {
		type rawDevice struct {
			Name      string     `json:"name"`
			OSVersion string     `json:"os_version"`
			Type      string     `json:"device_type"`
			Version   string     `json:"version"`
			LastSeen  *time.Time `json:"last_seen"`
			User      struct {
				Email string `json:"email"`
			} `json:"user"`
		}
		ds, total, err := pages[rawDevice](ctx, c, token, acct+"/devices")
		if err == nil {
			for i, d := range ds {
				if i == MaxDevices {
					break
				}
				snap.Devices = append(snap.Devices, Device{Name: cut(d.Name, 120), User: cut(d.User.Email, 200),
					OS: cut(strings.TrimSpace(d.Type+" "+d.OSVersion), 80), Version: cut(d.Version, 40), LastSeen: d.LastSeen})
			}
			snap.DevicesTotal = total
		}
		note(ListDevices, err)
	})
	run(func() {
		var magic []MagicTunnel
		var errs []error
		for _, kind := range []string{"ipsec", "gre"} {
			env, err := c.get(ctx, token, acct+"/magic/"+kind+"_tunnels")
			if err != nil {
				errs = append(errs, err)
				continue
			}
			var res map[string][]struct {
				Name               string `json:"name"`
				CustomerEndpoint   string `json:"customer_endpoint"`
				CustomerGRE        string `json:"customer_gre_endpoint"`
				CloudflareEndpoint string `json:"cloudflare_endpoint"`
				CloudflareGRE      string `json:"cloudflare_gre_endpoint"`
				InterfaceAddress   string `json:"interface_address"`
			}
			if json.Unmarshal(env.Result, &res) != nil {
				continue
			}
			for _, t := range res[kind+"_tunnels"] {
				m := MagicTunnel{Kind: kind, Name: cut(t.Name, 120), CustomerEndpoint: t.CustomerEndpoint,
					CloudflareEndpoint: t.CloudflareEndpoint, InterfaceAddress: t.InterfaceAddress}
				if kind == "gre" {
					m.CustomerEndpoint, m.CloudflareEndpoint = t.CustomerGRE, t.CloudflareGRE
				}
				magic = append(magic, m)
			}
		}
		// An account without Magic WAN answers both with an error; one
		// kind answering is enough to say the list was read.
		var err error
		if len(errs) == 2 {
			err = errs[0]
		}
		if err == nil {
			if len(magic) > 200 {
				magic = magic[:200]
			}
			snap.Magic = magic
		}
		note(ListMagic, err)
	})
	wg.Wait()
	if tunnelsErr != nil {
		var ae apiError
		if errors.As(tunnelsErr, &ae) && (ae.status == http.StatusUnauthorized || ae.status == http.StatusForbidden) {
			return Snapshot{}, fmt.Errorf("%w: %s. It needs Cloudflare Tunnel Read on this account", ErrToken, ae.msg)
		}
		return Snapshot{}, tunnelsErr
	}
	return snap, nil
}

// bound trims a tunnel list to what a row holds.
func bound(ts []Tunnel) []Tunnel {
	if len(ts) > 500 {
		ts = ts[:500]
	}
	for i := range ts {
		ts[i].Name = cut(ts[i].Name, 120)
		if len(ts[i].Connections) > 32 {
			ts[i].Connections = ts[i].Connections[:32]
		}
		if ts[i].Connections == nil {
			ts[i].Connections = []Connection{}
		}
	}
	return ts
}

func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n]
}
