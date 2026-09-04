# Gateway policy

What a brokered session may do, owned by the control plane and enforced by the
gateway. Added 2026-09-04; the console had shipped the UI for it first.

## Shape

One row in `gateway_policy` (migration `006`), enforced single by
`id BOOLEAN PRIMARY KEY CHECK (id)`. Eight booleans: four SSH forwarding
channels, four recording guarantees. Field names are identical across
`internal/control.GatewayPolicy`, `internal/gateway.Policy`,
`internal/reporter.GatewayPolicy` and `web/src/types/domain.ts` — deliberately,
so the four cannot drift into disagreeing about what a toggle is called.

## Which way is safe

`false` closes a forwarding channel; `true` keeps a recording guarantee. So a
**zero-valued Policy is never safe** — it denies forwarding but also switches
off SFTP decoding and fail-closed recording. Nothing constructs one:
`DefaultPolicy()` / `DefaultGatewayPolicy()` are the only entry points, and
`NewPolicyHolder()` starts from them.

Risk is a property of the *position*, not the field. Turning SFTP proxying off
loses per-file audit events, so it loosens the gateway exactly as much as
turning agent forwarding on. `policyFields` in `internal/control/policy.go`
encodes this once and drives both the audit severity and the console's warning.

## Enforcement points

The gateway already enforced all of this by hardcoding it. Policy did not
introduce the checks, it named them:

- `server.go` channel switch — `direct-tcpip` and friends
- `server.go` `handleGlobalRequests` — `tcpip-forward`. These used to go to
  `ssh.DiscardRequests`, which silently refuses; `ssh -R` then hangs until it
  times out instead of being told no.
- `session.go` `pumpRequests` default arm — `auth-agent-req@openssh.com`,
  `x11-req`
- `session.go` subsystem arm — SFTP decoding

All four are implemented in `internal/gateway/forward.go`. They are one shape —
join a channel on one side of the gateway to a matching one on the other — so
there is a single `relayChannels` and four thin callers rather than four
hand-rolled copies that would drift in error handling and in what they record.

- **-L** `direct-tcpip`: the gateway dials **from the target**, not from itself.
  `-L` through Argus reaches what the target can reach, which is what the
  network controls around the target were written for. Dialling from the
  gateway would silently grant its network position to every user.
- **-R** `tcpip-forward`: `client.Listen` on the target, then a
  `forwarded-tcpip` channel back to the operator per accepted connection.
  Listeners are tracked in `remoteForwards` and closed with the session — a
  port must not outlive the authorisation that opened it.
- **Agent / X11**: relayed verbatim. The gateway must not parse the agent
  protocol; the whole objection to agent forwarding is that whoever holds the
  gateway can sign with the user's keys, and parsing would position it to.

A browser session has no `userConn`, so forwarding is unavailable there and
says so rather than failing halfway through opening a channel.

## What a tunnel records

**Not its contents.** A tunnel carries someone else's protocol and can be
arbitrarily large; capturing it would put database traffic in a session
recording. Argus records the *fact*: endpoints, times, and bytes each way, as
`forward.open` / `forward.close` / `forward.refused` / `forward.listen` events
in the **audit chain** — a tunnel out of a bastion is exactly what someone must
later be able to prove happened. The Settings page states this boundary rather
than letting a buyer assume the recording covers it.

## Distribution

`GET /api/v1/gateway/policy` behind the **reporter** credential, separate from
the console's `GET /api/v1/policy`. A gateway holds a machine token and no
browser session. Sharing one handler between the two audiences is how a browser
eventually ends up reading something meant for a gateway.

`SyncPolicy` polls every minute. On any failure the holder keeps what it has:
an unreachable control plane must not change what a gateway permits **in either
direction** — losing contact should not open a channel, and should not close one
an owner deliberately opened either, or an outage looks like a policy change.

Policy is read once per connection. Sessions keep the policy they started under;
tightening policy is not how you stop a session in progress, terminating it is.

## Why agent forwarding is offered at all

It was reviewed and kept, deliberately. Credential injection means it is not
needed to *reach* the target, and while the channel is open anyone with root on
the gateway or the target can sign with the operator's keys.

It stays because a real workflow needs it — git on the target, against a forge
that authenticates the person — and a gateway that cannot do it sends that
operator around the gateway to a direct sshd session. An unrecorded bypass is
worse than a recorded, audited, deliberately-enabled risk, and coverage is what
this product actually defends. The whole Coverage page rests on the same
premise.

Off by default, owner-or-admin to change, `warning` severity on every channel,
described in full in the console. If the trade stops holding, deleting the field
and its column is the entire reversal.

## Bounds

`MaxLocalForwards` (32) and `MaxRemoteForwards` (8) per session. Both structures
are driven by client-supplied input — a channel request and an address to bind —
so without a ceiling an authorised session can exhaust the gateway from inside.
Refusals name the limit, so someone who genuinely needs more asks rather than
diagnosing a phantom fault. The limit is on what is *held*, so closing a forward
returns budget, and re-binding an address already held replaces its listener
rather than consuming another slot.
