import { test, expect, type Page } from '@playwright/test'
import { b64urlDecode, b64urlEncode, oneTimeKey, openSealed, readAAD, sealTo, stdB64Decode, stdB64Encode, writeAAD } from '../src/mf-secrets/seal'

/**
 * The Secrets page, against e2e/authstub.mjs, with argus-control's secrets
 * API answered by the test. What is checked is what only a browser shows:
 * the section is the separately built bundle (/mf/secrets/1/index.js), it
 * draws inside the console with the console's theme, and a value is sealed
 * and opened in the tab, never sent or received in the clear. Which reads
 * are allowed is argus-secret's to decide (internal/vault).
 */
test.use({ baseURL: 'http://localhost:5511', serviceWorkers: 'block' })

const now = new Date().toISOString()
const SECRETS = {
  secrets: [
    { name: 'prod/db', type: 'kv', versions: [{ n: 1, createdAt: now, createdBy: 'dev@northwind.id', fields: ['password', 'user'] }],
      labels: { current: 1 }, createdAt: now, createdBy: 'dev@northwind.id', updatedAt: now, server: 'Core', serverId: 's-core' },
    { name: 'edge/api-key', type: 'text', versions: [{ n: 1, createdAt: now, createdBy: 'dev@northwind.id' }],
      labels: { current: 1 }, createdAt: now, createdBy: 'dev@northwind.id', updatedAt: now, server: 'Edge', serverId: 's-edge' },
  ],
  unreachable: [{ server: 'Branch', error: 'Branch is connected to argus-control replica b, not this one: try again' }],
}

async function open(page: Page) {
  const sent: { method: string; path: string; body: Record<string, unknown> | null }[] = []
  // argus-secret's key for prod/, as the seal-key route would give it.
  const server = await oneTimeKey()
  const written: string[] = []
  await page.route('**/api/v1/secrets**', async (r) => {
    const req = r.request()
    const path = new URL(req.url()).pathname
    const body = req.postData() ? JSON.parse(req.postData()!) as Record<string, unknown> : null
    sent.push({ method: req.method(), path, body })
    if (path === '/api/v1/secrets') return r.fulfill({ json: SECRETS })
    if (path === '/api/v1/secrets/read') {
      // Sealed to the tab's one-time key, as argus-secret seals it.
      const sealed = await sealTo(b64urlDecode(String(body!.recipient)), new TextEncoder().encode('hunter2'),
        readAAD(String(body!.name), 1, String(body!.field ?? '')))
      return r.fulfill({ json: { name: body!.name, version: 1, field: body!.field, sealed: stdB64Encode(sealed) } })
    }
    if (path === '/api/v1/secrets/seal-key') return r.fulfill({ json: { server: 'Core', sealKey: b64urlEncode(server.publicKey) } })
    if (path === '/api/v1/secrets/item' && req.method() === 'PUT') {
      const plain = await openSealed(server, stdB64Decode(String(body!.sealedValue)), writeAAD(String(body!.name)))
      written.push(new TextDecoder().decode(plain))
      return r.fulfill({ json: { ...SECRETS.secrets[1], name: body!.name } })
    }
    return r.fulfill({ json: [] })
  })
  const bundle = page.waitForResponse((res) => new URL(res.url()).pathname === '/mf/secrets/1/index.js')
  await page.goto('/')
  await page.getByLabel(/^email$/i).fill('dev@northwind.id')
  await page.getByLabel(/^password$/i).fill('stub-password')
  await page.getByRole('button', { name: /^sign in$/i }).click()
  await expect(page.getByRole('button', { name: /^sign in$/i })).toBeHidden()
  await page.goto('/secrets')
  const res = await bundle
  expect(res.status()).toBe(200)
  expect(res.headers()['content-type']).toContain('javascript')
  return { sent, written }
}

test('secrets: the section loads from its own bundle, and values are sealed both ways', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  // The section's own failures only. Around it, the stub's alert stream and
  // the console's new-build check fail in WebKit ("access control checks"),
  // as they do on every page served by the stub; they are not this page's.
  // A section that failed to load shows the shell's Unavailable alert, which
  // is checked below as well.
  const errors: string[] = []
  page.on('pageerror', (e) => { if (/mf\/secrets|mf-secrets|seal/i.test(`${e.message} ${e.stack ?? ''}`)) errors.push(e.message) })
  page.on('requestfailed', (r) => { if (r.url().includes('/mf/')) errors.push(`failed: ${r.url()}`) })
  const { sent, written } = await open(page)

  await expect(page.getByRole('heading', { level: 1, name: 'Secrets' })).toHaveCount(1)
  await expect(page.getByRole('row', { name: /prod\/db/ })).toBeVisible()
  await expect(page.getByText('Branch did not answer')).toBeVisible()
  // An admin sees all four tabs; the section draws with the console's theme.
  for (const name of ['Secrets', 'Requests', 'Rules', 'App tokens']) await expect(page.getByRole('tab', { name })).toBeVisible()
  const tabFont = await page.getByRole('tab', { name: 'Secrets' }).evaluate((el) => getComputedStyle(el).fontFamily)
  const shellFont = await page.evaluate(() => getComputedStyle(document.body).fontFamily)
  expect(tabFont).toBe(shellFont)

  // Reading: a reason first, then the value, opened here.
  await page.getByRole('row', { name: /prod\/db/ }).getByRole('button', { name: 'Open' }).click()
  const dialog = page.getByRole('dialog')
  await dialog.getByLabel('Why do you need it?').fill('rotating the replica')
  await dialog.getByRole('button', { name: 'Show value' }).click()
  await expect(dialog.getByText('hunter2')).toBeVisible()
  const read = sent.find((s) => s.path === '/api/v1/secrets/read')!
  expect(read.body).toMatchObject({ name: 'prod/db', field: 'password', reason: 'rotating the replica' })
  expect(String(read.body!.recipient)).toMatch(/^[A-Za-z0-9_-]{43}$/)
  await dialog.getByRole('button', { name: 'Close' }).click()

  // Writing: sealed in the tab to the server's key; the body has no value.
  await page.getByRole('button', { name: 'New secret' }).click()
  const form = page.getByRole('dialog', { name: 'New secret' })
  await form.getByRole('textbox', { name: 'Name' }).fill('prod/payments/key')
  await form.getByRole('textbox', { name: 'Value' }).fill('correct horse battery staple')
  await form.getByLabel('Why?').fill('new payments provider')
  await form.getByRole('button', { name: 'Save' }).click()
  await expect(form).toBeHidden()
  const put = sent.find((s) => s.path === '/api/v1/secrets/item')!
  expect(JSON.stringify(put.body)).not.toContain('correct horse')
  expect(written).toEqual(['correct horse battery staple'])

  await expect(page.getByText('The secrets screens did not load')).toHaveCount(0)
  expect(errors).toEqual([])
})
