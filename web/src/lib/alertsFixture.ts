import type { AlertPage, AlertPrefs, ConsoleAlert } from '~/lib/live'

// Without a control plane the bell shows these, under the same "sample data"
// the rest of the console is labelled with. Module scope, so marking one read
// lasts until a reload, like the rest of the fixture. Loaded only then.
const ago = (min: number) => new Date(Date.now() - min * 60_000).toISOString()
let fixtureRead = new Set<number>()
const FIXTURE: Omit<ConsoleAlert, 'read'>[] = [
  { seq: 6, at: ago(2), kind: 'approval', severity: 'warning', title: 'rina@northwind.id asks for root on db-02',
    detail: 'Rotate the replication slot, ticket OPS-412.', link: '/approvals' },
  { seq: 5, at: ago(9), kind: 'signin', severity: 'warning', title: 'dewi@northwind.id signed in from a new device',
    detail: 'Chrome 131 on macOS · 182.253.14.7 · authenticator app', link: '/audit' },
  { seq: 4, at: ago(24), kind: 'session', severity: 'warning', title: 'Direct login on web-01 as deploy',
    detail: 'Not through Argus. The agent recorded it.', link: '/activity' },
  { seq: 3, at: ago(64), kind: 'session', severity: 'info', title: 'budi@northwind.id opened SSH on app-03 as deploy',
    detail: 'From 10.20.4.17.', link: '/activity' },
  { seq: 2, at: ago(130), kind: 'approval', severity: 'info', title: 'Your request for deploy on web-01 was approved',
    detail: 'Approved by rizal@northwind.id.', link: '/approvals' },
  { seq: 1, at: ago(190), kind: 'signin', severity: 'info', title: 'rizal@northwind.id signed in',
    detail: 'Firefox 132 on Windows · 10.0.0.12 · authenticator app', link: '/audit' },
]
let fixturePrefs: AlertPrefs = { muted: [], newDevicesOnly: false, popup: true, sound: false }

export function fixturePage(unread: boolean): AlertPage {
  const visible = FIXTURE.filter((a) => a.kind === 'security' || !fixturePrefs.muted.includes(a.kind))
    .filter((a) => !(a.kind === 'signin' && fixturePrefs.newDevicesOnly && a.severity === 'info'))
    .map((a) => ({ ...a, read: fixtureRead.has(a.seq) }))
  return {
    alerts: unread ? visible.filter((a) => !a.read) : visible,
    unread: visible.filter((a) => !a.read).length,
    more: false,
  }
}

/* ── Queries ─────────────────────────────────────────────────────────────── */

export function fixtureMarkRead(input: { seqs?: number[]; upTo?: number }): void {
  if (input.upTo) fixtureRead = new Set(FIXTURE.filter((a) => a.seq <= input.upTo!).map((a) => a.seq))
  for (const s of input.seqs ?? []) fixtureRead.add(s)
}

export function fixtureGetPrefs(): AlertPrefs { return fixturePrefs }

export function fixtureSetPrefs(p: AlertPrefs): AlertPrefs {
  fixturePrefs = { ...p, muted: p.muted.filter((k) => k !== 'security') }
  return fixturePrefs
}
