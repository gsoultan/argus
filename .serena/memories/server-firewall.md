# The firewall on each server

Design and mockups: `plans/server-firewall/` in the project files (approved
2026-11-23). All of it shipped in one PR (#354): read, change (Linux and
Windows) with a safety net, turn on, take over, and firewall policies.

## PR1: read only

- The agent reads its host's firewall with the facts report (every 10 min):
  `internal/agent/hostfw.go`. Linux picks the firewall in charge in this
  order: ufw active, firewalld running, iptables with filter rules, nftables
  input chain, else the first installed (off). Windows reads Defender Firewall
  in process with PowerShell (`hostfw_windows.go`). On Linux the read runs as
  `argus-agent firewall-read` under `systemd-run` (root, outside the daemon's
  sandbox), like the other root helpers.
- Shape: `tunnel.HostFirewall` (`internal/tunnel/hostfirewall.go`), at most
  500 rules (`Bounded`). A rule Argus made carries the comment `argus <port>`
  (`ArgusCommentPort`). `ReachOf(port)` says who a port is open to:
  everyone, a public address, these sources, or unknown. Tested in Go, not TS.
- Stored in `agents.firewall` (JSONB, migration 113, COALESCE so an older
  agent's facts keep it). Served by `GET /api/v1/assets/{id}/firewall` and
  `GET /api/v1/firewalls` (fleet roles only, `seesWholeFleet`), in
  `internal/control/firewall_view.go`. Each rule says who made it (argus,
  database = the per-database-port rule, outside = by hand); each listening
  port says who can reach it; addresses are named by the Argus server that
  has them.
- Console: Firewall tab on servers and workstations
  (`components/server/FirewallTab.tsx`), and Assets › Firewalls
  (`routes/assets.firewalls.tsx`). Live only: no sample data.

## Rules for the later PRs (agreed)

- The agent dials out only; outgoing policies always allow argus-control,
  DNS and system updates ("Always allowed"). Every change has a 2-minute
  auto-undo unless the agent checks back in; argus-control's own server
  keeps its HTTPS and QUIC ports open.
- Rules made by hand are changed only after "Take over". Argus may install
  ufw/firewalld and turn a firewall on, with approval and a 10-minute Keep.
- Admins, and operators with the "Manage firewall" permission.

## Changing it (PR #354)

- Agent: heartbeat field `firewall` (tunnel.FirewallBeat), order in
  `reply["firewall"]`. Every change is a trial undone after 2 min without
  Argus; turn-on / newly blocking going out also need Keep within 10 min.
- Control: migration 114. `firewall_lines.go` (line model, fwResolver; a line
  naming servers with no address is skipped, never widened),
  `firewall_changes.go` (FWDelta, change states, put back),
  `firewall_beat.go` (buildFWChange, syncFirewall, FirewallProtectLines for
  the control host), `firewall_policies.go` (policies, versions, targets,
  rollouts: first server then groups of five), `firewall_routes.go`
  (planFW checks, approval kind `servers.firewall`).
- Web: `components/server/FirewallChanges.tsx`, routes
  `assets.firewall-policies.*`, queries in `lib/firewallQueries.ts`.
