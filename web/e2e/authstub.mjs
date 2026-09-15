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
const TYPES = {
  '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css',
  '.json': 'application/json', '.svg': 'image/svg+xml', '.woff2': 'font/woff2',
  '.png': 'image/png', '.ico': 'image/x-icon', '.wasm': 'application/wasm',
}

const STATS = {
  assetsTotal: 0, assetsUnreachable: 0, hostKeysUnpinned: 0, sessionsActive: 0,
  sessionsToday: 0, requestsPending: 0, credentialsOverdue: 0,
  standingCredentialAssets: 0, sessionsDirectToday: 0,
}

const json = (res, code, body, headers = {}) => {
  const payload = JSON.stringify(body)
  res.writeHead(code, {
    'content-type': 'application/json',
    'content-length': Buffer.byteLength(payload),
    ...headers,
  })
  res.end(payload)
}

async function readBody(req) {
  const chunks = []
  for await (const c of req) chunks.push(c)
  try { return JSON.parse(Buffer.concat(chunks).toString() || '{}') } catch { return {} }
}

const signedIn = (req) => (req.headers.cookie ?? '').includes(`argus_session=${SESSION}`)

const server = createServer(async (req, res) => {
  const url = new URL(req.url, `http://localhost:${PORT}`)
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
    if (path === '/api/v1/coverage') {
      return json(res, 200, { assets: [], certificateAuth: 0, total: 0 })
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
