import { test, expect } from '@playwright/test'
import { readFile, stat } from 'node:fs/promises'
import { join } from 'node:path'

/**
 * What the service worker stores on a first visit.
 *
 * `globPatterns` in vite.config.ts is a hand-maintained list, with exactly the
 * drift profile of the Mantine stylesheet list that cascade.spec.ts guards:
 * widening it is one character, nothing fails, and the cost lands on every
 * first visit as a background download nobody sees.
 *
 * It has drifted before. Precaching everything meant a first visit pulled
 * xterm.js and every route chunk the operator never opened, which is why the
 * list is explicit — see [pwa-and-service-worker] in .serena/memories.
 *
 * Reads the build rather than driving a browser: the manifest is compiled into
 * dist/sw.js, and what is under test is what the build decided to store, not
 * what a browser does with it afterwards.
 */

const DIST = new URL('../dist/', import.meta.url).pathname

/**
 * 8 entries and 623.5 kB on disk when these were set. 705.7 kB with the
 * alerts bell (697.3 before), whose eager code and stylesheet are in the shell.
 * 712.7 kB and 9 entries with light and dark themes: public/color-scheme.js,
 * which index.html loads before the bundle and so must be stored for an
 * offline start, plus the dark tokens in the shared stylesheet and the theme
 * menu in the entry chunk.
 * 717.8 kB with Brokers in the menu (715.4 before, same build): the item, the
 * two broker routes' loaders and search rules, and the entry's map of the
 * chunks they load. The workspace and its stylesheet are not stored.
 * 718.5 kB with Android devices: the device kind in the shell's shared
 * modules (lists, roles, the asset type). The wizard's QR code and the phone
 * toolbar are in chunks that are not stored.
 * 720.7 kB with Cloud in the menu: the item and its icon, and the two cloud
 * routes' guards and search rule in the entry. The pages are not stored.
 * 722.1 kB with the Disks tab (722.0 before, same build): its name in the
 * server page's tab list and the shared stylesheet. The tab, its dialog and
 * Settings › Disk growing are in chunks that are not stored.
 * 722.7 kB with assets by type (722.1 before, same build): the Updates route's
 * guard and search rule in the entry, the bell's Updates tab, and the asset
 * type in the shared modules. The Updates page and Asset types settings are
 * in chunks that are not stored.
 */
const MAX_ENTRIES = 10
const MAX_KB = 723

async function precached(): Promise<string[]> {
  const sw = await readFile(join(DIST, 'sw.js'), 'utf8')
  // The manifest is inlined as precacheAndRoute([{url:"…",revision:…}, …]).
  const entries = [...sw.matchAll(/\{url:"([^"]+)",revision:/g)].map((m) => m[1] as string)
  expect(entries.length, 'found no precache manifest in dist/sw.js').toBeGreaterThan(0)
  return entries
}

test('the worker precaches the shell and nothing else', async ({ browserName }) => {
  // Reads a build artefact, so one run says everything a second would.
  test.skip(browserName !== 'chromium', 'inspects the build, not the browser')

  const entries = await precached()

  // Route chunks are fetched on demand and kept by runtimeCaching instead. The
  // terminal emulator is the one that made this rule: 83 kB gzipped, needed by
  // two routes, and it was being pushed to everyone on their first visit.
  const routeChunks = entries.filter(
    (u) => /^assets\/.+\.js$/.test(u) && !/^assets\/index-/.test(u),
  )
  expect(
    routeChunks,
    'Only the entry chunk belongs in the precache. These are route chunks, and '
      + 'precaching them makes every first visit pay for pages nobody opened.',
  ).toEqual([])

  // @fontsource ships a subset per script, and exactly two render this console:
  // IBM Plex Sans and JetBrains Mono, plain latin. weight.spec.ts asserts a first visit
  // fetches those two, so storing any more is storing what nobody asked for.
  //
  // `latin-ext` is the one to watch. The pattern was `*latin*`, which matched it
  // and put 98 kB of accented-Latin coverage in every first visit. An accented
  // name still renders — the CacheFirst rule fetches its subset on demand, the
  // way Cyrillic and Greek always have.
  const extraFonts = entries.filter(
    (u) => u.endsWith('.woff2') && !/-latin-wght-/.test(u),
  )
  expect(
    extraFonts,
    'Font subsets beyond plain latin are precached. They are fetched on demand '
      + 'by the CacheFirst font rule when a name actually needs one.',
  ).toEqual([])

  // Listing icon.svg in globPatterns as well as the manifest put it in twice,
  // same url, same revision.
  const duplicates = entries.filter((u, i) => entries.indexOf(u) !== i)
  expect(duplicates, 'the same url is precached more than once').toEqual([])

  expect(
    entries.length,
    `${entries.length} precache entries:\n  ${entries.join('\n  ')}`,
  ).toBeLessThanOrEqual(MAX_ENTRIES)

  const sizes = await Promise.all(
    entries.map(async (u) => {
      try {
        return (await stat(join(DIST, u))).size
      } catch {
        // index.html and the webmanifest are emitted; anything unresolvable is
        // worth knowing about rather than silently scoring zero.
        throw new Error(`precached ${u}, which is not in dist/`)
      }
    }),
  )
  const totalKB = sizes.reduce((a, b) => a + b, 0) / 1024
  // Same reason as weight.spec.ts: the figure in the comment above dates, the
  // reported one cannot.
  const line =
    `${entries.length} entries / ${MAX_ENTRIES}, ${totalKB.toFixed(1)} kB / ${MAX_KB}`
  test.info().annotations.push({ type: 'precache', description: line })
  console.log(`  ▸ precache: ${line}`)
  expect(
    totalKB,
    `The worker stores ${totalKB.toFixed(1)} kB on a first visit.`,
  ).toBeLessThan(MAX_KB)
})
