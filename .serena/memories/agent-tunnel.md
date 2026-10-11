# Agent tunnel

Step 2 of the no-gateway plan (`/mnt/project-files/plans/no-gateway-architecture.md`
in the project). Each enrolled agent holds one outbound connection to
argus-control; the control plane opens streams on it and names a *service*,
never an address. Code: `internal/tunnel` (wire format, both transports),
`internal/agent/tunnel.go` (reconnect loop, service table),
`internal/control/tunnel.go` (hub, admission, ping).

## Transports

- **QUIC** on `tunnel.quic_listen` (UDP, ALPN `argus-tunnel/1`). The agent's
  first stream is a `Hello` with its credential.
- **HTTPS fallback**: `GET /api/v1/agent/tunnel` with `Upgrade: argus-tunnel/1`,
  hijacked into yamux. Always on, on `listen`. The 101 advertises the QUIC port;
  the agent then tries QUIC in the background and moves when it works.
  QUIC failing parks the agent on HTTPS for 10 minutes before trying again.

## Rules

- Admitted only on a live **per-host** credential, checked against the database
  (not the 30s report cache). The shared reporter token never opens a tunnel.
  Hostname claimed must match the credential's.
- One tunnel per credential; the newest replaces the oldest.
- Every 30s the hub pings each tunnel and, in the same round trip to Postgres,
  re-checks the credential (`tunnelSeen`). A revocation on another replica ends
  a tunnel here within one ping. A DB error keeps the tunnel (don't take the
  fleet down because Postgres blinked).
- The agent refuses unknown service names and bounds concurrent streams (64).
  Today it offers only `ping`; sessions come as services in the next step.
- State lives on `agent_credentials.tunnel_*` (migration 018). A row whose
  `tunnel_seen_at` is older than three pings is listed as down: the replica
  holding it may be gone. Freshness is judged with the database's clock.
