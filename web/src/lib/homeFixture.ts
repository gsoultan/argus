import { EPOCH, buildAuditSkeleton } from '~/lib/seed'
import type { AccessRequest, Asset, FleetStats, HomeSnapshot, Session } from '~/types/domain'

interface FixtureDb { assets: Asset[]; sessions: Session[]; requests: AccessRequest[] }

/**
 * Home's dashboard from the fixture: counted from the same rows as every
 * other fixture page, with the usage and sign-in figures a control plane
 * would read from its agents and its audit log made up, since the fixture
 * has neither. Home says "Sample data" over all of it.
 */
export function computeHome(db: FixtureDb, st: FleetStats): HomeSnapshot {
  const HOUR = 3_600_000
  const hours: [number, number, number][] = Array.from({ length: 24 }, () => [0, 0, 0])
  const kinds: [number, number, number] = [0, 0, 0]
  for (const s of db.sessions) {
    const ago = Math.floor((EPOCH - Date.parse(s.startedAt)) / HOUR)
    if (ago < 0 || ago > 23) continue
    const k = s.protocol === 'rdp' ? 1 : s.protocol === 'postgres' ? 2 : 0
    hours[23 - ago]![k] += 1
    kinds[k] += 1
  }
  const live = db.sessions.filter((s) => s.state === 'active' && !s.silent)
  const admin = (p: string) => p === 'root' || p.toLowerCase() === 'administrator'
  const stale = db.assets.filter((a) => a.agentState === 'stale' && a.health !== 'unreachable').length
  const busy = db.assets.slice(0, 5).map((a, i) => ({ assetId: a.id, name: a.hostname, busy: [88, 62, 54, 37, 21][i]! }))
  const wave = (n: number, f: (i: number) => number) => Array.from({ length: n }, (_, i) => f(i))
  const pending = db.requests.filter((r) => r.state === 'pending')
  return {
    servers: { total: st.assetsTotal, down: st.assetsUnreachable, quiet: stale, answering: st.assetsTotal - st.assetsUnreachable - stale },
    live: { count: live.length, root: live.filter((s) => admin(s.principal)).length, database: live.filter((s) => s.protocol === 'postgres').length, silent: st.sessionsSilent },
    sessions: { last24h: kinds[0] + kinds[1] + kinds[2], weekAgo: Math.round((kinds[0] + kinds[1] + kinds[2]) * 0.88), hours, kinds },
    waiting: { count: pending.length, oldestAt: pending.map((r) => r.createdAt).sort()[0] ?? null },
    processor: { average: 31, reporting: busy.length, busiest: busy },
    disks: db.assets.length > 1 ? [{ assetId: db.assets[1]!.id, name: db.assets[1]!.hostname, mount: '/var/log', used: 91 }] : [],
    signIns: { last24h: 48, failed24h: 2, failed15m: 0, hours: [1, 0, 0, 0, 0, 1, 3, 8, 9, 6, 4, 3, 5, 4, 2, 1, 0, 0, 0, 1, 0, 0, 0, 0] },
    liveList: live.slice(0, 8).map((s) => ({
      id: s.id, user: s.userEmail, principal: s.principal, host: s.assetHostname,
      kind: s.protocol === 'rdp' ? 'desktop' : s.protocol === 'postgres' ? 'database' : 'terminal', startedAt: s.startedAt,
    })),
    audit: buildAuditSkeleton().map((e) => ({ ...e, hash: '', prevHash: '' })).sort((a, b) => b.seq - a.seq).slice(0, 6),
    trends: {
      since: new Date(EPOCH - 59 * 60_000).toISOString(),
      answering: wave(60, () => st.assetsTotal - st.assetsUnreachable - stale),
      live: wave(60, (i) => Math.max(0, Math.round(live.length + Math.sin(i / 5) * 1.5 - (59 - i) / 30))),
      waiting: wave(60, (i) => Math.min(pending.length, Math.floor(i / 15))),
      processor: wave(60, (i) => Math.round(30 + 4 * Math.sin(i / 3.1) + 2 * Math.cos(i / 1.7))),
    },
    unreviewedHosts: 0,
    at: new Date(EPOCH).toISOString(),
  }
}

