package gateway

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the package if any test leaves a goroutine running.
//
// This package spawns one per relay direction, per session, per listener and
// per policy sync. A forward that does not tear down cleanly is invisible in a
// passing test and shows up in production as a gateway whose memory climbs for
// a week -- exactly the failure that unit tests are worst at catching, and the
// reason this is enforced for the whole package rather than per test.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		// x/crypto/ssh keeps an internal handshake goroutine parked on a
		// channel read for the life of a connection; it is released when the
		// underlying conn closes, which can trail the test by a scheduling
		// quantum.
		goleak.IgnoreTopFunction("golang.org/x/crypto/ssh.(*handshakeTransport).readLoop"),
		goleak.IgnoreTopFunction("golang.org/x/crypto/ssh.(*handshakeTransport).kexLoop"),
		// net/http's idle connection reaper in httptest servers.
		goleak.IgnoreTopFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"),
		goleak.IgnoreAnyFunction("internal/poll.runtime_pollWait"),
	)
}
