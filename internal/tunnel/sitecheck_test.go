package tunnel

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
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
