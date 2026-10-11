package tunnel

import (
	"bytes"
	"strings"
	"testing"
)

// A header is a service name. Anything longer is not one, and reading it
// without a bound would let the other end make this one buffer forever.
func TestAnOversizedHeaderIsRefused(t *testing.T) {
	long := `{"service":"` + strings.Repeat("a", maxHeader) + `"}` + "\n"
	var h Header
	if err := ReadJSON(strings.NewReader(long), &h); err == nil {
		t.Fatal("a header past the bound was accepted")
	}
	if err := WriteJSON(&bytes.Buffer{}, Header{Service: strings.Repeat("a", maxHeader)}); err == nil {
		t.Error("a header past the bound was written")
	}
}

// The header reader must stop at the newline: what follows is the service's.
func TestTheHeaderLeavesTheServiceBytes(t *testing.T) {
	r := strings.NewReader(`{"service":"ping"}` + "\nPAYLOAD")
	var h Header
	if err := ReadJSON(r, &h); err != nil || h.Service != "ping" {
		t.Fatalf("got %+v, %v", h, err)
	}
	rest := make([]byte, 16)
	n, _ := r.Read(rest)
	if string(rest[:n]) != "PAYLOAD" {
		t.Errorf("service bytes after the header = %q", rest[:n])
	}
}
