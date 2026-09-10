package control

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// benchBody is one session report, the shape the busiest endpoint receives.
func benchBody(b *testing.B) []byte {
	b.Helper()
	head := "a1b2c3d4e5f60718293a4b5c6d7e8f900112233445566778899aabbccddeeff0"
	ended := time.Now().UTC()
	body, err := json.Marshal(Session{
		ID: "3ae54ffe-7cd2-4fa2-c920-559e98b9f713", UserEmail: "lin@northwind.id",
		AssetHostname: "pay-01", Principal: "ops", Protocol: "ssh",
		Origin: "brokered", State: "closed", StartedAt: time.Now().UTC().Add(-time.Minute),
		EndedAt: &ended, ClientIP: "10.0.0.9", Fidelity: "pty", RecordingBytes: 918273,
		ChainHead: &head, RiskFlags: []string{"root-principal", "off-hours"},
		ReportedBy: "gateway",
	})
	if err != nil {
		b.Fatal(err)
	}
	return body
}

// benchRequest reuses one request and only resets its body, so the measurement
// is decodeReport rather than the cost of building an http.Request.
func benchRequest(body []byte) (*http.Request, func()) {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/report/session", nil)
	return r, func() { r.Body = io.NopCloser(bytes.NewReader(body)) }
}

// The reporter path, as it runs on every session report from every gateway.
func BenchmarkDecodeReport(b *testing.B) {
	body := benchBody(b)
	req, reset := benchRequest(body)
	log := &capturingLogger{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reset()
		var in Session
		if err := decodeReport(req, &in, log, "report/session"); err != nil {
			b.Fatal(err)
		}
	}
}

// The same body with a field this build does not know, which is the case the
// unknown-field scan exists for.
func BenchmarkDecodeReportWithAnUnknownField(b *testing.B) {
	var m map[string]any
	if err := json.Unmarshal(benchBody(b), &m); err != nil {
		b.Fatal(err)
	}
	m["somethingNewer"] = "from a gateway ahead of this build"
	body, err := json.Marshal(m)
	if err != nil {
		b.Fatal(err)
	}
	req, reset := benchRequest(body)
	log := &capturingLogger{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reset()
		var in Session
		if err := decodeReport(req, &in, log, "report/session"); err != nil {
			b.Fatal(err)
		}
	}
}
