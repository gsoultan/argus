# Networks: VPNs and tunnels

The Networks page (`/networks`, `GET /api/v1/networks`, fleet roles only) shows
the VPN links and tunnels on managed servers, as their agents report them.
Design and the build plan: `plans/vpn/design.md` in the project files (five
PRs: detection, Cloudflare + cloud gateways, WireGuard server networks, site
links, staff VPN).

## What the agent reads (internal/agent/vpn.go)

- VPN software is in the application catalogue (wireguard, cloudflared, warp,
  openvpn, strongswan, libreswan, tailscale, zerotier), so presence and
  running come from the normal app scan.
- WireGuard: `wg show interfaces`, then `wg show <if> <field>` one public
  field at a time (public-key, listen-port, endpoints, allowed-ips,
  latest-handshakes, persistent-keepalive, transfer). **Never** `dump`,
  `showconf`, `private-key`, `preshared-keys` or `all`: they print secrets. A
  test fails if any of those is run. Without `wg`, `/sys/class/net/*/uevent`
  `DEVTYPE=wireguard` lists interfaces with no peers (`sources: ["sysfs"]`).
- cloudflared: its own metrics server on **loopback only**, found from the
  app scan's listeners (process `cloudflared`). `/ready` gives
  readyConnections and connectorId, `/metrics` the build_info version. This is
  the one deliberate exception to "the agent never connects to what it
  finds": loopback, two health-check paths, 2 s timeout, 1 MB cap. Config and
  credentials files are never read.
- Own one-minute loop in `cmd/argus-agent/main.go`. `VPNScan.State` (up/down
  per link, no counters) changing asks for a facts pass at once; otherwise the
  scan rides the ten-minute facts pass (sent on change or hourly).

## What the control plane does (internal/control/networks.go)

- Stored in `agents.vpn` (migration 106), COALESCE so an older agent's facts
  leave it alone; bounded on the way in.
- A peer is judged **at the scan's checked_at**, not now: up if a handshake in
  the 3 minutes before; down/never only when this side has an endpoint *and*
  persistent keepalive (otherwise a healthy link legitimately goes quiet:
  idle). An agent with no heartbeat for 5 minutes makes its links "unknown"
  and raises an agent-quiet warning instead of repeating stale state.
- Links between two Argus servers are matched by interface public key, and the
  console (`linksOf` in `web/src/lib/networks.ts`) shows each once, at its
  worse end.
- Problems: interface down, peer down/never, tunnel not connected, tunnel with
  one connection, VPN software installed but not running (never for
  WireGuard: wg-quick is a oneshot).
