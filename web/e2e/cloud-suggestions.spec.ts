import { test, expect, type Page } from '@playwright/test'

/**
 * Suggestions from Argus's own metrics, against e2e/authstub.mjs with the
 * suggestions served by the test: a busy server behind a load balancer on
 * its Overview and Metrics tabs, the list on Cloud › Scaling, a resize
 * opening with the size filled in, and Not now.
 */
test.use({ baseURL: 'http://localhost:5511', serviceWorkers: 'block' })

const SHOTS = process.env.ARGUS_SHOTS

const instance = (id: string, name: string, size: string, state = 'running') => ({
  accountId: 'ca1', accountName: 'payments-prod', environment: 'production', firstSeen: '2026-01-01T00:00:00Z', seenAt: new Date().toISOString(),
  madeByArgus: false, resource: { kind: 'instance', id, name, region: 'ap-southeast-3', state, size },
})

const ADD = {
  kind: 'add-server', assetId: 'a-closed-healthy', hostname: 'web-01', title: 'web-01 has been over 80% processor for 9 of the last 24 hours',
  detail: 'It is behind web-lb. Another m6i.large behind it would share the load, for about $70.08 a month.',
  instance: instance('i-0a41c27b9e3f1d27d', 'web-01', 'm6i.large'), balancer: 'web-lb', targetGroup: 'arn:tg/web', monthly: 70.08,
  load: { busyHours: 9, hours: 24, peakCpu: 93, peakMem: 61 },
}
const UP = {
  kind: 'resize-up', assetId: 'a-batch', hostname: 'batch-02', title: 'batch-02 has been over 80% processor for 7 of the last 24 hours',
  detail: 'It is not behind a load balancer, so one bigger server is simpler than two: resize it to m6i.xlarge, about $70.08 a month more. Resizing stops and starts it.',
  instance: instance('i-0batch', 'batch-02', 'm6i.large', 'stopped'), size: 'm6i.xlarge', monthly: 70.08,
  load: { busyHours: 7, hours: 24, peakCpu: 97, peakMem: 40 },
}
const DOWN = {
  kind: 'resize-down', assetId: 'a-etl', hostname: 'etl-runner-2', title: 'etl-runner-2 has been idle for 14 days',
  detail: 'Its processor stayed under 10% and memory under 60% every hour. m6i.large would do and saves about $70.08 a month. Resizing stops and starts it.',
  instance: instance('i-0etl', 'etl-runner-2', 'm6i.xlarge'), size: 'm6i.large', monthly: -70.08,
  load: { busyHours: 0, hours: 24, peakCpu: 6, peakMem: 30, days: 14 },
}

async function signIn(page: Page) {
  await page.route('**/api/v1/assets/*/suggestion', (r) => r.fulfill({ json: { suggestion: r.request().url().includes('a-closed-healthy') ? ADD : null } }))
  await page.route('**/api/v1/cloud/suggestions', (r) => r.fulfill({ json: { suggestions: [UP, ADD, DOWN] } }))
  await page.route('**/api/v1/cloud/launch-options?*', (r) => r.fulfill({ json: {
    images: [], sizes: ['m6i.large', 'm6i.2xlarge'], subnets: [], firewalls: [], environment: 'production', needsApproval: true } }))
  await page.goto('/')
  await page.getByLabel(/^email$/i).fill('dev@northwind.id')
  await page.getByLabel(/^password$/i).fill('stub-password')
  await page.getByRole('button', { name: /^sign in$/i }).click()
  await expect(page.getByText('Dev Admin')).toBeVisible()
}

test('a busy server behind a load balancer suggests another server', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  await signIn(page)
  await page.goto('/assets/a-closed-healthy')
  const banner = page.getByTestId('suggestion')
  await expect(banner).toContainText('web-01 has been over 80% processor for 9 of the last 24 hours')
  await expect(banner).toContainText('for about $70.08 a month')
  await page.getByRole('tab', { name: 'Metrics' }).click()
  await expect(page.getByTestId('suggestion')).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/17-suggestion-metrics.png` })
  await page.getByRole('button', { name: 'Add a server behind web-lb' }).click()
  await expect(page).toHaveURL(/\/cloud\/scale\/new\?.*from=i-0a41c27b9e3f1d27d/)
})

test('Not now hides it for a day', async ({ page }) => {
  await signIn(page)
  await page.goto('/assets/a-closed-healthy')
  await page.getByTestId('suggestion').getByRole('button', { name: 'Not now' }).click()
  await expect(page.getByTestId('suggestion')).toBeHidden()
  await page.reload()
  await expect(page.getByText('Dev Admin')).toBeVisible()
  await expect(page.getByTestId('suggestion')).toBeHidden()
})

test('Cloud › Scaling lists every suggestion, and a resize opens with its size', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  await signIn(page)
  await page.goto('/cloud?tab=scaling')
  const list = page.getByTestId('suggestions')
  await expect(list).toContainText('batch-02 has been over 80% processor')
  await expect(list).toContainText('etl-runner-2 has been idle for 14 days')
  await expect(list.getByRole('button', { name: 'Add a server behind web-lb' })).toBeVisible()
  if (SHOTS) await page.screenshot({ path: `${SHOTS}/18-suggestions-scaling.png` })
  await list.getByRole('button', { name: 'Resize to m6i.xlarge' }).click()
  const dialog = page.getByRole('dialog', { name: 'Change the size of batch-02' })
  await expect(dialog.getByRole('textbox', { name: 'New size' }).or(dialog.getByRole('combobox', { name: 'New size' })).first()).toHaveValue('m6i.xlarge')
})
