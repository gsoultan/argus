package cfapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const acct = "0123456789abcdef0123456789abcdef"

func server(t *testing.T, routes map[string]string, seen *[]string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("%s %s: only reads are allowed", r.Method, r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`))
			return
		}
		mu.Lock()
		*seen = append(*seen, r.URL.Path)
		mu.Unlock()
		body, ok := routes[strings.TrimPrefix(r.URL.Path, "/accounts/"+acct)]
		if !ok {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":9109,"message":"Unauthorized to access requested resource"}]}`))
			return
		}
		_, _ = w.Write([]byte(body))
	}))
}

func TestReadKeepsWhatTheTokenAllows(t *testing.T) {
	var seen []string
	srv := server(t, map[string]string{
		"/cfd_tunnel": `{"success":true,"result":[{"id":"t1","name":"web","status":"degraded","connections":[
			{"client_id":"c1","client_version":"2024.6.1","colo_name":"sin01","origin_ip":"203.0.113.5"}]}],
			"result_info":{"page":1,"total_pages":1,"total_count":1}}`,
		"/teamnet/routes": `{"success":true,"result":[{"network":"10.20.0.0/16","tunnel_id":"t1","tunnel_name":"web"}],
			"result_info":{"page":1,"total_pages":1,"total_count":1}}`,
		"/magic/ipsec_tunnels": `{"success":true,"result":{"ipsec_tunnels":[{"name":"office","customer_endpoint":"198.51.100.7","cloudflare_endpoint":"162.159.1.1","interface_address":"10.100.0.1/31"}]}}`,
	}, &seen)
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	snap, err := c.Read(context.Background(), acct, "tok", time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Tunnels) != 1 || snap.Tunnels[0].Connections[0].ClientID != "c1" || snap.Tunnels[0].Status != "degraded" {
		t.Fatalf("tunnels %+v", snap.Tunnels)
	}
	if len(snap.Routes) != 1 || len(snap.Magic) != 1 || snap.Magic[0].Kind != "ipsec" {
		t.Fatalf("routes %+v magic %+v", snap.Routes, snap.Magic)
	}
	for _, l := range []string{ListTunnels, ListRoutes, ListMagic} {
		if !contains(snap.Read, l) {
			t.Errorf("%s not read: %v", l, snap.Read)
		}
	}
	for _, l := range []string{ListDevices, ListConnectors} {
		if contains(snap.Read, l) || snap.Errors[l] == "" {
			t.Errorf("%s should be missing with a reason: %v %v", l, snap.Read, snap.Errors)
		}
	}
}

func TestReadRefusedToken(t *testing.T) {
	var seen []string
	srv := server(t, map[string]string{}, &seen)
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	_, err := c.Read(context.Background(), acct, "wrong", time.Now())
	if !errors.Is(err, ErrToken) {
		t.Fatalf("err %v", err)
	}
	if _, err := c.Read(context.Background(), "nope", "tok", time.Now()); err == nil {
		t.Fatal("bad account id accepted")
	}
}

func TestPagesFollowsResultInfo(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.RawQuery)
		if r.URL.Query().Get("page") == "1" {
			_, _ = w.Write([]byte(`{"success":true,"result":[{"name":"a"}],"result_info":{"page":1,"total_pages":2,"total_count":2}}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":true,"result":[{"name":"b"}],"result_info":{"page":2,"total_pages":2,"total_count":2}}`))
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	got, total, err := pages[Route](context.Background(), c, "tok", srv.URL+"/x?is_deleted=false")
	if err != nil || len(got) != 2 || total != 2 || len(seen) != 2 {
		t.Fatalf("got %v total %d err %v seen %v", got, total, err, seen)
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
