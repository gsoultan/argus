// Decodes a captured Windows desktop (deskdrill -capture) in Chrome, with the
// console's graphics pipeline code, and checks that a desktop came out.
//
//   bun build scripts/gfx-check/harness.ts --outfile <dir>/harness.js --target browser
//   node scripts/gfx-check/run.mjs <capture> <dir>
//
// Writes <dir>/desktop.png, the canvas at the end, and <dir>/result.json.
// GFX_CHANNEL picks the browser: "chrome" (the default) is the installed
// Google Chrome, which has an H.264 decoder; Playwright's own Chromium does
// not, and serves only to check the plumbing with GFX_REQUIRE_H264=0.
import { chromium } from '@playwright/test'
import { readFileSync, writeFileSync } from 'node:fs'
import { createServer } from 'node:http'
import { join } from 'node:path'

const [capture, dir] = process.argv.slice(2)
if (!capture || !dir) {
  console.error('usage: run.mjs <capture> <dir>')
  process.exit(2)
}
const requireH264 = process.env.GFX_REQUIRE_H264 !== '0'

const files = {
  '/': ['text/html', '<!doctype html><canvas id="screen"></canvas><script type="module" src="/harness.js"></script>'],
  '/harness.js': ['text/javascript', readFileSync(join(dir, 'harness.js'))],
  '/capture.bin': ['application/octet-stream', readFileSync(capture)],
}
// GFX_TRUTH names the 4:4:4 picture the capture's last frame should show.
const truth = process.env.GFX_TRUTH
if (truth) files['/truth.yuv'] = ['application/octet-stream', readFileSync(truth)]
const server = createServer((req, res) => {
  const f = files[req.url ?? '']
  if (!f) return res.writeHead(404).end()
  res.writeHead(200, { 'content-type': f[0] }).end(f[1])
})
await new Promise((ok) => server.listen(0, '127.0.0.1', ok))
const base = `http://127.0.0.1:${server.address().port}`

const channel = process.env.GFX_CHANNEL ?? 'chrome'
const browser = await chromium.launch(channel === 'chromium' ? {} : { channel })
let code = 0
try {
  const page = await browser.newPage()
  const errors = []
  page.on('pageerror', (e) => errors.push(e.message))
  await page.goto(base)
  await page.waitForFunction(() => 'decodeCapture' in window)
  const result = await page.evaluate(() => window.decodeCapture('/capture.bin'))
  result.browser = browser.version()
  result.combinerDiff = await page.evaluate(() => window.checkCombiner())
  if (truth) result.truth = await page.evaluate(() => window.compareTruth('/truth.yuv'))
  result.pageErrors = errors
  await page.locator('#screen').screenshot({ path: join(dir, 'desktop.png') })
  writeFileSync(join(dir, 'result.json'), JSON.stringify(result, null, 2))
  console.log(JSON.stringify(result, null, 2))

  const problems = []
  if (result.commands === 0) problems.push('no graphics commands in the capture')
  if (requireH264 && result.pictures === 0) problems.push('no H.264 picture was decoded')
  if (result.failures > 0) problems.push(`a decoder failed ${result.failures} times: ${result.lastError}`)
  if (requireH264 && result.colours < 16) problems.push(`the screen has ${result.colours} colours: nothing was drawn`)
  if (result.combinerDiff < 0) problems.push('no WebGL 2: AVC444 cannot be drawn in full colour')
  if (result.combinerDiff > 1) problems.push(`the GPU's full-colour pictures differ from the CPU's by up to ${result.combinerDiff}`)
  // The full-colour picture is within the encoder's loss of the truth, and
  // well closer than 4:2:0 could be.
  if (result.truth && (result.truth.full > 4 || result.truth.full > result.truth.at420 / 2)) {
    problems.push(`the screen is ${result.truth.full.toFixed(2)} from the 4:4:4 picture; 4:2:0 would be ${result.truth.at420.toFixed(2)}`)
  }
  if (errors.length > 0) problems.push(`the page threw: ${errors.join('; ')}`)
  for (const p of problems) console.error(`::error::${p}`)
  code = problems.length > 0 ? 1 : 0
} finally {
  await browser.close()
  server.close()
}
process.exit(code)
