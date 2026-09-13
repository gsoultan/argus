import type {
  AccessRequest, Asset, AssetGroup, AuditAction, AuditEvent, AuditSeverity,
  RiskFlag, Session, User,
} from '~/types/domain'

/** Deterministic PRNG — the fixture must not change between reloads. */
function mulberry32(seed: number) {
  return () => {
    seed |= 0
    seed = (seed + 0x6d2b79f5) | 0
    let t = Math.imul(seed ^ (seed >>> 15), 1 | seed)
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296
  }
}
const rnd = mulberry32(0xa19c5)
const pick = <T,>(xs: readonly T[]): T => xs[Math.floor(rnd() * xs.length)]!
const between = (lo: number, hi: number) => lo + Math.floor(rnd() * (hi - lo))

/** Fixed epoch so timestamps are stable across reloads. */
const NOW = Date.parse('2026-08-28T09:40:00Z')
const ago = (mins: number) => new Date(NOW - mins * 60_000).toISOString()
const ahead = (mins: number) => new Date(NOW + mins * 60_000).toISOString()

const hex = (n: number) =>
  Array.from({ length: n }, () => '0123456789abcdef'[Math.floor(rnd() * 16)]).join('')
const b64 = (n: number) =>
  Array.from(
    { length: n },
    () => 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/'[Math.floor(rnd() * 64)],
  ).join('')
const uuid = () =>
  `${hex(8)}-${hex(4)}-4${hex(3)}-a${hex(3)}-${hex(12)}`

/* ── Users ───────────────────────────────────────────────────────────────── */

export const users: User[] = [
  { id: uuid(), email: 'r.hakim@northwind.id', displayName: 'R. Hakim', role: 'owner', mfaEnrolled: true, lastSeenAt: ago(3) },
  { id: uuid(), email: 'dewi.p@northwind.id', displayName: 'Dewi P.', role: 'admin', mfaEnrolled: true, lastSeenAt: ago(18) },
  { id: uuid(), email: 'tomas.lie@northwind.id', displayName: 'Tomas Lie', role: 'approver', mfaEnrolled: true, lastSeenAt: ago(52) },
  { id: uuid(), email: 'a.wijaya@northwind.id', displayName: 'A. Wijaya', role: 'operator', mfaEnrolled: true, lastSeenAt: ago(1) },
  { id: uuid(), email: 'kevin.tan@northwind.id', displayName: 'Kevin Tan', role: 'operator', mfaEnrolled: false, lastSeenAt: ago(140) },
  { id: uuid(), email: 'siti.n@northwind.id', displayName: 'Siti N.', role: 'operator', mfaEnrolled: true, lastSeenAt: ago(9) },
  { id: uuid(), email: 'audit@northwind.id', displayName: 'Internal Audit', role: 'auditor', mfaEnrolled: true, lastSeenAt: ago(410) },
  { id: uuid(), email: 'breakglass@northwind.id', displayName: 'Break-glass', role: 'admin', mfaEnrolled: true, lastSeenAt: ago(14_400) },
]
export const currentUser = users[1]!

/* ── Groups & assets ─────────────────────────────────────────────────────── */

export const groups: AssetGroup[] = [
  { id: uuid(), name: 'prod-payments', description: 'PCI cardholder data environment', assetCount: 0 },
  { id: uuid(), name: 'prod-core', description: 'Core banking API tier', assetCount: 0 },
  { id: uuid(), name: 'prod-data', description: 'Postgres primaries and replicas', assetCount: 0 },
  { id: uuid(), name: 'staging', description: 'Pre-production mirror', assetCount: 0 },
  { id: uuid(), name: 'edge', description: 'Load balancers and bastion tier', assetCount: 0 },
]

const OS = ['Ubuntu 24.04 LTS', 'Debian 12', 'Rocky Linux 9', 'Amazon Linux 2023', 'RHEL 9.4']

function makeAssets(): Asset[] {
  const out: Asset[] = []
  const spec: [string, string, number, string[]][] = [
    ['pay', 'prod-payments', 8, ['pci', 'tier-1']],
    ['core', 'prod-core', 12, ['tier-1']],
    ['db', 'prod-data', 6, ['stateful', 'tier-1']],
    ['stg', 'staging', 9, ['tier-3']],
    ['edge', 'edge', 4, ['public', 'tier-2']],
  ]
  for (const [prefix, groupName, count, tags] of spec) {
    const group = groups.find((g) => g.name === groupName)!
    for (let i = 1; i <= count; i++) {
      // Most of the fleet is healthy and pinned; a small tail is not. That tail
      // is what the dashboard is for.
      const roll = rnd()
      const hostKeyState = roll > 0.93 ? 'unpinned' : roll > 0.90 ? 'changed' : 'pinned'
      const health = rnd() > 0.94 ? (rnd() > 0.5 ? 'degraded' : 'unreachable') : 'reachable'
      const credentialMode =
        groupName === 'prod-payments' ? 'ca-certificate' : rnd() > 0.65 ? 'ca-certificate' : 'injected-key'
      const isCa = credentialMode === 'ca-certificate'

      // Agent rollout is partial, which is the realistic state for any fleet
      // mid-migration — and the thing the dashboard has to make impossible to
      // ignore.
      const agentRoll = rnd()
      const agentState = agentRoll > 0.82 ? 'absent' : agentRoll > 0.74 ? 'stale' : 'healthy'
      // Only a host with a live agent AND certificate-only trust is truly
      // closed; everything else is either merely watched, or wide open.
      const bypassPosture =
        agentState === 'absent' ? 'open'
        : isCa && rnd() > 0.45 ? 'enforced'
        : 'monitored'
      out.push({
        id: uuid(),
        hostname: `${prefix}-${String(i).padStart(2, '0')}.${groupName.replace('prod-', '')}.northwind.id`,
        address: `10.${between(20, 60)}.${between(0, 12)}.${between(2, 250)}`,
        port: 22,
        os: pick(OS),
        tags,
        groupId: group.id,
        credentialMode,
        hostKeyState,
        hostKeyFingerprint: hostKeyState === 'unpinned' ? null : `SHA256:${b64(43)}`,
        hostKeyPinnedAt: hostKeyState === 'unpinned' ? null : ago(between(1440, 40_000)),
        health,
        lastCheckedAt: ago(between(0, 6)),
        // Agent coverage is the coverage metric for recording: without one,
        // a direct connection to port 22 is invisible.
        agentState,
        agentLastSeenAt:
          agentState === 'absent' ? null : ago(agentState === 'stale' ? between(180, 2200) : between(0, 3)),
        bypassPosture,
        unmanagedKeyCount: bypassPosture === 'enforced' ? 0 : (rnd() > 0.7 ? between(1, 5) : 0),
        principals: rnd() > 0.6 ? ['deploy', 'ops', 'root'] : ['deploy', 'ops'],
        credentialRotatedAt: isCa ? null : ago(between(200, 190_000)),
        rotationIntervalDays: isCa ? null : pick([30, 60, 90]),
      })
      group.assetCount++
    }
  }
  return out
}
const generatedAssets = makeAssets()

/**
 * One Remote Desktop host, so the fixture exercises the desktop player.
 *
 * Cloned from a generated asset rather than written from scratch, so a field
 * added to Asset later is populated here without anyone remembering to. The
 * console's RDP paths -- the canvas replay, its memory ceiling, the connect
 * page's "Open desktop" branch -- had nothing to run against until this.
 */
const win01: Asset = {
  ...generatedAssets[0]!,
  id: uuid(),
  hostname: 'win-01.corp.northwind.id',
  address: '10.44.7.12',
  port: 3389,
  os: 'Windows Server 2022',
  tags: ['windows', 'corp', 'jump'],
  protocol: 'rdp',
  credentialMode: 'injected-password',
  principals: ['Administrator', 'ops'],
  // No Windows agent exists yet; the coverage page says so rather than
  // counting this as a gap an operator could close.
  agentState: 'absent',
  agentLastSeenAt: null,
  bypassPosture: 'monitored',
  unmanagedKeyCount: 0,
  credentialRotatedAt: ago(4_000),
  rotationIntervalDays: 30,
}

export const assets: Asset[] = [...generatedAssets, win01]

/* ── Sessions ────────────────────────────────────────────────────────────── */

function makeSessions(): Session[] {
  const out: Session[] = []
  const operators = users.filter((u) => u.role === 'operator' || u.role === 'admin')

  const push = (state: Session['state'], startMinsAgo: number, durMins: number | null) => {
    const asset = pick(assets.filter((a) => a.protocol !== 'rdp'))
    const user = pick(operators)
    const principal = pick(asset.principals)
    // A direct session is only possible where the host isn't locked down.
    const canBypass = asset.bypassPosture !== 'enforced'
    const origin: Session['origin'] = canBypass && rnd() > 0.88 ? 'direct' : 'brokered'

    // Direct sessions are captured by the host agent, so they are always eBPF
    // fidelity — there was no gateway in the path to tee a PTY.
    const fidelity: Session['fidelity'] =
      origin === 'direct' ? 'ebpf' : rnd() > 0.55 ? 'ebpf' : 'pty'

    const flags: RiskFlag[] = []
    if (origin === 'direct') flags.push('bypassed-gateway')
    if (principal === 'root') flags.push('root-principal')
    if (asset.hostKeyState !== 'pinned') flags.push('unpinned-host-key')
    const hour = new Date(NOW - startMinsAgo * 60_000).getUTCHours()
    if (hour < 1 || hour > 15) flags.push('off-hours')
    if (rnd() > 0.93) flags.push('new-asset-for-user')
    if (rnd() > 0.96) flags.push('bulk-file-transfer')

    out.push({
      id: uuid(),
      userId: user.id,
      userEmail: user.email,
      assetId: asset.id,
      assetHostname: asset.hostname,
      principal,
      protocol: rnd() > 0.88 ? 'sftp' : 'ssh',
      origin,
      state,
      startedAt: ago(startMinsAgo),
      endedAt: durMins === null ? null : ago(startMinsAgo - durMins),
      clientIp: `103.${between(20, 99)}.${between(0, 255)}.${between(2, 250)}`,
      fidelity,
      recordingBytes: between(2_000, 900_000),
      commandCount: fidelity === 'ebpf' ? between(3, 180) : null,
      accessRequestId: null,
      chainHead: hex(64),
      riskFlags: flags,
    })
  }

  for (let i = 0; i < 6; i++) push('active', between(2, 95), null)
  for (let i = 0; i < 60; i++) {
    const start = between(100, 4300)
    push(rnd() > 0.96 ? 'terminated' : 'closed', start, between(2, 90))
  }
  return out.sort((a, b) => Date.parse(b.startedAt) - Date.parse(a.startedAt))
}
const generatedSessions = makeSessions()

/** A finished desktop session on win-01, so replay has an RDP artefact to load. */
const rdpSession: Session = {
  ...generatedSessions[0]!,
  id: uuid(),
  assetId: win01.id,
  assetHostname: win01.hostname,
  principal: 'ops',
  protocol: 'rdp',
  origin: 'brokered',
  state: 'closed',
  startedAt: ago(310),
  endedAt: ago(262),
  fidelity: 'rdp',
  recordingBytes: 38_500_000,
  commandCount: null,
  riskFlags: [],
}

export const sessions: Session[] = [...generatedSessions, rdpSession].sort(
  (a, b) => Date.parse(b.startedAt) - Date.parse(a.startedAt),
)

/* ── Access requests ─────────────────────────────────────────────────────── */

const JUSTIFICATIONS = [
  'INC-4471 — payment settlement worker stuck in retry loop, need to inspect queue depth.',
  'CHG-2210 — apply approved kernel patch during the Thursday maintenance window.',
  'Investigating elevated p99 latency on the core API tier reported by Grafana alert 118.',
  'INC-4488 — disk pressure on the primary; need to rotate and ship old WAL segments.',
  'Scheduled quarterly access review evidence collection for ISO 27001 A.8.2.',
  'INC-4502 — TLS certificate renewal failed on the edge tier, cert expires in 14h.',
]

function makeRequests(): AccessRequest[] {
  const out: AccessRequest[] = []
  const requesters = users.filter((u) => u.role === 'operator')
  const approvers = users.filter((u) => u.role === 'approver' || u.role === 'admin')

  const push = (state: AccessRequest['state'], minsAgo: number, breakGlass = false) => {
    const n = between(1, 4)
    // Pick without replacement — a request listing the same host twice reads
    // as a bug to anyone reviewing the queue.
    const chosen: Asset[] = []
    while (chosen.length < n) {
      const candidate = pick(assets)
      if (!chosen.some((a) => a.id === candidate.id)) chosen.push(candidate)
    }
    const requester = pick(requesters)
    const decided = state !== 'pending'
    const approver = pick(approvers)
    const duration = pick([60, 120, 240])
    out.push({
      id: uuid(),
      requesterId: requester.id,
      requesterEmail: requester.email,
      assetIds: chosen.map((a) => a.id),
      assetHostnames: chosen.map((a) => a.hostname),
      principal: breakGlass ? 'root' : pick(['deploy', 'ops']),
      justification: pick(JUSTIFICATIONS),
      durationMinutes: duration,
      state,
      createdAt: ago(minsAgo),
      decidedAt: decided ? ago(minsAgo - between(1, 20)) : null,
      decidedByEmail: decided ? approver.email : null,
      decisionNote:
        state === 'denied' ? 'Scope too broad — re-request for the single affected host.' : null,
      expiresAt: state === 'approved' ? ahead(duration - between(0, 40)) : null,
      breakGlass,
    })
  }

  push('pending', 4)
  push('pending', 11)
  push('pending', 26, true)
  push('pending', 47)
  for (let i = 0; i < 14; i++) {
    push(rnd() > 0.78 ? 'denied' : rnd() > 0.75 ? 'expired' : 'approved', between(120, 9000))
  }
  return out.sort((a, b) => Date.parse(b.createdAt) - Date.parse(a.createdAt))
}
export const accessRequests = makeRequests()

/* ── Audit chain ─────────────────────────────────────────────────────────── */

const ACTION_POOL: [AuditAction, AuditSeverity][] = [
  ['session.start', 'info'], ['session.end', 'info'], ['session.command', 'info'],
  ['file.download', 'notice'], ['file.upload', 'notice'], ['request.create', 'info'],
  ['request.approve', 'notice'], ['request.deny', 'notice'], ['credential.rotate', 'notice'],
  ['hostkey.pin', 'notice'], ['auth.login', 'info'], ['policy.change', 'warning'],
  ['session.terminate', 'warning'], ['auth.mfa_fail', 'warning'], ['hostkey.mismatch', 'critical'],
]

/**
 * Events carry a `prevHash` but NOT a valid `hash` — the real hashes are
 * computed in the verification worker at load. Seeding fake digests here would
 * make the integrity check meaningless.
 */
export const auditSeedCount = 480

export function buildAuditSkeleton(): Omit<AuditEvent, 'hash' | 'prevHash'>[] {
  const out: Omit<AuditEvent, 'hash' | 'prevHash'>[] = []
  for (let i = 0; i < auditSeedCount; i++) {
    const [action, severity] = pick(ACTION_POOL)
    const actor = pick(users)
    const asset = pick(assets)
    const detail =
      action === 'session.command'
        ? pick(['systemctl status payments-worker', 'journalctl -u core-api -n 200', 'tail -f /var/log/syslog', 'df -h', 'sudo -l'])
        : action === 'hostkey.mismatch'
          ? 'Presented host key does not match the pinned fingerprint. Session refused.'
          : action === 'credential.rotate'
            ? 'Rotated injected key after session checkout.'
            : action === 'auth.mfa_fail'
              ? 'TOTP verification failed (attempt 2 of 3).'
              : 'ok'
    out.push({
      seq: auditSeedCount - i,
      id: uuid(),
      at: ago(i * between(2, 9)),
      action,
      severity,
      actorEmail: actor.email,
      target: asset.hostname,
      detail,
    })
  }
  return out
}

export const EPOCH = NOW
