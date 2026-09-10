package gateway

import (
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/gsoultan/argus/internal/rdp"
)

// An elevated Windows account is not reachable through the native proxy, even
// when the inventory allows it.
//
// The SSH path and the browser path both make an elevated principal produce an
// approved access request first. This path checked only the inventory, so
// Administrator opened -- with a vaulted credential injected on the way -- for
// anyone who could reach the port and send a cookie saying so.
//
// It cannot enforce the rule the same way, because it does not know who is
// asking: the mstshash cookie is unauthenticated and names a principal and a
// target, not a person. There is nobody to hold an approval and nobody to name
// in the audit chain, so the answer is no until this path authenticates one.
func TestRDPRefusesAnElevatedPrincipalTheInventoryAllows(t *testing.T) {
	targetAddr, fingerprint := windowsHost(t, rdp.ProtocolSSL)
	host, portStr, _ := net.SplitHostPort(targetAddr)
	port, _ := strconv.Atoi(portStr)

	// Administrator *is* in the allowlist this time. Being listed is
	// permission to ask, not permission to have.
	asset := Asset{
		Hostname: "win-01", Address: host, Port: port,
		Protocol: ProtocolRDP, Principals: []string{"ops", "Administrator"},
	}
	gwAddr, _ := startRDPGateway(t, asset, fingerprint)

	conn, err := net.Dial("tcp", gwAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	if _, err := conn.Write(clientCR("Administrator:win-01", rdp.ProtocolSSL)); err != nil {
		t.Fatal(err)
	}
	reply, err := rdp.ReadPDU(conn)
	if err != nil {
		t.Fatalf("no refusal was sent: %v", err)
	}
	if _, failure, _ := rdp.ParseConnectionConfirm(reply); failure == 0 {
		t.Error("the gateway brokered an Administrator session with no approval " +
			"behind it and no authenticated requester to attach one to")
	}
}

// An ordinary principal is unaffected: it needs no approval, so nothing here
// should stand between it and a session.
func TestRDPStillBrokersAnOrdinaryPrincipal(t *testing.T) {
	targetAddr, fingerprint := windowsHost(t, rdp.ProtocolSSL)
	host, portStr, _ := net.SplitHostPort(targetAddr)
	port, _ := strconv.Atoi(portStr)

	asset := Asset{
		Hostname: "win-01", Address: host, Port: port,
		Protocol: ProtocolRDP, Principals: []string{"ops"},
	}
	gwAddr, _ := startRDPGateway(t, asset, fingerprint)

	conn, err := net.Dial("tcp", gwAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	if _, err := conn.Write(clientCR("ops:win-01", rdp.ProtocolSSL)); err != nil {
		t.Fatal(err)
	}
	reply, err := rdp.ReadPDU(conn)
	if err != nil {
		t.Fatalf("no reply: %v", err)
	}
	if _, failure, _ := rdp.ParseConnectionConfirm(reply); failure != 0 {
		t.Errorf("an ordinary principal was refused (failure code %d); the "+
			"elevation check is catching sessions it should not", failure)
	}
}
