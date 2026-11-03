package notify

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"strings"
	"sync"
	"testing"
	"time"
)

// mimeParts parses a rendered message and returns its headers and each body
// part, decoded, by content type.
func mimeParts(t *testing.T, raw []byte) (mail.Header, map[string]string) {
	t.Helper()
	m, err := mail.ReadMessage(bytes.NewReader(bytes.ReplaceAll(raw, []byte("\r\n.."), []byte("\r\n."))))
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, raw)
	}
	out := map[string]string{}
	ct, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("content type: %v", err)
	}
	if strings.HasPrefix(ct, "multipart/") {
		r := multipart.NewReader(m.Body, params["boundary"])
		for {
			p, err := r.NextRawPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			pct, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
			out[pct] = decodeBody(t, p.Header.Get("Content-Transfer-Encoding"), p)
		}
		return m.Header, out
	}
	out[ct] = decodeBody(t, m.Header.Get("Content-Transfer-Encoding"), m.Body)
	return m.Header, out
}

func decodeBody(t *testing.T, enc string, r io.Reader) string {
	t.Helper()
	switch strings.ToLower(enc) {
	case "base64":
		r = base64.NewDecoder(base64.StdEncoding, r)
	case "quoted-printable":
		r = quotedprintable.NewReader(r)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

func TestPanmailConfigIsCheckedAtStartUp(t *testing.T) {
	ok := PanmailConfig{URL: "https://mail.example.com", APIKey: "pm_key", ProviderID: "0f8b8f4e-0000-4000-8000-000000000000"}
	for name, cfg := range map[string]EmailConfig{
		"no gateway":     {Via: ViaPanmail, From: "a@x", To: []string{"b@x"}},
		"no url":         {Via: ViaPanmail, Panmail: &PanmailConfig{APIKey: "k", ProviderID: "p"}, From: "a@x", To: []string{"b@x"}},
		"no key":         {Via: ViaPanmail, Panmail: &PanmailConfig{URL: ok.URL, ProviderID: "p"}, From: "a@x", To: []string{"b@x"}},
		"no provider":    {Via: ViaPanmail, Panmail: &PanmailConfig{URL: ok.URL, APIKey: "k"}, From: "a@x", To: []string{"b@x"}},
		"key in clear":   {Via: ViaPanmail, Panmail: &PanmailConfig{URL: "http://mail.example.com", APIKey: "k", ProviderID: "p"}, From: "a@x", To: []string{"b@x"}},
		"no recipients":  {Via: ViaPanmail, Panmail: &ok, From: "a@x"},
		"unknown method": {Via: "carrier-pigeon", Host: "smtp", From: "a@x", To: []string{"b@x"}},
	} {
		if _, err := NewEmail(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// An SMTP host is not needed to send through Panmail.
	if _, err := NewEmail(EmailConfig{Via: ViaPanmail, Panmail: &ok, From: "a@x", To: []string{"b@x"}}); err != nil {
		t.Fatal(err)
	}
}

type panmailCall struct {
	key  string
	body map[string]any
}

// fakePanmail answers SendEmail as a gateway does, recording each request.
func fakePanmail(t *testing.T, answer func(n int, w http.ResponseWriter)) (string, func() []panmailCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []panmailCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/panmail.v1.EmailService/SendEmail" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		calls = append(calls, panmailCall{key: r.Header.Get("X-API-Key"), body: body})
		n := len(calls)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		answer(n, w)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []panmailCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]panmailCall(nil), calls...)
	}
}

func accepted(_ int, w http.ResponseWriter) {
	_, _ = io.WriteString(w, `{"messageId":"0b0e0000-0000-4000-8000-000000000001","status":"EMAIL_EVENT_TYPE_PENDING"}`)
}

func TestPanmailSendsBothBodiesWithTheProviderAndKey(t *testing.T) {
	url, calls := fakePanmail(t, accepted)
	e, err := NewEmail(EmailConfig{Via: ViaPanmail, From: "argus@x.io", To: []string{"sec@x.io"},
		Panmail: &PanmailConfig{URL: url, APIKey: "pm_secret", ProviderID: "prov-1"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.SendTo(ctx, []string{"dewi@acme.co"}, "Argus: evil\r\nBcc: x@evil.io", "text body", "<p>html</p>"); err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != 1 {
		t.Fatalf("%d calls", len(got))
	}
	c := got[0]
	if c.key != "pm_secret" || c.body["providerId"] != "prov-1" || c.body["from"] != "argus@x.io" ||
		c.body["bodyText"] != "text body" || c.body["bodyHtml"] != "<p>html</p>" {
		t.Fatalf("request: key=%q body=%v", c.key, c.body)
	}
	if to, _ := c.body["to"].([]any); len(to) != 1 || to[0] != "dewi@acme.co" {
		t.Fatalf("to = %v", c.body["to"])
	}
	if s, _ := c.body["subject"].(string); strings.ContainsAny(s, "\r\n") {
		t.Fatalf("subject kept a line break: %q", s)
	}
	if k, _ := c.body["idempotencyKey"].(string); !strings.HasPrefix(k, "argus-") {
		t.Fatalf("no idempotency key: %v", c.body["idempotencyKey"])
	}
}

// A send whose outcome is unknown is asked again with the same key, so the
// gateway can answer with the message it already queued rather than a copy.
func TestPanmailRetriesAnUnknownOutcomeWithTheSameKey(t *testing.T) {
	url, calls := fakePanmail(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"code":"aborted","message":"a concurrent send holds the key"}`)
			return
		}
		accepted(n, w)
	})
	e, err := NewEmail(EmailConfig{Via: ViaPanmail, From: "argus@x.io", To: []string{"sec@x.io"},
		Panmail: &PanmailConfig{URL: url, APIKey: "k", ProviderID: "p"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Send(context.Background(), "s", "b"); err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != 2 || got[0].body["idempotencyKey"] != got[1].body["idempotencyKey"] {
		t.Fatalf("calls = %+v", got)
	}
}

// A refusal says what to fix and is not repeated, and the API key is not in it.
func TestPanmailRefusalNamesTheFixAndNotTheKey(t *testing.T) {
	url, calls := fakePanmail(t, func(_ int, w http.ResponseWriter) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"code":"permission_denied","message":"missing scope email:send"}`)
	})
	e, _ := NewEmail(EmailConfig{Via: ViaPanmail, From: "argus@x.io", To: []string{"sec@x.io"},
		Panmail: &PanmailConfig{URL: url, APIKey: "pm_SECRET", ProviderID: "p"}})
	err := e.Send(context.Background(), "s", "b")
	if err == nil || !strings.Contains(err.Error(), "email:send") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "pm_SECRET") {
		t.Fatalf("error leaks the key: %v", err)
	}
	if n := len(calls()); n != 1 {
		t.Fatalf("refusal sent %d times", n)
	}
}
