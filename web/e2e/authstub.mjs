// A control plane, reduced to the part the sign-in screen talks to.
//
// The rest of the e2e suite builds with VITE_CONTROL_URL empty, where
// isConfigured() is false and LoginGate waves everything through -- so the
// sign-in screen, the branch every operator actually meets, was the one part of
// the console no browser test had ever rendered. Two bugs shipped through that
// gap: a console that landed on its error boundary after every sign-in, and a
// session cookie Safari silently refused.
//
// Real HTTP rather than route interception, because the second of those was a
// cookie-attribute bug. Set-Cookie has to come off a real response for the
// browser to apply its real rules to it, and the assets have to be same-origin
// for the cookie to come back.
//
// Deliberately not the real control plane: that needs Postgres, certificates
// and a signing secret, and this is testing the console rather than the server.
/** @import { IncomingMessage, ServerResponse } from 'node:http' */
import { createServer } from 'node:http'
import { readFile } from 'node:fs/promises'
import { extname, join, normalize } from 'node:path'

const PORT = Number(process.env.STUB_PORT ?? 5511)
const DIST = process.env.STUB_DIST ?? 'dist-live'
// Lets a test assert what the browser does with a cookie it is entitled to
// drop, which is exactly what broke sign-in on Safari.
const SECURE = process.env.STUB_SECURE_COOKIE === '1'

const SESSION = 'stub-session-token'
const GCP_PRINCIPAL = 'argus-control@northwind-ops.iam.gserviceaccount.com'
// argus-control's application in Microsoft Entra, by its client id.
const AZURE_PRINCIPAL = '3f8a1c52-7b0e-4d19-a6c4-91d2e0b57f36'
const AZURE_SUBSCRIPTION = '6b1f2c0e-8a44-4f0b-9d55-0c2a7e4d1a90'
const PASSWORD = 'stub-password'
/** @type {Record<string, string>} */
const TYPES = {
  '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css',
  '.json': 'application/json', '.svg': 'image/svg+xml', '.woff2': 'font/woff2',
  '.png': 'image/png', '.ico': 'image/x-icon', '.wasm': 'application/wasm',
}

/**
 * A session log with the three states that matter to the counters.
 *
 * `silent` is an active session nothing has reported in minutes, and it is the
 * one the console used to get wrong: it counted as live, so the header pill and
 * the Overview tile both overstated what was running, one of them for nineteen
 * days in the real control plane.
 *
 * The three counts are deliberately all different -- 2 live, 3 silent, 5 active
 * -- so a test asserting "the alert's number matches the rows its link lands
 * on" cannot pass by landing on the wrong set.
 */
const ago = (/** @type {number} */ mins) => new Date(Date.now() - mins * 60_000).toISOString()
const session = (/** @type {Record<string, unknown>} */ o) => ({
  userId: 'u1', userEmail: 'lin@northwind.id', assetId: 'a-open-absent-1',
  assetHostname: 'open-absent-1', principal: 'ops', protocol: 'ssh',
  origin: 'brokered', clientIp: '103.20.1.5', fidelity: 'pty',
  recordingBytes: 4096, commandCount: null, accessRequestId: null,
  chainHead: 'c0ffee', recordingKey: 'recordings/2026-09-20/delivered.cast',
  riskFlags: [], endedAt: null, endInferred: false, ...o,
})

/** How often the large recording was downloaded, for the test that it is not unasked. */
let HUGE_FETCHES = 0

const SESSIONS = [
  session({ id: 's-live-1', state: 'active', startedAt: ago(9), lastReportedAt: ago(0), silent: false }),
  session({ id: 's-live-2', state: 'active', startedAt: ago(4), lastReportedAt: ago(0), silent: false }),
  session({ id: 's-quiet-1', state: 'active', startedAt: ago(27_400), lastReportedAt: ago(1_450), silent: true }),
  session({ id: 's-quiet-2', state: 'active', startedAt: ago(9_100), lastReportedAt: ago(700), silent: true }),
  session({ id: 's-quiet-3', state: 'active', startedAt: ago(300), lastReportedAt: ago(12), silent: true }),
  session({ id: 's-direct-today', state: 'closed', origin: 'direct', startedAt: ago(200), endedAt: ago(190), lastReportedAt: ago(190), silent: false, riskFlags: ['bypassed-gateway'] }),
  // Older than the window. If the "Bypassed Argus" link ever loses its 24h
  // scope, this row is what makes the test notice.
  session({ id: 's-direct-old', state: 'closed', origin: 'direct', startedAt: ago(9_000), endedAt: ago(8_990), lastReportedAt: ago(8_990), silent: false, riskFlags: ['bypassed-gateway'] }),
  session({ id: 's-closed-1', state: 'closed', startedAt: ago(400), endedAt: ago(360), lastReportedAt: ago(360), silent: false }),
  // Closed by the control plane after a day of silence, at its last sighting.
  session({ id: 's-inferred-end', state: 'closed', startedAt: ago(9_900), endedAt: ago(8_400), lastReportedAt: ago(8_400), silent: false, endInferred: true }),
  // Sealed, with real recorded bytes, and the artefact still on the gateway
  // that produced it. 44 sessions in the dev control plane are like this.
  session({ id: 's-closed-2', state: 'closed', startedAt: ago(800), endedAt: ago(790), lastReportedAt: ago(790), silent: false, recordingKey: null }),
  // Ended at a time nobody recorded: `endedAt` was once written only inside the
  // seal branch, so a terminated session with no recorder kept a null. 26 of
  // these in dev, every one rendering as still running.
  // A runner's deploy copied through a direct login: the recording is
  // hundreds of megabytes, almost all of it the file. The player asks before
  // it downloads one this size.
  session({ id: 's-huge-transfer', state: 'closed', origin: 'direct', fidelity: 'ebpf', principal: 'sys-admin', clientIp: '10.0.0.167', startedAt: ago(20_000), endedAt: ago(19_999), lastReportedAt: ago(19_999), silent: false, recordingBytes: 269_400_000, riskFlags: ['bypassed-gateway'], recordingKey: 'recordings/2026-11-16/huge.cast' }),
  // An admin removes this one's recording in large-recording.spec.ts.
  session({ id: 's-removable', state: 'closed', origin: 'direct', fidelity: 'ebpf', principal: 'sys-admin', assetHostname: 'imp-crm', clientIp: '10.0.0.167', startedAt: ago(21_000), endedAt: ago(20_999), lastReportedAt: ago(20_999), silent: false, recordingBytes: 269_400_000, riskFlags: ['bypassed-gateway'], recordingKey: 'recordings/2026-11-16/removable.cast' }),
  session({ id: 's-unknown-end', state: 'terminated', startedAt: ago(18_600), endedAt: null, lastReportedAt: ago(18_500), silent: false }),
]

const countSessions = (/** @type {(s: any) => boolean} */ f) => SESSIONS.filter(f).length

/**
 * A small fleet, deliberately mixed.
 *
 * Coverage states a number and links to the inventory filtered to what it
 * counted. Getting that pairing wrong is silent — the page still renders, the
 * link still works, it just lands on a different set — and it shipped twice
 * before a run against a real control plane caught it: an alert headed "5
 * hosts" whose link selected 2, because the count is `bypassPosture === 'open'`
 * and the filter was `agentState === 'absent'`.
 *
 * The postures below are chosen so no two counters agree by accident. An
 * `absent` host that is `monitored`, and a `stale` host that is `open`, are
 * what separate "has no agent" from "can be reached around Argus".
 */
const asset = (/** @type {Record<string, unknown>} */ o) => ({
  id: `a-${o.hostname}`, address: '10.0.0.1', port: 22, os: 'Ubuntu 24.04 LTS',
  groupId: 'g1', protocol: 'ssh', tags: [], principals: ['ops'],
  credentialMode: 'ca-certificate', hostKeyState: 'pinned',
  hostKeyFingerprint: 'SHA256:stub', hostKeyPinnedAt: null, health: 'reachable',
  lastCheckedAt: new Date().toISOString(), agentLastSeenAt: null,
  unmanagedKeyCount: 0, credentialRotatedAt: null, rotationIntervalDays: null,
  ...o,
})

const ASSETS = [
  asset({ hostname: 'open-absent-1', agentState: 'absent', bypassPosture: 'open',
    credentialMode: 'injected-key' }),
  asset({ hostname: 'open-absent-2', agentState: 'absent', bypassPosture: 'open',
    credentialMode: 'injected-password' }),
  asset({ hostname: 'open-stale-1', agentState: 'stale', bypassPosture: 'open',
    hostKeyState: 'unpinned' }),
  // Two different not-pinned states. A single-state filter can select neither
  // set the "Unverified hosts" tile counts, which is why `unverified` exists.
  asset({ hostname: 'monitored-absent', agentState: 'absent', bypassPosture: 'monitored',
    hostKeyState: 'changed' }),
  asset({ hostname: 'monitored-stale', agentState: 'stale', bypassPosture: 'monitored',
    credentialMode: 'injected-key' }),
  asset({ hostname: 'closed-healthy', agentState: 'healthy', bypassPosture: 'enforced' }),
  asset({ hostname: 'win-01', protocol: 'rdp', port: 3389, agentState: 'absent',
    bypassPosture: 'monitored', os: 'Windows Server 2022' }),
]

/**
 * Derived from ASSETS, never written out.
 *
 * Two hand-kept literals would drift, and then the test below would be
 * asserting that they still agree rather than that the console links correctly.
 */
const count = (/** @type {(a: any) => boolean} */ f) => ASSETS.filter(f).length
const COVERAGE = {
  assets: ASSETS.length,
  sshAssets: count((a) => a.protocol === 'ssh'),
  rdpAssets: count((a) => a.protocol === 'rdp'),
  assetsWithAgent: count((a) => a.agentState === 'healthy'),
  assetsAgentStale: count((a) => a.agentState === 'stale'),
  assetsUnmonitored: count((a) => a.bypassPosture === 'open'),
  rdpAwaitingAgent: count((a) => a.protocol === 'rdp' && a.agentState !== 'healthy'),
  unreviewedHosts: 0,
  ignoredHosts: 0,
}

/**
 * Two pending, one already decided.
 *
 * The Overview's "Pending approvals" tile lands on /requests, which defaults to
 * its pending tab -- so the two agree only as long as that default holds.
 * Nothing in the code says so, which is what the test below is for.
 */
const REQUESTS = [
  {
    id: 'r-1', requesterId: 'u1', requesterEmail: 'lin@northwind.id',
    assetIds: ['a-open-absent-1'], assetHostnames: ['open-absent-1'],
    principal: 'ops', justification: 'Investigating INC-4471 on the payments box.',
    durationMinutes: 60, state: 'pending', createdAt: ago(30),
    decidedAt: null, decidedByEmail: null, decisionNote: null,
    expiresAt: null, breakGlass: false,
  },
  {
    id: 'r-2', requesterId: 'u1', requesterEmail: 'lin@northwind.id',
    assetIds: ['a-closed-healthy'], assetHostnames: ['closed-healthy'],
    principal: 'root', justification: 'Rotating the host key after the rebuild.',
    durationMinutes: 30, state: 'pending', createdAt: ago(12),
    decidedAt: null, decidedByEmail: null, decisionNote: null,
    expiresAt: null, breakGlass: true,
  },
  {
    id: 'r-3', requesterId: 'u1', requesterEmail: 'lin@northwind.id',
    assetIds: ['a-monitored-stale'], assetHostnames: ['monitored-stale'],
    principal: 'ops', justification: 'Routine patching window.',
    durationMinutes: 120, state: 'approved', createdAt: ago(300),
    decidedAt: ago(290), decidedByEmail: 'dev@northwind.id',
    decisionNote: 'Approved for the window.', expiresAt: ago(-60), breakGlass: false,
  },
]

/**
 * The audit log as a real control plane serves it: a slice, plus a header
 * saying how big the log is.
 *
 * The two numbers are deliberately different. The console used to verify what
 * it received, report "chain intact", and export an evidence pack documented as
 * the whole chain -- with no way to know it held 500 of 4,546 entries.
 */
const AUDIT_TOTAL = 4546
// The whole log, newest first, so `before` paging is exercised against a real
// server rather than mocked. The console asks for 2000 at a time, so this takes
// three requests and the last one is short -- which is how it learns to stop.
const AUDIT_EVENTS = Array.from({ length: AUDIT_TOTAL }, (_, i) => {
  const seq = AUDIT_TOTAL - i
  return {
    seq,
    id: `ae-${seq}`,
    at: ago(i),
    action: 'session.terminate',
    severity: 'warning',
    actorEmail: 'dev@northwind.id',
    target: 's-live-1',
    detail: `Authorised ending a session (#${seq})`,
  }
})

// Declared after ASSETS and SESSIONS because it counts both of them.
const DAY_AGO = Date.now() - 24 * 60 * 60_000
const within24h = (/** @type {any} */ s) => Date.parse(s.startedAt) > DAY_AGO

const STATS = {
  assetsTotal: ASSETS.length,
  assetsUnreachable: 0,
  hostKeysUnpinned: count((a) => a.hostKeyState !== 'pinned'),
  // Derived, never written out: two hand-kept literals would drift, and then
  // the test would be asserting that they still agree rather than that the
  // console counts and links correctly.
  sessionsActive: countSessions((s) => s.state === 'active' && !s.silent),
  sessionsSilent: countSessions((s) => s.state === 'active' && s.silent),
  sessionsToday: countSessions(within24h),
  requestsPending: REQUESTS.filter((r) => r.state === 'pending').length,
  credentialsOverdue: 0,
  standingCredentialAssets: count((a) => a.credentialMode !== 'ca-certificate'),
  sessionsDirectToday: countSessions((s) => s.origin === 'direct' && within24h(s)),
}

/**
 * The control plane's own settings, as a fresh install serves them: no alert
 * channel set up yet. Enough for the Alerts section to render.
 */
const SETTINGS = {
  configFile: '/etc/argus/control.yaml', consoleUrl: `http://localhost:${PORT}`, fleetUrl: '',
  listen: ':8443', consoleListen: '', quicListen: '', behindTlsProxy: false,
  allowedOrigins: [], trustedProxies: [], loginPerMinute: 10, failuresPerHour: 20,
  ticketsPerMinute: 30, sessionTtl: '8h', ticketTtl: '1m', agentStaleAfter: '2m', logLevel: 'info',
  agentKeepRecordingsDays: 0, archive: { afterDays: 0, storageClass: '' },
  database: { host: 'db', port: 5432, name: 'argus', user: 'argus', sslMode: 'disable', passwordSet: true },
  storage: { enabled: false, endpoint: '', bucket: '', region: '', useSsl: true, accessKey: '', secretKeySet: false },
  email: { enabled: false, via: 'smtp', host: '', port: 0, username: '', passwordSet: false, from: '', to: [],
    panmail: { url: '', providerId: '', apiKeySet: false } },
  telegram: { enabled: false, botTokenSet: false, chatIds: [] },
  fcm: { enabled: false, credentialsSet: false, projectId: '', topic: '', tokens: [] },
  tls: { enabled: false, managed: false, canIssue: false, mutualTls: false },
  signInAlerts: { admins: true, person: true, when: 'new_device' },
  // A control plane off AWS, as most self-hosted ones are: the host gives
  // nothing, so its own identity has no answer; an IAM user's key does.
  cloud: { aws: { default: 'northwind-iam-user', identities: [
    { id: 'main', name: 'Main', organizations: [], source: 'default', accessKeyId: '', secretAccessKeySet: false,
      profile: '', credentialsFile: '', homeRegion: '' },
    { id: 'northwind-iam-user', name: 'Northwind IAM user', organizations: [], source: 'key',
      accessKeyId: 'AKIAIOSFODNN7EXAMPLE', secretAccessKeySet: true, profile: '', credentialsFile: '', homeRegion: '' },
  ] } },
}

const NO_IMDS = 'this control plane has no identity for the provider: no EC2 IMDS role found'
const KEY_ARN = 'arn:aws:iam::123456789012:user/argus-control'
/** What each running identity answers, and who uses it. */
const IDENTITY_STATUS = [
  { identity: 'main', name: 'Main', error: NO_IMDS, use: [] },
  { identity: 'northwind-iam-user', name: 'Northwind IAM user', principal: KEY_ARN,
    use: [{ organization: 'default', name: 'Northwind', accounts: 1 }] },
]

/**
 * @param {ServerResponse} res
 * @param {number} code
 * @param {unknown} body
 * @param {Record<string, string>} [headers]
 */
const json = (res, code, body, headers = {}) => {
  const payload = JSON.stringify(body)
  res.writeHead(code, {
    'content-type': 'application/json',
    'content-length': Buffer.byteLength(payload),
    ...headers,
  })
  res.end(payload)
}

/** @param {IncomingMessage} req */
async function readBody(req) {
  const chunks = []
  // A page that navigates away mid-request aborts its body. Reading it then
  // throws ECONNRESET, and from this async handler that was an unhandled
  // rejection that took the whole stub down, failing every later test on
  // "connection refused". An aborted request gets nothing; the server stays.
  try {
    for await (const c of req) chunks.push(c)
  } catch {
    return {}
  }
  try { return JSON.parse(Buffer.concat(chunks).toString() || '{}') } catch { return {} }
}

/**
 * Two organizations, so the organization menu has something to switch
 * between. Which one a browser is in rides in a cookie of its own, as the real
 * control plane carries it inside the session cookie's claim.
 */
const ORGS = [
  { id: 'default', slug: 'default', name: 'Northwind', default: true, role: 'admin', home: true },
  { id: 'acme0000000a', slug: 'acme', name: 'Acme Logistics', default: false, role: 'owner', home: false },
]
/** @param {IncomingMessage} req */
const orgOf = (req) => {
  const m = /(?:^|;\s*)argus_org=([a-z0-9-]+)/.exec(req.headers.cookie ?? '')
  return ORGS.find((o) => o.slug === m?.[1]) ?? /** @type {typeof ORGS[number]} */ (ORGS[0])
}
const ACME_ASSETS = () => [asset({ hostname: 'acme-web-1', agentState: 'healthy', bypassPosture: 'enforced' })]

const FIREWALL = {
  reported: true, listeningRead: true,
  firewall: { kind: 'ufw', installed: ['ufw'], active: true, defaultIn: 'deny', defaultOut: 'allow', manageable: true,
    readAt: new Date(Date.now() - 4 * 60_000).toISOString(), rules: [] },
  rules: [
    { direction: 'in', action: 'allow', protocol: 'tcp', ports: '22', sources: ['10.8.0.0/24'], madeBy: 'outside',
      raw: '22/tcp ALLOW IN 10.8.0.0/24' },
    { direction: 'in', action: 'allow', protocol: 'tcp', ports: '80,443', madeBy: 'outside', raw: '80,443/tcp ALLOW IN Anywhere' },
    { direction: 'in', action: 'allow', protocol: 'tcp', ports: '5432', sources: ['10.0.0.12'], argus: true, argusPort: 5432,
      name: 'argus 5432', madeBy: 'database', database: { id: 'db-orders', name: 'orders', engine: 'postgres' } },
    { direction: 'in', action: 'deny', protocol: 'any', sources: ['198.51.100.23'], madeBy: 'outside' },
  ],
  listening: [
    { port: 22, process: 'sshd', localOnly: false, reach: { everyone: false, public: false, sources: ['10.8.0.0/24'] } },
    { port: 443, process: 'nginx', localOnly: false, reach: { everyone: true, public: true } },
    { port: 5432, process: 'postgres', kind: 'postgres', localOnly: false, reach: { everyone: false, public: false, sources: ['10.0.0.12'] } },
    { port: 9100, process: 'node_exporter', localOnly: false, reach: { everyone: false, public: false, sources: [] } },
    { port: 6379, process: 'redis-server', kind: 'redis', localOnly: true, reach: { everyone: false, public: false } },
  ],
  names: { '10.0.0.12': 'open-absent-2' },
}
// Changing it: one line Argus keeps, one policy, and the change asked for
// last, which stays waiting for the server.
const FW_POLICY = {
  id: 'p-web', name: 'Web servers', description: 'Web from everyone, metrics from the monitoring servers.', version: 3,
  lines: [
    { key: 'web', direction: 'in', action: 'allow', protocol: 'tcp', ports: '80,443', from: [{ kind: 'everyone' }], note: 'Public site' },
    { key: 'mon', direction: 'in', action: 'allow', protocol: 'tcp', ports: '9100', from: [{ kind: 'tag', value: 'monitoring' }] },
  ],
  defaultOut: 'allow', starter: false, createdBy: 'dev@northwind.id', updatedBy: 'dev@northwind.id',
  updatedAt: new Date(Date.now() - 3 * 3600_000).toISOString(),
  targets: [{ id: 't1', tag: 'web' }, { id: 't2', assetId: 'a-closed-healthy', hostname: 'closed-healthy' }],
  servers: 6, current: 4,
  rollout: { id: 'r1', policyId: 'p-web', policyName: 'Web servers', version: 3, state: 'running',
    summary: 'Version 3 of Web servers to 6 server(s)', requestedBy: 'dev@northwind.id',
    requestedAt: new Date(Date.now() - 6 * 60_000).toISOString(), message: '', total: 6, done: 4, failed: 0,
    servers: ['closed-healthy', 'open-absent-2', 'web-03', 'web-04', 'web-05', 'web-06'].map((h, i) => ({
      id: `c${i}`, assetId: 'a-closed-healthy', hostname: h, kind: 'policy', summary: 'Move the policy Web servers to version 3',
      reason: '', preview: [], state: i < 4 ? 'applied' : i === 4 ? 'trial' : 'queued', wave: i === 0 ? 0 : 1, requestedBy: 'dev@northwind.id',
      requestedAt: new Date().toISOString(), updatedAt: new Date().toISOString(), message: '', ran: [], canPutBack: i < 4 })) },
}
const FW_CHANGES = [
  { id: 'fc-1', assetId: 'a-closed-healthy', hostname: 'closed-healthy', kind: 'lines',
    summary: 'Let in Postgres 5432 from Servers tagged app', reason: 'orders app', preview: [], state: 'applied', wave: 0,
    requestedBy: 'dev@northwind.id', requestedAt: new Date(Date.now() - 26 * 3600_000).toISOString(),
    updatedAt: new Date().toISOString(), message: 'done', canPutBack: true,
    ran: [{ command: "ufw allow in proto tcp from 10.0.0.12 to any port 5432 comment 'argus fw-s4k2p9x1'", ok: true }] },
]
const fwManage = () => ({
  canChange: true, ready: true, defaultOut: 'allow', managers: ['ops@northwind.id'], remotePort: 22,
  always: ["This server's own connection to Argus, and agent updates", 'Name lookups (DNS) to the resolvers the server uses',
    "System updates from the server's package sources (Windows Update on Windows)"],
  own: [{ key: 's4k2p9x1', direction: 'in', action: 'allow', protocol: 'tcp', ports: '5432', from: [{ kind: 'tag', value: 'app' }],
    note: 'orders app', addedBy: 'dev@northwind.id' }],
  policies: [{ id: 'p-web', name: 'Web servers', version: 3, latest: 3, defaultOut: 'allow', lines: FW_POLICY.lines, wanted: true }],
  changes: FW_CHANGES, open: FW_CHANGES.find((c) => ['awaiting_approval', 'queued', 'sent', 'trial', 'waiting_keep'].includes(c.state)),
})
const fwPlan = (/** @type {any} */ b) => ({
  kind: b.takeOver ? 'take_over' : b.turnOn ? 'turn_on' : 'lines',
  summary: b.takeOver ? 'Take over 1 rule(s) made by hand' : 'Let in TCP 22 from private networks',
  preview: b.takeOver
    ? ['ufw --force delete <the rule ' + b.takeOver[0] + '>', "ufw allow in proto tcp from 10.8.0.0/24 to any port 22 comment 'argus fw-s7h3k2m1'"]
    : ["ufw allow in proto tcp from 10.0.0.0/8 to any port 22 comment 'argus fw-s7h3k2m1'",
      "ufw allow in proto tcp from 172.16.0.0/12 to any port 22 comment 'argus fw-s7h3k2m1'",
      "ufw allow in proto tcp from 192.168.0.0/16 to any port 22 comment 'argus fw-s7h3k2m1'",
      "ufw allow in proto tcp from fc00::/7 to any port 22 comment 'argus fw-s7h3k2m1'"],
  lines: [], needsApproval: !!b.takeOver, warnings: [], keep: false,
})
const FIREWALL_FLEET = () => ASSETS.slice(0, 6).map((a, i) => ({
  assetId: a.id, hostname: a.id.slice(2), kind: 'server', os: 'Ubuntu 24.04', tags: [], reported: i !== 5,
  firewall: i === 3 ? '' : i === 4 ? 'firewalld' : 'ufw', active: i !== 2 && i !== 3, manageable: true,
  rules: i === 3 ? 0 : 3 + i, outside: i === 0 || i === 3 ? 0 : 2, openToInternet: i === 1 ? [443, 6379] : i === 2 ? [22, 3306] : [],
  docker: i === 4,
}))

/** @param {IncomingMessage} req */
const signedIn = (req) => (req.headers.cookie ?? '').includes(`argus_session=${SESSION}`)

const server = createServer(async (req, res) => {
  const url = new URL(req.url ?? '/', `http://localhost:${PORT}`)
  const path = url.pathname

  if (path === '/auth/password' && req.method === 'POST') {
    const body = await readBody(req)
    if (body.password !== PASSWORD) {
      // One message for every way of being wrong, as the real handler does.
      return json(res, 401, { error: 'invalid email or password' })
    }
    // Attributes mirror internal/auth/cookies.go. Secure is the one the dev
    // deployment must not set, because the console is reached over HTTP.
    const cookie = `argus_session=${SESSION}; Path=/; Max-Age=28800; HttpOnly; SameSite=Lax`
    return json(res, 200, { email: 'dev@northwind.id', role: 'admin' },
      { 'set-cookie': SECURE ? `${cookie}; Secure` : cookie })
  }
  if (path === '/auth/logout' && req.method === 'POST') {
    return json(res, 200, { status: 'signed out' },
      { 'set-cookie': 'argus_session=; Path=/; Max-Age=0; HttpOnly; SameSite=Lax' })
  }
  if (path === '/auth/me') {
    const org = orgOf(req)
    return signedIn(req)
      ? json(res, 200, {
          authenticated: true, email: 'dev@northwind.id',
          displayName: 'Dev Admin', role: org.role, mfaEnrolled: false,
          org: { id: org.id, slug: org.slug, name: org.name, default: org.default },
          orgs: ORGS, multiOrg: true, platformOwner: org.default,
        })
      : json(res, 401, {
          authenticated: false, passwordEnabled: true, accountsExist: true,
        })
  }
  if (path === '/auth/switch' && req.method === 'POST') {
    if (!signedIn(req)) return json(res, 401, { error: 'unauthorized' })
    const body = await readBody(req)
    const to = ORGS.find((o) => o.slug === body.org)
    if (!to) return json(res, 404, { error: 'you are not a member of that organization' })
    return json(res, 200, { authenticated: true, email: 'dev@northwind.id', role: to.role },
      { 'set-cookie': `argus_org=${to.slug}; Path=/; Max-Age=28800; HttpOnly; SameSite=Lax` })
  }
  // For the large-recording test: whether the player downloaded unasked.
  if (path === '/__stub/huge-fetches') return json(res, 200, { count: HUGE_FETCHES })
  // Each browser project removes s-removable once against this one stub, so
  // the test puts its recording back before it starts.
  if (path === '/__stub/restore-removable' && req.method === 'POST') {
    const target = /** @type {any} */ (SESSIONS.find((/** @type {any} */ v) => v.id === 's-removable'))
    Object.assign(target, {
      recordingKey: 'recordings/2026-11-16/removable.cast', recordingRemovedAt: undefined,
      recordingRemovedBy: undefined, recordingRemovedReason: undefined,
    })
    return json(res, 200, { restored: true })
  }
  if (path.startsWith('/api/')) {
    if (!signedIn(req)) return json(res, 401, { error: 'unauthorized' })
    if (orgOf(req).slug !== 'default') {
      // Everything an organization holds is its own: one server, nothing else.
      if (path === '/api/v1/assets') return json(res, 200, ACME_ASSETS())
      if (path === '/api/v1/stats' || path === '/api/v1/coverage') {
        // Fall through to the shared shapes; the counts are not under test.
      } else if (path.startsWith('/api/v1/platform/')) {
        return json(res, 404, { error: 'not found' })
      } else {
        return json(res, 200, [])
      }
    }
    // A rename answers with the new name and keeps the list as it is, so the
    // other tests sharing this stub still find the names they expect.
    const renamed = /^\/api\/v1\/platform\/organizations\/([a-z0-9]+)\/name$/.exec(path)
    if (renamed && req.method === 'POST') {
      const o = ORGS.find((x) => x.id === renamed[1])
      if (!o) return json(res, 404, { error: 'no such organization' })
      const body = /** @type {any} */ (await readBody(req))
      const name = String(body.name ?? '').trim()
      if (!name) return json(res, 400, { error: 'the name must be between 1 and 120 characters' })
      return json(res, 200, { id: o.id, slug: o.slug, name, default: o.default })
    }
    if (path === '/api/v1/platform/organizations' && req.method === 'GET') {
      return json(res, 200, ORGS.map((o) => ({
        id: o.id, slug: o.slug, name: o.name, default: o.default,
        createdAt: ago(60 * 24 * 30), createdBy: o.default ? 'system' : 'dev@northwind.id',
        counts: o.default ? { servers: ASSETS.length, people: 4 } : { servers: 1, people: 2 },
      })))
    }
    if (path === '/api/v1/stats') return json(res, 200, STATS)
    if (path === '/api/v1/settings' && req.method === 'GET') return json(res, 200, SETTINGS)
    // A test send: refused, as the control plane refuses it, when Panmail is
    // chosen without an API key; otherwise "sent" to the form's recipients.
    if (path === '/api/v1/settings/test-email' && req.method === 'POST') {
      const body = /** @type {any} */ (await readBody(req))
      if (body.via === 'panmail' && !body.panmail?.apiKey && !body.panmail?.apiKeySet) {
        return json(res, 400, { error: 'the test email was not sent: alerts.email: not configured: set panmail.api_key' })
      }
      return json(res, 200, { sent: true, to: body.to ?? [] })
    }
    // Who AWS says the form's identity is: an IAM user for a well-formed key,
    // the instance metadata timeout for the host's own chain, as off EC2.
    if (path === '/api/v1/settings/test-cloud/aws' && req.method === 'POST') {
      const aws = /** @type {any} */ (await readBody(req))
      if (aws.source === 'key' && /^AKIA[A-Z0-9]{16}$/.test(aws.accessKeyId ?? '')) {
        const arn = aws.accessKeyId === 'AKIAIOSFODNN7EXAMPLE' ? KEY_ARN : 'arn:aws:iam::123456789012:user/argus-second'
        return json(res, 200, { provider: 'aws', arn, principal: arn, source: 'the access key in Settings › Cloud identities' })
      }
      return json(res, 400, { error: `AWS did not accept that identity: ${NO_IMDS}` })
    }
    // An account just connected: nothing read yet, nothing changed or asked for.
    if (path === '/api/v1/cloud/resources' && req.method === 'GET') return json(res, 200, { items: [] })
    if (path === '/api/v1/cloud/changes' && req.method === 'GET') return json(res, 200, { changes: [] })
    if (path === '/api/v1/cloud/access' && req.method === 'GET') return json(res, 200, { grants: [], minutes: [30, 60] })
    if (path === '/api/v1/cloud/schedules' && req.method === 'GET') return json(res, 200, { schedules: [], keepRunning: [] })
    if (path === '/api/v1/cloud/idle' && req.method === 'GET') return json(res, 200, { items: [], monthly: 0 })
    if (path === '/api/v1/cloud/findings' && req.method === 'GET') {
      return json(res, 200, { items: [], kept: [], accounts: [{ id: 'ca1', name: 'payments-prod', accountChecks: true }], monthly: 0, checkedAt: new Date().toISOString() })
    }
    if (path === '/api/v1/settings/cloud-identities/aws' && req.method === 'GET') {
      if (!orgOf(req).default) return json(res, 404, { error: 'not found' })
      return json(res, 200, IDENTITY_STATUS)
    }
    if (path === '/api/v1/cloud/accounts' && req.method === 'GET') {
      const bases = [
        { provider: 'aws', identity: 'northwind-iam-user', name: 'Northwind IAM user', default: true, principal: KEY_ARN,
          source: 'the access key in Settings › Cloud identities' },
        { provider: 'aws', identity: 'main', name: 'Main', default: false, error: NO_IMDS },
        { provider: 'gcp', identity: 'main', name: 'Main', default: true, principal: GCP_PRINCIPAL,
          source: 'the service account of the VM argus-control runs on' },
        { provider: 'azure', identity: 'main', name: 'Main', default: true, principal: AZURE_PRINCIPAL,
          source: 'the app registration in cloud.azure' },
      ]
      if (!orgOf(req).default) return json(res, 200, { accounts: [], bases })
      return json(res, 200, { accounts: [{
        id: 'ca1', provider: 'aws', accountId: '412788931204', name: 'payments-prod', access: 'read', environment: 'production',
        identity: 'northwind-iam-user', identityMissing: false, roleArn: 'arn:aws:iam::412788931204:role/ArgusRead',
        identityArn: KEY_ARN, regions: ['us-east-1'], connectedBy: 'dev@northwind.id', connectedAt: '2026-11-01T08:00:00Z',
        syncedAt: new Date(Date.now() - 120_000).toISOString(), syncErrors: {}, counts: {}, manageMissing: [], manageCheckedAt: null,
      }, {
        id: 'ca2', provider: 'gcp', accountId: 'analytics-prod-4182', name: 'analytics-prod', access: 'read', environment: 'production',
        identity: 'main', identityMissing: false, roleArn: 'argus-read@analytics-prod-4182.iam.gserviceaccount.com',
        identityArn: 'argus-read@analytics-prod-4182.iam.gserviceaccount.com', regions: ['europe-west1', 'us-central1', 'global'],
        connectedBy: 'dev@northwind.id', connectedAt: '2026-11-14T08:00:00Z', syncedAt: new Date(Date.now() - 60_000).toISOString(),
        syncErrors: {}, counts: { instance: 6, volume: 7, firewall: 4 }, manageMissing: [], manageCheckedAt: null,
      }, {
        id: 'ca3', provider: 'azure', accountId: AZURE_SUBSCRIPTION, name: 'billing-azure', access: 'read', environment: 'production',
        identity: 'main', identityMissing: false,
        roleArn: `/subscriptions/${AZURE_SUBSCRIPTION}/providers/Microsoft.Authorization/roleDefinitions/acdd72a7-3385-48ef-bd42-f606fba81ae7`,
        identityArn: `/subscriptions/${AZURE_SUBSCRIPTION}/providers/Microsoft.Authorization/roleAssignments/0d5c1f6e-2a3b-5c4d-8e9f-a1b2c3d4e5f6`,
        regions: ['southeastasia', 'westeurope'],
        connectedBy: 'dev@northwind.id', connectedAt: '2026-11-18T08:00:00Z', syncedAt: new Date(Date.now() - 90_000).toISOString(),
        syncErrors: {}, counts: { instance: 4, volume: 5, firewall: 3 }, manageMissing: [], manageCheckedAt: null,
      }], bases })
    }
    // A Google Cloud project: argus-read is where it will be, and is not
    // there yet, which is what the page waits on.
    if (path === '/api/v1/cloud/accounts/prepare' && req.method === 'POST') {
      const body = /** @type {any} */ (await readBody(req))
      if (body.provider === 'azure') {
        const s = body.accountId
        const ext = 'argus-6f1c0e9a2b7d4c5e8f9a0b1c2d3e4f50'
        const role = `/subscriptions/${s}/providers/Microsoft.Authorization/roleDefinitions/acdd72a7-3385-48ef-bd42-f606fba81ae7`
        return json(res, 200, { provider: 'azure', accountId: s, externalId: ext, principal: AZURE_PRINCIPAL,
          source: 'the app registration in cloud.azure', identity: 'main', identityName: 'Main', roleName: 'Reader',
          readRoleArn: role, template: '', canDeploy: false,
          command: `# Gives argus-control the Reader role in ${s}: it may list and describe, nothing else.\nS=${s} R=acdd72a7-3385-48ef-bd42-f606fba81ae7\n`
            + 'grant() {\n  O=$(az ad sp show --id $1 --query id -o tsv 2>/dev/null || az ad sp create --id $1 --query id -o tsv) || return 1\n'
            + '  az rest --method put --url "https://management.azure.com/subscriptions/$S/providers/Microsoft.Authorization/roleAssignments/$2?api-version=2022-04-01" \\\n'
            + `    --body "{\\"properties\\":{\\"principalId\\":\\"$O\\",\\"description\\":\\"Argus reads this subscription's inventory through this assignment. argus-external-id=${ext}\\"}}"\n}\n`
            + `grant ${AZURE_PRINCIPAL} 0d5c1f6e-2a3b-5c4d-8e9f-a1b2c3d4e5f6 && \\\n`
            + (body.manage ? 'M=5a1d0c3e-7b2f-4e6a-9c8d-1f2e3d4c5b6a && \\\naz rest --method put --url "$A/roleDefinitions/$M?api-version=2022-04-01" --body "{...Microsoft.Compute/virtualMachines/deallocate/action...}" && \\\n' : '')
            + 'echo "Done. Argus connects by itself within a few minutes."\n',
          ...(body.manage ? { manageRoleName: 'Argus manage',
            manageRoleArn: `/subscriptions/${s}/providers/Microsoft.Authorization/roleDefinitions/5a1d0c3e-7b2f-4e6a-9c8d-1f2e3d4c5b6a` } : {}) })
      }
      if (body.provider !== 'gcp') return json(res, 400, { error: 'only Google Cloud and Azure are stubbed' })
      const p = body.accountId
      const sa = `argus-read@${p}.iam.gserviceaccount.com`
      const ext = 'argus-6f1c0e9a2b7d4c5e8f9a0b1c2d3e4f50'
      return json(res, 200, { provider: 'gcp', accountId: p, externalId: ext, principal: GCP_PRINCIPAL,
        source: 'the service account of the VM argus-control runs on', identity: 'main', identityName: 'Main', roleName: 'argus-read',
        readRoleArn: sa, template: '', canDeploy: false,
        command: `# Makes argus-read in ${p}: it may list and describe Compute Engine, nothing else.\nP=${p} SA=${sa}\n`
          + 'retry() { for i in 1 2 3 4 5 6 7 8 9 10; do "$@" >/dev/null && return 0; sleep 6; done; return 1; }\n'
          + 'gcloud services enable iam.googleapis.com iamcredentials.googleapis.com --project=$P && \\\n'
          + `{ gcloud iam service-accounts describe $SA --project=$P >/dev/null 2>&1 \\\n  && gcloud iam service-accounts update $SA --project=$P --description="Argus reads this project's Compute Engine inventory through this account. argus-external-id=${ext}" >/dev/null \\\n`
          + `  || gcloud iam service-accounts create argus-read --project=$P --display-name="Argus (read only)" --description="Argus reads this project's Compute Engine inventory through this account. argus-external-id=${ext}"; } && \\\n`
          + 'retry gcloud projects add-iam-policy-binding $P --member=serviceAccount:$SA --role=roles/compute.viewer --condition=None --quiet && \\\n'
          + 'retry gcloud projects add-iam-policy-binding $P --member=serviceAccount:$SA --role=roles/iam.serviceAccountViewer --condition=None --quiet && \\\n'
          + `retry gcloud iam service-accounts add-iam-policy-binding $SA --project=$P --member=serviceAccount:${GCP_PRINCIPAL} --role=roles/iam.serviceAccountTokenCreator --quiet && \\\n`
          + (body.manage
            ? `retry gcloud projects add-iam-policy-binding $P --member=serviceAccount:$M --role=roles/compute.instanceAdmin.v1 --condition=None --quiet && \\\n`
            : '')
          + 'echo "Done. Argus connects by itself within a minute."\n',
        ...(body.manage ? { manageRoleName: 'argus-manage', manageRoleArn: `argus-manage@${p}.iam.gserviceaccount.com` } : {}) })
    }
    if (path === '/api/v1/cloud/accounts/check' && req.method === 'POST') {
      const body = /** @type {any} */ (await readBody(req))
      if (body.provider === 'azure') {
        return json(res, 200, { ready: false, detail: 'The subscription does not let Argus in yet: argus-control has no Reader role there' })
      }
      return json(res, 200, { ready: false, detail: `argus-read@${body.accountId}.iam.gserviceaccount.com does not let Argus in yet: Permission 'iam.serviceAccounts.getAccessToken' denied` })
    }
    // A server whose agent read its instance from EC2's metadata service, and
    // one whose agent names an instance in an account Argus does not read.
    if (path === '/api/v1/assets/a-closed-healthy/cloud' && req.method === 'GET') {
      return json(res, 200, { reported: { provider: 'aws', account: '412788931204', region: 'us-east-1', zone: 'us-east-1a',
        id: 'i-0a41c27b9e3f1d27d', type: 'm6i.large' },
      instance: { accountId: 'ca1', accountName: 'payments-prod', provider: 'aws', providerAccount: '412788931204',
        environment: 'production', firstSeen: '2026-11-01T08:00:00Z', seenAt: new Date(Date.now() - 120_000).toISOString(),
        resource: { kind: 'instance', id: 'i-0a41c27b9e3f1d27d', name: 'closed-healthy', region: 'us-east-1', zone: 'us-east-1a',
          state: 'running', size: 'm6i.large', addresses: ['10.0.0.1'], publicIp: '44.201.17.9' },
        server: { id: 'a-closed-healthy', hostname: 'closed-healthy', by: 'agent' }, madeByArgus: false } })
    }
    if (path === '/api/v1/assets/a-monitored-stale/cloud' && req.method === 'GET') {
      return json(res, 200, { reported: { provider: 'aws', account: '999999999999', region: 'us-east-1', zone: 'us-east-1b',
        id: 'i-0123456789abcdef0', type: 't3.small' }, instance: null })
    }
    // A server's firewall as its agent read it: ufw on, two rules made by
    // hand, one Argus opened for a database, and a port open to the internet.
    if (path === '/api/v1/assets/a-closed-healthy/firewall' && req.method === 'GET') {
      return json(res, 200, { ...FIREWALL, manage: fwManage() })
    }
    if (path === '/api/v1/assets/a-closed-healthy/firewall/plan' && req.method === 'POST') {
      return json(res, 200, fwPlan(await readBody(req)))
    }
    if (path === '/api/v1/assets/a-closed-healthy/firewall/changes' && req.method === 'POST') {
      const b = await readBody(req)
      const p = fwPlan(b)
      const c = { .../** @type {any} */ (FW_CHANGES[0]), id: `fc-${FW_CHANGES.length + 1}`, kind: p.kind, summary: p.summary, reason: b.reason ?? '',
        preview: p.preview, state: p.needsApproval ? 'awaiting_approval' : 'queued', requestedAt: new Date().toISOString(),
        message: '', ran: [], canPutBack: false }
      FW_CHANGES.unshift(c)
      return json(res, 202, c)
    }
    {
      const m = /^\/api\/v1\/firewall-changes\/([^/]+)\/(approve|deny|cancel|keep)$/.exec(path)
      if (m && req.method === 'POST') {
        const c = FW_CHANGES.find((x) => x.id === m[1])
        if (!c) return json(res, 404, { error: 'no such firewall change' })
        if (m[2] === 'keep') Object.assign(c, { keptBy: 'dev@northwind.id', keptAt: new Date().toISOString() })
        else c.state = { approve: 'queued', deny: 'denied', cancel: 'cancelled' }[/** @type {string} */ (m[2])] ?? c.state
        return json(res, 200, c)
      }
    }
    if (path === '/api/v1/firewall-policies' && req.method === 'GET') {
      return json(res, 200, { policies: [FW_POLICY, { ...FW_POLICY, id: 'p-lock', name: 'Locked down', version: 1, starter: true,
        description: 'SSH from private networks only, and nothing going out but what Argus always allows.',
        lines: [{ key: 'ssh', direction: 'in', action: 'allow', protocol: 'tcp', ports: '22', from: [{ kind: 'private' }] }],
        defaultOut: 'deny', targets: [{ id: 't3', tag: 'pci' }], servers: 2, current: 2, rollout: undefined }] })
    }
    if (path === '/api/v1/firewall-policies/p-web' && req.method === 'GET') return json(res, 200, FW_POLICY)
    if (path === '/api/v1/firewalls' && req.method === 'GET') return json(res, 200, { servers: FIREWALL_FLEET() })
    // Everything else about one asset (logs, disks, packages…) is not stubbed:
    // a 404 lets each card say so instead of drawing an empty list as data.
    if (/^\/api\/v1\/assets\/[^/]+\/.+/.test(path)) return json(res, 404, { error: 'not stubbed' })
    if (path === '/api/v1/coverage') return json(res, 200, COVERAGE)
    if (path === '/api/v1/assets') return json(res, 200, ASSETS)
    // A window of a larger log, which is what a real control plane serves: the
    // console asks for the most recent 500 and the dev log holds 4,546. No
    // server hashes, so the page takes its "confirming its own arithmetic"
    // branch -- what is under test is whether it admits how much it has, not
    // whether the arithmetic is right.
    if (path === '/api/v1/audit') {
      const limit = Math.min(Number(url.searchParams.get('limit')) || 500, 2000)
      const before = Number(url.searchParams.get('before')) || 0
      // Strictly less than `before`, matching the control plane. An off-by-one
      // here would either repeat a link or skip one, and both break the chain.
      const page = AUDIT_EVENTS.filter((e) => (before > 0 ? e.seq < before : true)).slice(0, limit)
      return json(res, 200, page, { 'x-argus-audit-total': String(AUDIT_TOTAL) })
    }
    if (path === '/api/v1/requests') {
      const want = url.searchParams.get('state')
      return json(res, 200, want ? REQUESTS.filter((r) => r.state === want) : REQUESTS)
    }
    // Sealed, with real recorded bytes, and the artefact still on the gateway
    // that produced it -- 18 sessions in the dev control plane are in exactly
    // this state. The control plane's own wording, verbatim, because the
    // console repeats it rather than inventing its own.
    if (path === '/api/v1/sessions/s-removable/recording' && req.method === 'DELETE') {
      const body = await readBody(req)
      const target = /** @type {any} */ (SESSIONS.find((/** @type {any} */ v) => v.id === 's-removable'))
      if (String(body.confirm ?? '').toLowerCase() !== target.assetHostname) {
        return json(res, 400, { error: `type the host name, ${target.assetHostname}, to confirm` })
      }
      Object.assign(target, {
        recordingKey: null, recordingRemovedAt: new Date().toISOString(),
        recordingRemovedBy: 'dev@northwind.id', recordingRemovedReason: body.reason,
      })
      return json(res, 200, { session: target.id, removed: true, by: 'dev@northwind.id' })
    }
    if (path === '/api/v1/sessions/s-huge-transfer/recording') {
      HUGE_FETCHES += 1
      res.writeHead(200, { 'content-type': 'application/x-asciicast', 'x-argus-chain-verified': 'unverified' })
      return res.end('{"version":2,"width":80,"height":24}\n[0.1,"i","scp -t /srv/app\\r\\n"]\n[0.2,"o","deployed\\r\\n"]\n')
    }
    if (/^\/api\/v1\/sessions\/[^/]+\/recording$/.test(path)) {
      return json(res, 404, {
        error: 'recording is not in object storage; it exists only on the host that produced it',
      })
    }
    const one = /^\/api\/v1\/sessions\/([^/]+)$/.exec(path)
    if (one) {
      const found = SESSIONS.find((/** @type {any} */ v) => v.id === one[1])
      return found ? json(res, 200, found) : json(res, 404, { error: 'no such session' })
    }
    if (path === '/api/v1/sessions') {
      // The control plane narrows by state server-side; returning everything
      // here would let a console bug that forgets to filter still look right.
      const want = url.searchParams.get('state')
      return json(res, 200, want ? SESSIONS.filter((/** @type {any} */ s) => s.state === want) : SESSIONS)
    }
    return json(res, 200, [])
  }

  // Static assets, with the SPA fallback every client-side route needs.
  const rel = normalize(path).replace(/^(\.\.[/\\])+/, '')
  for (const candidate of [join(DIST, rel), join(DIST, 'index.html')]) {
    try {
      const body = await readFile(candidate)
      res.writeHead(200, { 'content-type': TYPES[extname(candidate)] ?? 'application/octet-stream' })
      return res.end(body)
    } catch { /* fall through to the SPA entry point */ }
  }
  res.writeHead(404).end()
})

server.listen(PORT, () => console.log(`auth stub on http://localhost:${PORT} (dist=${DIST}, secure=${SECURE})`))
