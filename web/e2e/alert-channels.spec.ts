import { test, expect } from '@playwright/test'

/**
 * Setting up email and Telegram alerts from Settings, against e2e/authstub.mjs.
 *
 * The bug this exists for: "Set up Email" only picked the Email tab, which was
 * already the open one, and the mail server fields stayed hidden behind a
 * switch that was off. The button did nothing anyone could see, and there was
 * no form to fill in.
 */
test.use({ baseURL: 'http://localhost:5511', serviceWorkers: 'block' })

async function openAlerts(page: import('@playwright/test').Page) {
  await page.goto('/')
  await page.getByLabel(/^email$/i).fill('dev@northwind.id')
  await page.getByLabel(/^password$/i).fill('stub-password')
  await page.getByRole('button', { name: /^sign in$/i }).click()
  await expect(page.getByText('Dev Admin')).toBeVisible()
  await page.goto('/settings?section=alerts')
  await expect(page.getByRole('heading', { name: 'Alert channels' })).toBeVisible()
}

test('"Set up Email" opens the mail server form', async ({ page }) => {
  await openAlerts(page)
  await expect(page.getByRole('textbox', { name: 'SMTP server' })).toBeHidden()

  await page.getByRole('button', { name: 'Set up Email' }).click()

  const host = page.getByRole('textbox', { name: 'SMTP server' })
  await expect(host).toBeVisible()
  await expect(host).toBeFocused()
  await expect(page.getByRole('switch', { name: 'Send email' })).toBeChecked()
  await expect(page.getByLabel('From')).toBeVisible()
  // Nothing is saved behind the operator's back: the draft is marked unsaved.
  await expect(page.getByText('unsaved').first()).toBeVisible()
})

test('"Set up Telegram" opens the bot form', async ({ page }) => {
  await openAlerts(page)

  await page.getByRole('button', { name: 'Set up Telegram' }).click()

  const token = page.getByLabel('Bot token')
  await expect(token).toBeVisible()
  await expect(token).toBeFocused()
  await expect(page.getByLabel('Send to Telegram')).toBeChecked()
  await expect(page.getByLabel('Chat ids').first()).toBeVisible()
})

test('email can go through Panmail, and a test send says where it went', async ({ page }) => {
  await openAlerts(page)
  await page.getByRole('button', { name: 'Set up Email' }).click()

  await page.getByText('Panmail', { exact: true }).click()
  await expect(page.getByRole('textbox', { name: 'SMTP server' })).toBeHidden()
  await page.getByLabel('Panmail address').fill('https://mail.northwind.id')
  await page.getByLabel('Provider id').fill('0f8b8f4e-0000-4000-8000-000000000000')
  await page.getByRole('textbox', { name: 'From', exact: true }).fill('argus@northwind.id')
  const to = page.getByRole('combobox', { name: 'To', exact: true })
  await to.fill('sec@northwind.id')
  await to.press('Enter')

  // Without the key the form says so, and the test button waits for it.
  await expect(page.getByText(/Still needed: API key/)).toBeVisible()
  await expect(page.getByRole('button', { name: 'Send test email' })).toBeDisabled()

  await page.getByLabel('API key').fill('pm_test_key')
  await page.getByRole('button', { name: 'Send test email' }).click()
  await expect(page.getByText('Test email sent')).toBeVisible()
  await expect(page.getByText(/Panmail queued it for sec@northwind.id/)).toBeVisible()
})

test('the platform bot finds its group and sends a test before saving', async ({ page }) => {
  const sent: { url: string; body: unknown }[] = []
  await page.route('**/api/v1/settings/telegram-chats', (r) => {
    sent.push({ url: 'chats', body: JSON.parse(r.request().postData() ?? '{}') })
    return r.fulfill({ json: { chats: [{ id: '-1009988776655', title: 'Ops on-call', kind: 'supergroup' }, { id: '42', title: 'Dewi', kind: 'private' }] } })
  })
  await page.route('**/api/v1/settings/test-telegram', (r) => {
    const body = JSON.parse(r.request().postData() ?? '{}') as { chatIds: string[] }
    sent.push({ url: 'test', body })
    return r.fulfill({ json: { chats: body.chatIds } })
  })
  await openAlerts(page)
  await page.getByRole('button', { name: 'Set up Telegram' }).click()

  await expect(page.getByTestId('telegram-token-help').getByText('/newbot')).toBeVisible()
  await expect(page.getByTestId('telegram-chat-help').getByText(/Add members/)).toBeVisible()
  await expect(page.getByRole('button', { name: 'Find chats' })).toBeDisabled()

  await page.getByLabel('Bot token').fill('7091534211:AAFplatformplatformplatform')
  await page.getByRole('button', { name: 'Find chats' }).click()
  await page.getByTestId('platform-telegram-found').getByLabel(/Ops on-call/).check()
  await expect(page.getByRole('combobox', { name: 'Chat ids' }).locator('..').getByText('-1009988776655')).toBeVisible()
  await page.getByRole('button', { name: 'Send test message' }).click()
  await expect(page.getByText('Test message sent')).toBeVisible()
  expect(sent).toEqual([
    { url: 'chats', body: { botToken: '7091534211:AAFplatformplatformplatform' } },
    { url: 'test', body: { botToken: '7091534211:AAFplatformplatformplatform', chatIds: ['-1009988776655'] } },
  ])
  if (process.env.ARGUS_SHOTS) { await page.setViewportSize({ width: 1440, height: 1100 }); await page.getByRole('heading', { name: 'Alert channels' }).evaluate((e) => e.scrollIntoView()); await page.screenshot({ path: `${process.env.ARGUS_SHOTS}/platform-telegram.png` }) }
})
