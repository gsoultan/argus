import { test, expect, type Page } from '@playwright/test'
import { TELEGRAM } from './telegramFixture'

/**
 * Websites, against e2e/authstub.mjs with the checks served by the test: the
 * list with each site's state and uptime, one site's day of rounds and its
 * outages, and the form. What counts as down is the control plane's to
 * decide (internal/control/websites.go); this is what the page says about it.
 */
test.use({ baseURL: 'http://localhost:5511', serviceWorkers: 'block' })

const SHOTS = process.env.ARGUS_SHOTS
const now = Date.now()
const ago = (m: number) => new Date(now - m * 60_000).toISOString()

const base = {
  expectStatus: 0, keyword: '', everySeconds: 60, timeoutSeconds: 30, paused: false, createdBy: 'dev@northwind.id',
  createdAt: ago(60 * 24 * 40), failingRounds: 0, lastOutcome: 'up', confirmFrom: [] as string[], confirmHosts: [] as string[],
  telegram: 'default', telegramBot: '', telegramChats: [] as string[],
}

const LIST = [
  { ...base, id: 'w1', name: 'Shop', url: 'https://shop.northwind.id/health', keyword: 'ok', state: 'up', stateSince: ago(60 * 26),
    lastAt: ago(0.5), lastMs: 182, lastStatus: 200, lastDetail: 'answered 200 OK', certNotAfter: new Date(now + 64 * 86_400_000).toISOString(),
    confirmFrom: ['a1', 'a2'], confirmHosts: ['web-sg-01', 'web-fra-01'],
    uptime: { day: 100, week: 99.98, month: 99.95 } },
  { ...base, id: 'w2', name: 'Payments API', url: 'https://pay.northwind.id/v1/ping', state: 'down', stateSince: ago(7),
    lastAt: ago(0.4), lastMs: 30_000, lastDetail: 'no answer in 30s (3 tries from Argus; also failed from web-sg-01)',
    confirmFrom: ['a1'], confirmHosts: ['web-sg-01'], telegram: 'chats', telegramBot: 'b-pay', telegramChats: ['-100payments'],
    uptime: { day: 99.51, week: 99.93, month: 99.97 },
    openOutage: { id: 9, checkId: 'w2', startedAt: ago(7), cause: 'no answer in 30s', confirmed: 'Argus, web-sg-01' } },
  { ...base, id: 'w3', name: 'Docs', url: 'https://docs.northwind.id', state: 'up', stateSince: ago(60 * 3),
    lastAt: ago(0.2), lastMs: 96, lastStatus: 200, failingRounds: 0, lastOutcome: 'partial',
    lastDetail: 'Argus could not reach it (connection refused), but web-fra-01 could: answered 200 OK',
    uptime: { day: 100, week: 100, month: 100 } },
  { ...base, id: 'w5', name: 'Blog', url: 'https://blog.northwind.id', state: 'up', stateSince: ago(60 * 50), lastAt: ago(0.1),
    lastMs: 30_000, failingRounds: 1, lastOutcome: 'pending', lastDetail: 'no answer in 30s (3 tries from Argus)',
    uptime: { day: 100, week: 100, month: 99.99 } },
  { ...base, id: 'w4', name: 'Intranet', url: 'http://10.0.8.20/', state: 'unknown', stateSince: ago(2), lastAt: ago(0.3),
    lastDetail: 'Argus could not check: its own network is not reaching the internet right now (DNS did not answer for intranet)',
    uptime: { day: 100, week: 100, month: null } },
]

const rounds = Array.from({ length: 1440 }, (_, i) => {
  const m = 1440 - i
  const down = m <= 7 || (m > 600 && m <= 612)
  return { at: ago(m), secs: 60, outcome: down ? 'down' : m === 300 ? 'unknown' : 'up',
    ms: down ? 30_000 : 140 + Math.round(60 * Math.sin(i / 40)) + (i % 17), detail: down ? 'no answer in 30s' : 'answered 200 OK' }
})

const DETAIL = {
  check: LIST[1],
  rounds,
  outages: [
    { id: 9, checkId: 'w2', startedAt: ago(7), cause: 'no answer in 30s', confirmed: 'Argus, web-sg-01' },
    { id: 8, checkId: 'w2', startedAt: ago(612), endedAt: ago(600), cause: 'answered 502 Bad Gateway', confirmed: 'Argus, web-sg-01' },
  ],
}

async function signIn(page: Page) {
  await page.route('**/api/v1/websites', (r) => r.fulfill({ json: LIST }))
  await page.route('**/api/v1/websites/w2', (r) => r.fulfill({ json: DETAIL }))
  await page.route('**/api/v1/telegram', (r) => r.fulfill({ json: TELEGRAM }))
  await page.route('**/api/v1/websites/w2/test-alert', (r) => r.fulfill({ json: { bot: 'Payments team', chats: ['-100payments'] } }))
  await page.route('**/api/v1/websites/test', (r) => r.fulfill({ json: { ok: true, status: 200, detail: 'answered 200 OK', ms: 211 } }))
  await page.goto('/')
  await page.getByLabel(/^email$/i).fill('dev@northwind.id')
  await page.getByLabel(/^password$/i).fill('stub-password')
  await page.getByRole('button', { name: /^sign in$/i }).click()
  await expect(page.getByRole('button', { name: /^sign in$/i })).toBeHidden()
}

test('websites: state, uptime, and how sure down is', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1100 })
  await signIn(page)
  await page.goto('/websites')
  await expect(page.getByRole('heading', { name: 'Websites' })).toBeVisible()
  await expect(page.getByText('1 down')).toBeVisible()
  const pay = page.getByRole('row', { name: /Payments API/ })
  await expect(pay.getByText('Down')).toBeVisible()
  await expect(pay.getByText('99.51%')).toBeVisible()
  // Reached by a chosen server but not by Argus is up, with a note.
  await expect(page.getByRole('row', { name: /Docs/ }).getByText('Up')).toBeVisible()
  await expect(page.getByRole('row', { name: /Docs/ }).getByText('Argus cannot reach it; another server can')).toBeVisible()
  // A failure not yet confirmed is not shown as down.
  await expect(page.getByRole('row', { name: /Blog/ }).getByText('Checking again')).toBeVisible()
  // Argus's own network failing is "Cannot check", never down.
  await expect(page.getByRole('row', { name: /Intranet/ }).getByText('Cannot check')).toBeVisible()
  await expect(page.getByText('How Argus makes sure a site is really down')).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/01-websites.png`, fullPage: true })

  await pay.getByRole('button', { name: 'Open Payments API' }).click()
  const drawer = page.getByRole('dialog')
  await expect(drawer.getByText(/^Down since/)).toBeVisible()
  await expect(drawer.getByText('Confirmed by Argus, web-sg-01').first()).toBeVisible()
  await expect(drawer.getByText('12 min')).toBeVisible()
  await expect(drawer.getByText(/asks web-sg-01 to check too/)).toBeVisible()
  await expect(drawer.getByText('Alerts go to the bell and, through Payments team, to Payments.')).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/02-website-down.png` })
  await drawer.getByRole('button', { name: 'Send test alert' }).click()
  await expect(page.getByText('Payments team posted a test alert to 1 chat.')).toBeVisible()
})

test('websites: Telegram for the whole feature and for one site', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1100 })
  await signIn(page)
  await page.goto('/websites')
  await page.getByRole('button', { name: 'Telegram alerts' }).click()
  const modal = page.getByRole('dialog', { name: 'Website alerts' })
  await expect(modal.getByLabel('Bot', { exact: true })).toHaveValue('Ops alerts')
  await expect(modal.getByText('Ops on-call')).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/05-website-alerts.png` })
  await modal.getByRole('button', { name: 'Cancel' }).click()

  await page.getByRole('button', { name: 'Add website' }).first().click()
  const form = page.getByRole('dialog')
  await expect(form.getByLabel('Telegram', { exact: true })).toHaveValue('Same as all websites (Ops alerts, 1 chat)')
  await form.getByLabel('Telegram', { exact: true }).click()
  await page.getByRole('option', { name: 'Its own bot and chats' }).click()
  await form.getByLabel('Bot', { exact: true }).click()
  await page.getByRole('option', { name: 'Payments team' }).click()
  await form.getByLabel('Chats', { exact: true }).click()
  await page.getByRole('option', { name: 'Payments' }).click()
  await expect(form.getByText('Only the chats set on this bot.')).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/06-website-telegram.png` })
})

test('websites: adding one, with a test first', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1100 })
  await signIn(page)
  await page.goto('/websites')
  await page.getByRole('button', { name: 'Add website' }).first().click()
  const form = page.getByRole('dialog')
  await form.getByLabel('Address').fill('https://status.northwind.id')
  await form.getByRole('button', { name: 'Test now' }).click()
  await expect(form.getByText(/Up from Argus: answered 200 OK in 211 ms/)).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/03-add-website.png` })
})

test('websites: on a phone', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await signIn(page)
  await page.goto('/websites')
  await expect(page.getByText('1 down')).toBeVisible()
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)
  expect(overflow).toBeLessThanOrEqual(0)
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/04-websites-phone.png`, fullPage: true })
})
