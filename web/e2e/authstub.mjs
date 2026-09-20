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
  chainHead: null, riskFlags: [], endedAt: null, ...o,
})

const SESSIONS = [
  session({ id: 's-live-1', state: 'active', startedAt: ago(9), lastReportedAt: ago(0), silent: false }),
  session({ id: 's-live-2', state: 'active', startedAt: ago(4), lastReportedAt: ago(0), silent: false }),
  session({ id: 's-quiet-1', state: 'active', startedAt: ago(27_400), lastReportedAt: ago(1_450), silent: true }),
  session({ id: 's-quiet-2', state: 'active', startedAt: ago(9_100), lastReportedAt: ago(700), silent: true }),
  session({ id: 's-quiet-3', state: 'active', startedAt: ago(300), lastReportedAt: ago(12), silent: true }),
  session({ id: 's-direct-today', state: 'closed', origin: 'direct', startedAt: ago(200), endedAt: ago(190), lastReportedAt: ago(190), silent: false, riskFlags: ['bypassed-gateway'] }),
  // Older than the window. If the "Bypassed gateway" link ever loses its 24h
  // scope, this row is what makes the test notice.
  session({ id: 's-direct-old', state: 'closed', origin: 'direct', startedAt: ago(9_000), endedAt: ago(8_990), lastReportedAt: ago(8_990), silent: false, riskFlags: ['bypassed-gateway'] }),
  session({ id: 's-closed-1', state: 'closed', startedAt: ago(400), endedAt: ago(360), lastReportedAt: ago(360), silent: false }),
  session({ id: 's-closed-2', state: 'closed', startedAt: ago(800), endedAt: ago(790), lastReportedAt: ago(790), silent: false }),
  // Ended at a time nobody recorded: `endedAt` was once written only inside the
  // seal branch, so a terminated session with no recorder kept a null. 26 of
  // these in dev, every one rendering as still running.
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
  asset({ hostname: 'open-absent-1', agentState: 'absent', bypassPosture: 'open' }),
  asset({ hostname: 'open-absent-2', agentState: 'absent', bypassPosture: 'open' }),
  asset({ hostname: 'open-stale-1', agentState: 'stale', bypassPosture: 'open',
    hostKeyState: 'unpinned' }),
  // Two different not-pinned states. A single-state filter can select neither
  // set the "Unverified hosts" tile counts, which is why `unverified` exists.
  asset({ hostname: 'monitored-absent', agentState: 'absent', bypassPosture: 'monitored',
    hostKeyState: 'changed' }),
  asset({ hostname: 'monitored-stale', agentState: 'stale', bypassPosture: 'monitored' }),
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
  standingCredentialAssets: 0,
  sessionsDirectToday: countSessions((s) => s.origin === 'direct' && within24h(s)),
}

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
  for await (const c of req) chunks.push(c)
  try { return JSON.parse(Buffer.concat(chunks).toString() || '{}') } catch { return {} }
}

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
    return signedIn(req)
      ? json(res, 200, {
          authenticated: true, email: 'dev@northwind.id',
          displayName: 'Dev Admin', role: 'admin', mfaEnrolled: false,
        })
      : json(res, 401, {
          authenticated: false, passwordEnabled: true, accountsExist: true,
        })
  }
  if (path.startsWith('/api/')) {
    if (!signedIn(req)) return json(res, 401, { error: 'unauthorized' })
    if (path === '/api/v1/stats') return json(res, 200, STATS)
    if (path === '/api/v1/coverage') return json(res, 200, COVERAGE)
    if (path === '/api/v1/assets') return json(res, 200, ASSETS)
    if (path === '/api/v1/requests') {
      const want = url.searchParams.get('state')
      return json(res, 200, want ? REQUESTS.filter((r) => r.state === want) : REQUESTS)
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
