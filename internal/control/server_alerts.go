package control

import (
	"context"
	"time"

	"github.com/gsoultan/argus/internal/notify"
)

/*
"Server connected": administrators are told when a server's agent connects.

The first connection after enrolment is the moment a new server becomes
usable, which is what someone who just ran the install command is waiting to
hear, and a server nobody expected to see join is worth hearing about too. It
goes to the bell (as "joined" already did at enrolment) and to the email and
Telegram channels that sign-in alerts use, to the same people.

Reconnections are opt-in ("every"): agents redial after every network blip and
control plane restart, so telling anyone about each one trains them to ignore
the channel. Even then each server is announced at most once every
serverReconnectQuiet, and a redial that replaces a connection still open here
is not announced at all.
*/

// ServerAlertPolicy is when administrators are told a server connected.
type ServerAlertPolicy string

const (
	// ServerAlertFirst tells them once, when a server first connects. The
	// default.
	ServerAlertFirst ServerAlertPolicy = "first"
	// ServerAlertEvery also tells them when a server comes back.
	ServerAlertEvery ServerAlertPolicy = "every"
	// ServerAlertOff tells nobody by email or Telegram. The bell still
	// shows a new server.
	ServerAlertOff ServerAlertPolicy = "off"
)

// ValidServerAlertPolicy reports whether p is one this build knows.
func ValidServerAlertPolicy(p string) bool {
	switch ServerAlertPolicy(p) {
	case "", ServerAlertFirst, ServerAlertEvery, ServerAlertOff:
		return true
	}
	return false
}

// serverReconnectQuiet is how often one server's reconnection is announced.
const serverReconnectQuiet = 30 * time.Minute

// ServerConnected is the TunnelHub's OnConnect.
func (a *API) ServerConnected(ctx context.Context, c ServerConnection) {
	if c.First {
		a.alertServer(ctx, "", c.Host+" is connected",
			"Its agent connected for the first time and the server is ready to use.")
	}
	policy := a.ServerAlerts
	if policy == "" {
		policy = ServerAlertFirst
	}
	switch {
	case policy == ServerAlertOff, a.SignInAlerts == nil:
		return
	case !c.First && (policy != ServerAlertEvery || c.Replaced):
		return
	case !c.First && !a.serverAlertLimiter.firstWithin(tenantIDOf(ctx)+"\x00"+c.Host,
		time.Now(), serverReconnectQuiet):
		return
	}
	admins, err := a.store.AdministratorEmails(ctx)
	if err != nil {
		a.log.Error("could not list administrators for a server alert", "error", err)
	}
	a.SignInAlerts.ServerConnected(notify.ServerConnected{
		Host: c.Host, Org: orgLabel(ctx), First: c.First,
		Transport: c.Transport, RemoteAddr: c.RemoteAddr, At: time.Now().UTC(),
	}, admins, tenantIDOf(ctx) == DefaultTenantID)
}
