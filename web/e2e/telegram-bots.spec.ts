import { test, expect, type Page } from '@playwright/test'
import { TELEGRAM } from './telegramFixture'

/**
 * Settings › Telegram bots, against e2e/authstub.mjs with the bots served by
 * the test: the list, adding one (token checked, chats found), and choosing
 * a bot for each kind of alert. What reaches Telegram is the control
 * plane's to decide (internal/control/telegram_bots.go).
 */
test.use({ baseURL: 'http://localhost:5511', serviceWorkers: 'block' })

const SHOTS = process.env.ARGUS_SHOTS

const view = () => {
  const v = structuredClone(TELEGRAM) as typeof TELEGRAM & { bots: Record<string, unknown>[]; platform?: unknown }
  v.bots.push({
    id: 'b-mkt', name: 'Old marketing bot', username: 'nw_mkt_bot', tokenTail: 'x9P',
    chats: [{ id: '-100mkt', title: 'Marketing' }], default: false, createdBy: 'dev@northwind.id',
    createdAt: new Date(Date.now() - 40 * 86_400_000).toISOString(), updatedAt: new Date().toISOString(), usedBy: [],
    lastError: 'telegram refused the message: Unauthorized', lastErrorAt: new Date(Date.now() - 3 * 86_400_000).toISOString(),
  })
  v.platform = { chats: [{ id: '-100plat', title: '-100plat' }], usedBy: [] }
  return v
}

async function open(page: Page) {
  const sent: { method: string; url: string; body: unknown }[] = []
  await page.route('**/api/v1/telegram**', async (r) => {
    const req = r.request()
    const url = new URL(req.url())
    const body = req.postData() ? JSON.parse(req.postData()!) : null
    sent.push({ method: req.method(), url: url.pathname, body })
    if (url.pathname === '/api/v1/telegram') return r.fulfill({ json: view() })
    if (url.pathname === '/api/v1/telegram/check') return r.fulfill({ json: { username: 'nw_support_bot', name: 'Support' } })
    if (url.pathname === '/api/v1/telegram/chats') {
      return r.fulfill({ json: { chats: [{ id: '-1002231004418', title: 'Support desk', kind: 'supergroup' }, { id: '77', title: 'Dewi', kind: 'private' }] } })
    }
    if (url.pathname.endsWith('/test')) return r.fulfill({ json: { bot: 'Ops alerts', chats: ['-100ops', '-100infra'] } })
    if (req.method() === 'POST' && url.pathname === '/api/v1/telegram/bots') return r.fulfill({ status: 201, json: { ...TELEGRAM.bots[1], id: 'b-new' } })
    return r.fulfill({ json: view() })
  })
  await page.goto('/')
  await page.getByLabel(/^email$/i).fill('dev@northwind.id')
  await page.getByLabel(/^password$/i).fill('stub-password')
  await page.getByRole('button', { name: /^sign in$/i }).click()
  await expect(page.getByRole('button', { name: /^sign in$/i })).toBeHidden()
  await page.goto('/settings?section=telegram')
  // Notifications opens on where each alert goes; the bots are the next tab.
  await expect(page.getByRole('tab', { name: 'Where alerts go', selected: true })).toBeVisible()
  await page.getByRole('tab', { name: /^Bots/ }).click()
  await expect(page.getByText('Bots that send this organization')).toBeVisible()
  return sent
}

test('telegram bots: the list, and adding one', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  const sent = await open(page)
  const ops = page.getByRole('row', { name: /Ops alerts/ })
  await expect(ops.getByText('Default')).toBeVisible()
  await expect(ops.getByText('Ops on-call')).toBeVisible()
  await expect(ops.getByText('Sign-in alerts, Server connected, Websites, Backups')).toBeVisible()
  await expect(page.getByRole('row', { name: /Old marketing bot/ }).getByText('Failed')).toBeVisible()
  await expect(page.getByRole('tabpanel', { name: /^Bots/ }).getByText('Platform bot', { exact: true })).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/07-telegram-bots.png`, fullPage: true })

  await ops.getByRole('button', { name: 'Test' }).click()
  await expect(page.getByText('Ops alerts posted a test message to 2 chats.')).toBeVisible()

  await page.getByRole('button', { name: 'Add bot' }).click()
  const form = page.getByRole('dialog', { name: 'Add a Telegram bot' })
  await form.getByLabel('Bot token').fill('7091534211:AAFsupportsupportsupportsupport')
  await form.getByRole('button', { name: 'Check' }).click()
  await expect(form.getByText('Works: this is @nw_support_bot')).toBeVisible()
  await expect(form.getByLabel('Name')).toHaveValue('Support')
  await form.getByRole('button', { name: 'Find chats' }).click()
  await form.getByLabel(/Support desk/).check()
  await expect(form.getByRole('switch', { name: /Default bot/ })).not.toBeChecked()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/08-add-telegram-bot.png` })
  await form.getByRole('button', { name: 'Add bot' }).click()
  await expect(form).toBeHidden()
  const added = sent.find((s) => s.method === 'POST' && s.url === '/api/v1/telegram/bots')
  expect(added?.body).toMatchObject({
    name: 'Support', token: '7091534211:AAFsupportsupportsupportsupport', default: false,
    chats: [{ id: '-1002231004418', title: 'Support desk', kind: 'supergroup' }],
  })
})

test('telegram bots: which bot each alert uses', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  const sent = await open(page)
  await page.getByRole('tab', { name: 'Where alerts go' }).click()
  await expect(page.getByRole('combobox', { name: 'Bot for Sign-in alerts' })).toHaveValue('Default bot (Ops alerts)')
  await expect(page.getByRole('combobox', { name: 'Bot for Server connected' })).toHaveValue('Ops alerts')
  // Backups never offer the platform bot: its channels already have them.
  await page.getByRole('combobox', { name: 'Bot for Backups' }).click()
  await expect(page.getByRole('option', { name: 'Platform bot' })).toHaveCount(0)
  await page.keyboard.press('Escape')
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/09-telegram-routes.png` })

  await page.getByRole('combobox', { name: 'Bot for Websites' }).click()
  await page.getByRole('option', { name: 'Payments team' }).click()
  await expect.poll(() => sent.find((s) => s.method === 'PUT' && s.url === '/api/v1/telegram/routes/website')?.body)
    .toEqual({ bot: 'b-pay', chats: [] })
})

test('telegram bots: on a phone', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  await open(page)
  await page.getByRole('button', { name: 'Add bot' }).click()
  await expect(page.getByTestId('telegram-chat-help')).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/telegram-steps-phone.png` })
  await page.keyboard.press('Escape')
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)
  expect(overflow).toBeLessThanOrEqual(0)
})

test("telegram bots: a group's chat ID is explained, and found as soon as the group writes", async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  await open(page)
  // Telegram has not heard from the group yet; it has on the third look.
  let looks = 0
  await page.route('**/api/v1/telegram/chats', (r) => {
    looks++
    return r.fulfill({ json: { chats: looks < 3 ? [] : [{ id: '-1002231004418', title: 'Support desk', kind: 'supergroup' }] } })
  })
  await page.getByRole('button', { name: 'Add bot' }).click()
  const form = page.getByRole('dialog', { name: 'Add a Telegram bot' })
  // With no chats yet the steps are open; the token's are one click away.
  const steps = form.getByTestId('telegram-chat-help')
  await expect(steps).toBeVisible()
  await expect(steps.getByText(/Add members/)).toBeVisible()
  await form.getByRole('button', { name: 'How do I get a bot token?' }).click()
  await expect(form.getByTestId('telegram-token-help').getByText('/newbot')).toBeVisible()
  await expect(form.getByText('Paste the bot token first')).toBeVisible()

  await form.getByLabel('Bot token').fill('7091534211:AAFsupportsupportsupportsupport')
  await form.getByRole('button', { name: 'Check' }).click()
  // The message to send names this bot, ready to copy.
  await expect(steps.getByText('/start@nw_support_bot')).toBeVisible()
  await form.getByRole('button', { name: "Channels, private chats, and when it is not found" }).click()
  await expect(form.getByTestId('telegram-chat-help-more').getByText(/upgraded to a supergroup/)).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/telegram-chat-steps.png` })

  await form.getByRole('button', { name: 'Find chats' }).click()
  await expect(form.getByTestId('telegram-watching')).toBeVisible()
  if (SHOTS) await form.getByTestId('telegram-watching').scrollIntoViewIfNeeded()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/telegram-watching.png` })
  await expect(form.getByLabel(/Support desk/)).toBeChecked({ timeout: 15_000 })
  await expect(form.getByTestId('telegram-watching')).toBeHidden()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/telegram-found.png` })
})
