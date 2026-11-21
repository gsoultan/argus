package tunnel

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"syscall"
	"time"
)

/*
The website check: does an address answer the way a visitor needs it to?

	argus-control ──"sitecheck" stream──▶ agent on a chosen server ──▶ GET url

argus-control checks every website itself. When a check fails there, it
asks the servers an administrator picked to look too, so a site is only
called down when places other than Argus agree: a problem between Argus
and the site, or on Argus's own network, is not the site being down.

Both sides run ProbeSite, so "failed" means the same thing wherever it was
seen. One request (a GET unless the check names another method, headers or
a body to send), a fresh connection each time (keep-alive would hide a server
that stopped accepting new ones), redirects followed up to five times, at
most a megabyte of the body read, and only to look for the keyword. Nothing
of the body leaves the machine that fetched it.
*/

// SiteCheckService is the tunnel service name.
const SiteCheckService = "sitecheck"

// FrameSiteResult carries the one SiteResult.
const FrameSiteResult byte = 'z'

const (
	// SiteTimeoutDefault is how long a check waits for an answer, unless the
	// check says otherwise. A slow answer is not a failure; none in this
	// long is.
	SiteTimeoutDefault = 30
	// SiteTimeoutMax bounds what a check may ask for.
	SiteTimeoutMax = 60
	// siteBodyMax is how much of a page is read to look for the keyword.
	siteBodyMax   = 1 << 20
	siteRedirects = 5
	// SiteKeywordMax bounds the word a check looks for.
	SiteKeywordMax = 200
	// SiteBodyMax bounds the body a check sends.
	SiteBodyMax = 64 << 10
	// SiteHeadersMax bounds how many headers a check sends.
	SiteHeadersMax = 20
	// SiteHeaderValueMax bounds one header's value.
	SiteHeaderValueMax = 4096
)

// The methods a check may send. GET is the default; the rest are for
// endpoints that only answer a request with something in it.
var SiteMethods = []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch}

// SiteHeader is one request header a check sends.
type SiteHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// siteHeaderReserved are headers the probe owns: the transport sets them, or
// they would change what the connection is rather than what is asked.
var siteHeaderReserved = map[string]bool{
	"Host": true, "Content-Length": true, "Transfer-Encoding": true, "Connection": true,
	"Upgrade": true, "Te": true, "Trailer": true, "Keep-Alive": true, "Expect": true,
	"Proxy-Authorization": true, "Proxy-Connection": true,
}

// ValidSiteHeaderName is a header name a check may send.
func ValidSiteHeaderName(name string) error {
	if name == "" {
		return errors.New("a header has no name")
	}
	if len(name) > 100 {
		return errors.New("a header name is longer than 100 characters")
	}
	for _, r := range name {
		if r > 126 || r <= 32 || strings.ContainsRune("()<>@,;:\\\"/[]?={}", r) {
			return fmt.Errorf("%q is not a header name: use letters, digits and dashes", name)
		}
	}
	if siteHeaderReserved[http.CanonicalHeaderKey(name)] {
		return fmt.Errorf("Argus sets %s itself", http.CanonicalHeaderKey(name))
	}
	return nil
}

// HasRequest is whether the check sends anything but a plain GET.
func (g GrantSite) HasRequest() bool {
	return (g.Method != "" && g.Method != http.MethodGet) || len(g.Headers) > 0 || g.Body != ""
}

func (g GrantSite) method() string {
	if g.Method == "" {
		return http.MethodGet
	}
	return g.Method
}

// What kind of failure a result is. The first three are the ones the
// checking machine's own network can cause, so they are the ones that ask
// whether it is the checker that is broken.
const (
	SiteFailDNS     = "dns"
	SiteFailConnect = "connect"
	SiteFailTimeout = "timeout"
	SiteFailTLS     = "tls"
	SiteFailStatus  = "status"
	SiteFailKeyword = "keyword"
	// SiteFailBlocked is an address no check may connect to.
	SiteFailBlocked = "blocked"
)

// NetworkFailure is whether kind is one the checker's own network could
// have caused.
func NetworkFailure(kind string) bool {
	return kind == SiteFailDNS || kind == SiteFailConnect || kind == SiteFailTimeout
}

// GrantSite is the one website a check grant may fetch.
type GrantSite struct {
	URL string `json:"url"`
	// Status is the status the page must answer with; 0 takes any below 400.
	Status int `json:"status,omitempty"`
	// Keyword, when set, must appear in the page.
	Keyword string `json:"keyword,omitempty"`
	// TimeoutSec is how long to wait; 0 is SiteTimeoutDefault.
	TimeoutSec int `json:"timeoutSec,omitempty"`
	// Method is the request's method; empty is GET.
	Method string `json:"method,omitempty"`
	// Headers are sent with the request, and only to the address's own host:
	// a redirect to another host gets none of them.
	Headers []SiteHeader `json:"headers,omitempty"`
	// Body is sent with a POST, PUT or PATCH.
	Body string `json:"body,omitempty"`
}

// ValidSiteURL is an http or https address with a host and no user name
// or password in it.
func ValidSiteURL(raw string) error {
	if strings.TrimSpace(raw) != raw || raw == "" {
		return errors.New("enter the address, starting with https://")
	}
	if len(raw) > 2000 {
		return errors.New("the address is too long")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("that is not a web address")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return errors.New("the address must start with https:// or http://")
	}
	if u.Hostname() == "" {
		return errors.New("the address has no host")
	}
	if u.User != nil {
		return errors.New("put no user name or password in the address")
	}
	if a, err := netip.ParseAddr(u.Hostname()); err == nil && blockedAddr(a) {
		return errors.New("that address cannot be checked: it is a link-local, multicast or unspecified address")
	}
	return nil
}

// Validate refuses a check that is not one fetch of one website.
func (g *GrantSite) Validate() error {
	if err := ValidSiteURL(g.URL); err != nil {
		return err
	}
	if g.Status != 0 && (g.Status < 100 || g.Status > 599) {
		return errors.New("the expected status must be between 100 and 599")
	}
	if len(g.Keyword) > SiteKeywordMax {
		return fmt.Errorf("the word to look for is longer than %d characters", SiteKeywordMax)
	}
	if g.TimeoutSec < 0 || g.TimeoutSec > SiteTimeoutMax {
		return fmt.Errorf("the timeout must be at most %d seconds", SiteTimeoutMax)
	}
	if g.Method != "" && !slices.Contains(SiteMethods, g.Method) {
		return errors.New("the method must be GET, HEAD, POST, PUT or PATCH")
	}
	m := g.method()
	if m == http.MethodHead && g.Keyword != "" {
		return errors.New("a HEAD request has no page to look for a word in: choose GET or POST, or clear the word")
	}
	if g.Body != "" && m != http.MethodPost && m != http.MethodPut && m != http.MethodPatch {
		return errors.New("only a POST, PUT or PATCH request sends a body")
	}
	if len(g.Body) > SiteBodyMax {
		return fmt.Errorf("the body is longer than %d KB", SiteBodyMax>>10)
	}
	if len(g.Headers) > SiteHeadersMax {
		return fmt.Errorf("send at most %d headers", SiteHeadersMax)
	}
	for _, h := range g.Headers {
		if err := ValidSiteHeaderName(h.Name); err != nil {
			return err
		}
		if len(h.Value) > SiteHeaderValueMax {
			return fmt.Errorf("the value of %s is longer than %d characters", h.Name, SiteHeaderValueMax)
		}
		if strings.ContainsAny(h.Value, "\r\n\x00") {
			return fmt.Errorf("the value of %s has a line break in it", h.Name)
		}
	}
	return nil
}

func (g GrantSite) timeout() time.Duration {
	if g.TimeoutSec <= 0 {
		return SiteTimeoutDefault * time.Second
	}
	return time.Duration(g.TimeoutSec) * time.Second
}

// SiteResult is how one fetch went.
type SiteResult struct {
	OK bool `json:"ok"`
	// Status is the final answer's status, after redirects; 0 for none.
	Status int `json:"status,omitempty"`
	// Fail is what kind of failure it was (SiteFail*); empty when OK.
	Fail string `json:"fail,omitempty"`
	// Detail says what a visitor would have seen, in words.
	Detail string `json:"detail,omitempty"`
	MS     int64  `json:"ms"`
	// CertNotAfter is when the site's certificate ends, for https.
	CertNotAfter *time.Time `json:"certNotAfter,omitempty"`
	From         string     `json:"from,omitempty"`
	// Method is the method this probe sent. An agent from before checks could
	// send anything but a GET leaves it out, which is how argus-control knows
	// not to count its answer for a check that sends more.
	Method string `json:"method,omitempty"`
}

// blockedAddr is an address a website check never connects to: link-local
// covers the cloud metadata services (169.254.169.254, fd00:ec2::254 is
// unique-local and allowed, as any private site is), whose answers are
// credentials, not websites.
func blockedAddr(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsMulticast() ||
		a.IsUnspecified() || a.IsInterfaceLocalMulticast()
}

// siteProxy is the environment's proxy, except for a blocked address
// literal, which a proxy would otherwise fetch on the check's behalf (a
// redirect can name one). A name the proxy resolves is the proxy's to
// guard.
func siteProxy(req *http.Request) (*url.URL, error) {
	if a, err := netip.ParseAddr(req.URL.Hostname()); err == nil && blockedAddr(a) {
		return nil, errBlocked
	}
	return http.ProxyFromEnvironment(req)
}

// errBlocked is a connection refused before it was made.
var errBlocked = errors.New("blocked address")

// SiteProbe fetches websites. The zero value is ready to use.
type SiteProbe struct {
	// Resolver and Dial stand in for the network in tests.
	Resolver *net.Resolver
	Dial     func(ctx context.Context, network, addr string) (net.Conn, error)
	// RootCAs, when set, replaces the system's roots (tests).
	RootCAs *x509.CertPool
	// UserAgent names the checker.
	UserAgent string
}

// Probe fetches g.URL once.
func (p SiteProbe) Probe(ctx context.Context, g GrantSite) SiteResult {
	res := SiteResult{}
	if err := g.Validate(); err != nil {
		res.Fail, res.Detail = SiteFailBlocked, err.Error()
		return res
	}
	timeout := g.timeout()
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	d := &net.Dialer{Timeout: timeout, Resolver: p.Resolver,
		// Checked on the address actually connected to, after DNS, so a name
		// that resolves to the metadata service is refused too.
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err == nil && blockedAddr(ap.Addr()) {
				return errBlocked
			}
			return nil
		}}
	dial := d.DialContext
	if p.Dial != nil {
		dial = p.Dial
	}
	tr := &http.Transport{
		Proxy:               siteProxy,
		DialContext:         dial,
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: timeout,
		TLSClientConfig:     &tls.Config{RootCAs: p.RootCAs, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2:   true,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= siteRedirects {
			return fmt.Errorf("more than %d redirects", siteRedirects)
		}
		if req.URL.Scheme != "https" && req.URL.Scheme != "http" {
			return errors.New("redirected to an address that is not a website")
		}
		// The check's own headers are for the address it names. They often
		// carry a key, and a redirect elsewhere is not who it was given to.
		if !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
			for _, h := range g.Headers {
				req.Header.Del(h.Name)
			}
		}
		return nil
	}}
	res.Method = g.method()
	var send io.Reader
	if g.Body != "" {
		send = strings.NewReader(g.Body)
	}
	req, err := http.NewRequestWithContext(cctx, res.Method, g.URL, send)
	if err != nil {
		res.Fail, res.Detail = SiteFailBlocked, "the address is not valid"
		return res
	}
	ua := p.UserAgent
	if ua == "" {
		ua = "Argus-Website-Check"
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Cache-Control", "no-cache")
	// A check's own header replaces the probe's (a User-Agent an API insists
	// on); a name given twice is sent twice.
	set := map[string]bool{}
	for _, h := range g.Headers {
		if k := http.CanonicalHeaderKey(h.Name); !set[k] {
			set[k] = true
			req.Header.Set(h.Name, h.Value)
			continue
		}
		req.Header.Add(h.Name, h.Value)
	}
	if g.Body != "" && req.Header.Get("Content-Type") == "" {
		ct := "text/plain; charset=utf-8"
		if b := strings.TrimSpace(g.Body); strings.HasPrefix(b, "{") || strings.HasPrefix(b, "[") {
			ct = "application/json"
		}
		req.Header.Set("Content-Type", ct)
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		res.MS = time.Since(start).Milliseconds()
		res.Fail, res.Detail = classifySiteError(err, timeout)
		return res
	}
	defer resp.Body.Close()
	res.Status = resp.StatusCode
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		na := resp.TLS.PeerCertificates[0].NotAfter.UTC()
		res.CertNotAfter = &na
	}
	var body []byte
	if g.Keyword != "" {
		body, err = io.ReadAll(io.LimitReader(resp.Body, siteBodyMax))
	} else {
		_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, siteBodyMax))
	}
	res.MS = time.Since(start).Milliseconds()
	if err != nil && cctx.Err() != nil {
		res.Fail, res.Detail = SiteFailTimeout, fmt.Sprintf("the page did not finish loading in %s", timeout)
		return res
	}
	wantStatus := g.Status == 0 && resp.StatusCode < 400 || g.Status != 0 && resp.StatusCode == g.Status
	if !wantStatus {
		res.Fail = SiteFailStatus
		if g.Status != 0 {
			res.Detail = fmt.Sprintf("answered %s, not %d", resp.Status, g.Status)
		} else {
			res.Detail = "answered " + resp.Status
		}
		return res
	}
	if g.Keyword != "" && !strings.Contains(string(body), g.Keyword) {
		res.Fail = SiteFailKeyword
		res.Detail = fmt.Sprintf("answered %s, but the page does not contain %q", resp.Status, g.Keyword)
		return res
	}
	res.OK = true
	res.Detail = "answered " + resp.Status
	return res
}

// classifySiteError says which kind of failure err is, in a visitor's words.
func classifySiteError(err error, timeout time.Duration) (string, string) {
	var dnsErr *net.DNSError
	var certErr *tls.CertificateVerificationError
	var unknownAuth x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var invalid x509.CertificateInvalidError
	var recordErr tls.RecordHeaderError
	switch {
	case errors.Is(err, errBlocked):
		return SiteFailBlocked, "the address resolves to a link-local or metadata address, which is never checked"
	case errors.As(err, &dnsErr):
		if dnsErr.IsNotFound {
			return SiteFailDNS, "the name " + dnsErr.Name + " does not exist in DNS"
		}
		if dnsErr.IsTimeout {
			return SiteFailDNS, "DNS did not answer for " + dnsErr.Name
		}
		return SiteFailDNS, "could not look up " + dnsErr.Name + ": " + dnsErr.Err
	case errors.As(err, &certErr), errors.As(err, &unknownAuth), errors.As(err, &hostErr), errors.As(err, &invalid):
		return SiteFailTLS, "the certificate is not valid: " + lastErr(err)
	case errors.As(err, &recordErr):
		return SiteFailTLS, "the server does not speak https on that port"
	case errors.Is(err, context.DeadlineExceeded) || isNetTimeout(err):
		return SiteFailTimeout, fmt.Sprintf("no answer in %s", timeout)
	case errors.Is(err, syscall.ECONNREFUSED):
		return SiteFailConnect, "connection refused: nothing is listening, or a firewall rejects it"
	case errors.Is(err, syscall.ECONNRESET):
		return SiteFailConnect, "the connection was reset"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return SiteFailConnect, "no route to the server"
	case strings.Contains(err.Error(), "tls:"):
		return SiteFailTLS, "the secure connection failed: " + lastErr(err)
	case strings.Contains(err.Error(), "redirect"):
		return SiteFailStatus, lastErr(err)
	}
	return SiteFailConnect, lastErr(err)
}

func isNetTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// lastErr is the innermost message, without the URL a *url.Error wraps it
// in: the address can carry a token in its path.
func lastErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	msg := err.Error()
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return msg
}
