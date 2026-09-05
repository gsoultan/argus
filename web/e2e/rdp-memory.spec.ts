import { test, expect } from '@playwright/test'
import { firstSessionPath, heapMB, watchErrors } from './helpers'

/**
 * The desktop player uses the same design as the terminal one -- the worker
 * owns the recording, the main thread receives finished bitmaps and closes
 * each one the moment it is drawn -- but pixels are heavier than text, so it
 * is the likelier of the two to leak. Same assertion: the main-thread heap
 * must not track the size of what is played.
 *
 * Stream layout (see rdpreplay.worker.ts): repeated entries of
 *   u32 ms | u8 kind=1 | u8 pad | u16 x | u16 y | u16 w | u16 h | u16 pad | RGBA[w*h]
 * all little-endian, no file header.
 */
function makeDisplayStream(opts: { seconds: number; rects: number; full: number; w: number; h: number }): Buffer {
  const { seconds, rects, full, w, h } = opts
  const entries: Buffer[] = []
  let seed = 11
  const rnd = () => (seed = (seed * 1103515245 + 12345) & 0x7fffffff) / 0x7fffffff
  const entry = (ms: number, x: number, y: number, rw: number, rh: number) => {
    const head = Buffer.alloc(16)
    head.writeUInt32LE(ms, 0)
    head.writeUInt8(1, 4)
    head.writeUInt16LE(x, 6)
    head.writeUInt16LE(y, 8)
    head.writeUInt16LE(rw, 10)
    head.writeUInt16LE(rh, 12)
    const px = Buffer.alloc(rw * rh * 4)
    // Noise rather than a solid colour, so nothing downstream can compress
    // it into something smaller than a real desktop would be.
    for (let i = 0; i < px.length; i += 4) {
      px[i] = (rnd() * 255) | 0; px[i + 1] = (rnd() * 255) | 0; px[i + 2] = (rnd() * 255) | 0; px[i + 3] = 255
    }
    entries.push(Buffer.concat([head, px]))
  }
  const total = rects + full
  const step = (seconds * 1000) / total
  let ms = 0
  for (let i = 0; i < total; i++) {
    if (i % Math.ceil(total / full) === 0) entry(ms, 0, 0, w, h)
    else {
      const rw = 128, rh = 128
      entry(ms, (rnd() * (w - rw)) | 0, (rnd() * (h - rh)) | 0, rw, rh)
    }
    ms += step
  }
  return Buffer.concat(entries)
}

test('desktop replay holds a flat main-thread heap on a large recording', async ({ page }) => {
  test.slow()
  const stream = makeDisplayStream({ seconds: 300, rects: 300, full: 6, w: 1024, h: 768 })
  await page.route('**/e2e.rdp', (route) =>
    route.fulfill({ status: 200, contentType: 'application/octet-stream', body: stream }),
  )
  const errs = watchErrors(page)

  let path: string
  try {
    path = await firstSessionPath(page, /RDP/i)
  } catch {
    test.skip(true, 'fixture has no RDP session to load a stream into')
    return
  }
  await page.goto(`${path}?rdp=/e2e.rdp&w=1024&h=768`)
  await expect(page.getByRole('button', { name: 'Play' })).toBeEnabled({ timeout: 60_000 })
  const afterLoad = await heapMB(page)

  await page.getByRole('combobox', { name: 'Playback speed' }).click()
  await page.getByRole('option', { name: '8x' }).click()
  await page.getByRole('button', { name: 'Play' }).click()
  await page.waitForTimeout(8_000)
  const duringPlayback = await heapMB(page)
  await page.getByRole('button', { name: 'Pause' }).click()

  // Backward seeks are the heavy path for a delta stream: clear and replay
  // from the start, creating and closing a bitmap per rectangle.
  for (let i = 0; i < 3; i++) await page.keyboard.press('Shift+ArrowRight')
  for (let i = 0; i < 5; i++) await page.keyboard.press('Shift+ArrowLeft')
  await page.waitForTimeout(2_000)
  const afterSeeks = await heapMB(page)

  const ceiling = afterLoad * 1.35 + 24
  expect(duringPlayback, `heap grew during playback: ${afterLoad} -> ${duringPlayback} MB`).toBeLessThan(ceiling)
  expect(afterSeeks, `heap grew after seeks: ${afterLoad} -> ${afterSeeks} MB`).toBeLessThan(ceiling)
  errs.assertClean()
})
