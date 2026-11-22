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

## Cloud VPNs (internal/control/cloud/vpn.go)

- `cloud.KindVPN`, read by the normal cloud sync. Details: tunnelsUp,
  tunnelsTotal, tunnels ("addr=UP; addr=DOWN: why"), remoteAddress,
  remoteNetworks, gateway, vpnType, routing. `cloud.SetTunnels` sets the
  state: up, partial, down.
- AWS: DescribeVpnConnections + DescribeCustomerGateways, tunnels from
  VgwTelemetry. **Never** reads CustomerGatewayConfiguration (it holds the
  pre-shared keys). Azure: `Microsoft.Network/connections` and
  `localNetworkGateways`; sharedKey is never mapped; no status means
  "unknown", not down. GCP: `/aggregated/vpnTunnels` grouped per gateway and
  peer so HA VPN's two tunnels are one connection; ESTABLISHED is up.
- Every provider treats the VPN list as **optional**: if the role cannot list
  it, the kind is dropped from the snapshot's Kinds (so nothing is marked
  gone) and the rest of the inventory still syncs.

## Cloudflare accounts (cloudflare_accounts.go, cfapi/)

- Table `cloudflare_accounts` (migration 107), up to 10 per organization.
  Token sealed like Telegram tokens (key from signing_secret, label
  "argus cloudflare tokens v1"), only its last three characters shown.
  Adding or a new token reads the account first: nothing is saved that
  cannot read tunnels.
- `cfapi.Client.Read`: cfd_tunnel (required), warp_connector, teamnet/routes,
  devices, magic/ipsec_tunnels + gre_tunnels (each optional; `read` lists what
  worked, `errors` why the rest did not). GET only; a test fails on any other
  method. Read every 5 minutes (`RunCloudflareSync`) or on "Read now".
- Matching: a Cloudflare connection's `client_id` is the `connectorId` that
  cloudflared's /ready reports to the agent. Unmatched → "unknown machine"
  warning, worded so it may also be a server whose metrics Argus cannot read.
- Problems: cf-tunnel-down (down), degraded, single connector, inactive with
  routes, unknown machine, cloudflared over a year old, account read failed.

## The bell (networks_watch.go)

- Alert kind `network` (console_alerts check extended in migration 107).
  Every minute `RunNetworkWatch` raises one alert per down problem, subject
  `network:<problem key>`, and closes it with "Back up" when the key is gone.
  A key's asset whose agent is quiet is neither raised nor closed. Warnings
  stay on the page only.

## Server networks: WireGuard set up through agents (wg_networks.go, agent/wgnet*.go)

- Tables `wg_networks` + `wg_members` (migration 108), `agents.wgnet_state`.
  Shapes: mesh (peers carry /32s) and hub (spoke's one peer is the hub with
  the whole range; the hub forwards). Interface `argus` + 6 hex of the id.
- Every change (create, change, remove) is a draft in `pending`; nothing
  reaches an agent until another admin approves (self-approval rule from
  self_approval.go with runDeciders). Approval re-checks clashes.
- Clash check: other Argus networks (and their drafts), non-Argus WireGuard
  addresses from agent scans, cloud network/subnet `Size`, Cloudflare routes,
  each member's own addresses, and the same UDP port on a member.
- Heartbeat `wgnet`: agent sends public keys per network and its last run;
  control answers the full set. nil (not empty) on read errors, so an agent
  never tears networks down because the DB blinked. Peers appear once their
  key is reported.
- Agent: private key stays in `<state>/wgnet/<id>.key` (0600); helper
  `argus-agent wgnet-apply` via systemd-run writes /etc/wireguard/argus*.conf,
  wg-quick up, opens the port in ufw/firewalld, then waits up to 2 minutes for
  the daemon to confirm a heartbeat got through (confirm-<run> file);
  otherwise restores files/interfaces and closes ports it opened. AllowedIPs
  must sit inside the network range, so the control-plane route never moves.
- Network watch only manages alerts whose subject starts `network:`; the
  "change was undone" alert is plain news.
