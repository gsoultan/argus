package tunnel

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestSiteProbeAnswersAndFailures(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<h1>Welcome to shop</h1>")) })
	mux.HandleFunc("/broken", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	mux.HandleFunc("/moved", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ok", http.StatusFound) })
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p := SiteProbe{}
	ctx := context.Background()

	if r := p.Probe(ctx, GrantSite{URL: srv.URL + "/ok", Keyword: "Welcome"}); !r.OK || r.Status != 200 {
		t.Errorf("a page with the word is up: %+v", r)
	}
	if r := p.Probe(ctx, GrantSite{URL: srv.URL + "/moved"}); !r.OK || r.Status != 200 {
		t.Errorf("a redirect is followed to the page: %+v", r)
	}
	if r := p.Probe(ctx, GrantSite{URL: srv.URL + "/moved", Status: 302}); r.OK {
		t.Errorf("the expected status is the final one, after redirects: %+v", r)
	}
	if r := p.Probe(ctx, GrantSite{URL: srv.URL + "/broken"}); r.OK || r.Fail != SiteFailStatus || r.Status != 502 {
		t.Errorf("a 502 is a status failure: %+v", r)
	}
	if r := p.Probe(ctx, GrantSite{URL: srv.URL + "/ok", Keyword: "Checkout"}); r.OK || r.Fail != SiteFailKeyword {
		t.Errorf("a page without the word fails: %+v", r)
	}
	if r := p.Probe(ctx, GrantSite{URL: srv.URL + "/slow", TimeoutSec: 1}); r.OK || r.Fail != SiteFailTimeout {
		t.Errorf("no answer in the timeout is a timeout: %+v", r)
	}

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	_ = ln.Close()
	if r := p.Probe(ctx, GrantSite{URL: "http://" + addr + "/"}); r.OK || r.Fail != SiteFailConnect {
		t.Errorf("a closed port is a connection failure: %+v", r)
	}
	if !NetworkFailure(SiteFailConnect) || !NetworkFailure(SiteFailDNS) || NetworkFailure(SiteFailStatus) {
		t.Error("only DNS, connection and timeout failures can be the checker's own network")
	}
}

func TestSiteProbeTLS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	if r := (SiteProbe{}).Probe(context.Background(), GrantSite{URL: srv.URL}); r.OK || r.Fail != SiteFailTLS {
		t.Errorf("a certificate nobody trusts fails as TLS: %+v", r)
	}
	pool := srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	r := (SiteProbe{RootCAs: pool}).Probe(context.Background(), GrantSite{URL: srv.URL})
	if !r.OK || r.CertNotAfter == nil {
		t.Errorf("a trusted certificate is up and says when it ends: %+v", r)
	}
}

// The cloud metadata service answers with credentials, so no check may
// reach it, whether by address or by a name that resolves to it.
func TestSiteCheckRefusesMetadataAddresses(t *testing.T) {
	for _, u := range []string{"http://169.254.169.254/latest/meta-data/", "http://[fe80::1]/", "ftp://example.com/",
		"https://user:pw@example.com/", " https://example.com", "https://"} {
		if err := ValidSiteURL(u); err == nil {
			t.Errorf("accepted %q", u)
		}
	}
	for _, u := range []string{"https://example.com/health?token=x", "http://10.0.0.5:8080/", "https://[2001:db8::1]/"} {
		if err := ValidSiteURL(u); err != nil {
			t.Errorf("refused %q: %v", u, err)
		}
	}
	// A redirect to it is refused before any connection is made.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()
	if r := (SiteProbe{}).Probe(context.Background(), GrantSite{URL: srv.URL, TimeoutSec: 2}); r.OK || r.Fail != SiteFailBlocked {
		t.Errorf("a redirect to the metadata service is blocked: %+v", r)
	}
	if !blockedAddr(mustAddr("169.254.169.254")) || !blockedAddr(mustAddr("::ffff:169.254.169.254")) || blockedAddr(mustAddr("10.1.2.3")) {
		t.Error("link-local, including IPv4-mapped, is blocked; private is not")
	}
	// The metadata services outside link-local. Root cause of the gap: the
	// rule was link-local only, and AWS's IPv6 endpoint and Alibaba's are not.
	for _, a := range []string{"fd00:ec2::254", "100.100.100.200", "::ffff:100.100.100.200"} {
		if !blockedAddr(mustAddr(a)) {
			t.Errorf("%s is a metadata service and is not blocked", a)
		}
	}
	for _, a := range []string{"fd00:ec2::253", "100.100.100.201", "fd12:3456::1"} {
		if blockedAddr(mustAddr(a)) {
			t.Errorf("%s is an ordinary private address and is blocked", a)
		}
	}
	for _, u := range []string{"http://[fd00:ec2::254]/latest/meta-data/", "http://100.100.100.200/latest/meta-data/"} {
		if err := ValidSiteURL(u); err == nil {
			t.Errorf("accepted %q", u)
		}
	}
}

func TestSiteGrantValidate(t *testing.T) {
	ok := GrantSite{URL: "https://example.com", Status: 204, Keyword: "ok", TimeoutSec: 10}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, g := range []GrantSite{{URL: "https://example.com", Status: 99}, {URL: "https://example.com", TimeoutSec: 61},
		{URL: "https://example.com", Keyword: string(make([]byte, SiteKeywordMax+1))}} {
		if err := g.Validate(); err == nil {
			t.Errorf("accepted %+v", g)
		}
	}
}

func mustAddr(s string) netip.Addr { return netip.MustParseAddr(s) }

func TestSiteProbeSendsTheRequest(t *testing.T) {
	type seen struct {
		method, body, key, ct, ua string
	}
	got := make(chan seen, 4)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- seen{method: r.Method, key: r.Header.Get("X-Api-Key")}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer other.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- seen{method: r.Method, body: string(b), key: r.Header.Get("X-Api-Key"), ct: r.Header.Get("Content-Type"), ua: r.Header.Get("User-Agent")}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/away", func(w http.ResponseWriter, r *http.Request) {
		got <- seen{method: r.Method, key: r.Header.Get("X-Api-Key")}
		http.Redirect(w, r, strings.Replace(other.URL, "127.0.0.1", "localhost", 1)+"/x", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p := SiteProbe{}
	ctx := context.Background()

	g := GrantSite{URL: srv.URL + "/health", Method: http.MethodPost, Body: `{"ping":1}`, Keyword: `"ok"`,
		Headers: []SiteHeader{{Name: "X-Api-Key", Value: "s3cret"}, {Name: "user-agent", Value: "Uptime/1"}}}
	if r := p.Probe(ctx, g); !r.OK || r.Method != http.MethodPost {
		t.Fatalf("a POST with a body to an endpoint that wants one is up: %+v", r)
	}
	s := <-got
	if s.method != "POST" || s.body != `{"ping":1}` || s.key != "s3cret" || s.ct != "application/json" || s.ua != "Uptime/1" {
		t.Errorf("the endpoint got the method, body, headers and a JSON content type: %+v", s)
	}
	if r := p.Probe(ctx, GrantSite{URL: srv.URL + "/health"}); r.OK || r.Method != http.MethodGet {
		t.Errorf("a plain GET is still a GET, and says so: %+v", r)
	}
	<-got

	// Headers go only to the address's own host.
	if r := p.Probe(ctx, GrantSite{URL: srv.URL + "/away", Headers: []SiteHeader{{Name: "X-Api-Key", Value: "s3cret"}}}); !r.OK {
		t.Fatalf("the redirect is followed: %+v", r)
	}
	if first, then := <-got, <-got; first.key != "s3cret" || then.key != "" {
		t.Errorf("a redirect to another host gets none of the check's headers: first %+v, then %+v", first, then)
	}

	for _, bad := range []GrantSite{
		{URL: srv.URL, Method: "DELETE"},
		{URL: srv.URL, Method: http.MethodHead, Keyword: "x"},
		{URL: srv.URL, Body: "x"},
		{URL: srv.URL, Method: http.MethodPost, Headers: []SiteHeader{{Name: "Host", Value: "evil"}}},
		{URL: srv.URL, Method: http.MethodPost, Headers: []SiteHeader{{Name: "X-A", Value: "a\r\nX-B: b"}}},
		{URL: srv.URL, Method: http.MethodPost, Headers: []SiteHeader{{Name: "Bad Name", Value: "a"}}},
		{URL: srv.URL, Method: http.MethodPost, Body: strings.Repeat("x", SiteBodyMax+1)},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("refused: %+v", bad.Method)
		}
	}
	if !(GrantSite{Method: "POST"}).HasRequest() || (GrantSite{Method: "GET"}).HasRequest() || !(GrantSite{Headers: []SiteHeader{{Name: "A"}}}).HasRequest() {
		t.Error("HasRequest is anything but a plain GET")
	}
}
