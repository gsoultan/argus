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

/** 8 entries and 623.5 kB on disk when these were set. */
const MAX_ENTRIES = 10
const MAX_KB = 700

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
  // Inter and JetBrains Mono, plain latin. weight.spec.ts asserts a first visit
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
