import { describe, expect, it, vi } from 'vitest'
import { entryOf, isNewBuild, runningEntry, servedEntry } from '~/lib/update'

const html = (entry: string) =>
  `<!doctype html><html><head><script src="/color-scheme.js"></script>` +
  `<script type="module" crossorigin src="${entry}"></script></head><body></body></html>`

describe('noticing a new console build', () => {
  it('reads the entry script an index.html names', () => {
    expect(entryOf(html('/assets/index-Ab_9-xY.js'))).toBe('/assets/index-Ab_9-xY.js')
    expect(entryOf('<html><script type="module" src="/src/main.tsx"></script></html>')).toBe('')
  })

  it('reads the entry script this page runs, and nothing under the dev server', () => {
    document.head.innerHTML = '<script type="module" src="/assets/index-OLD1.js"></script>'
    expect(runningEntry()).toBe('/assets/index-OLD1.js')
    document.head.innerHTML = '<script type="module" src="/src/main.tsx"></script>'
    expect(runningEntry()).toBe('')
  })

  it('calls it new only when both sides are known and differ', () => {
    expect(isNewBuild('/assets/index-OLD1.js', '/assets/index-NEW2.js')).toBe(true)
    expect(isNewBuild('/assets/index-OLD1.js', '/assets/index-OLD1.js')).toBe(false)
    expect(isNewBuild('', '/assets/index-NEW2.js')).toBe(false)
    expect(isNewBuild('/assets/index-OLD1.js', '')).toBe(false)
  })

  it('asks the server past both caches', async () => {
    const f = vi.fn(async () => new Response(html('/assets/index-NEW2.js')))
    expect(await servedEntry(f as unknown as typeof fetch)).toBe('/assets/index-NEW2.js')
    const [url, init] = f.mock.calls[0] as unknown as [string, RequestInit]
    expect(url).toMatch(/^\/index\.html\?build-check=\d+$/)
    expect(init.cache).toBe('no-store')
  })

  it('knows nothing while the control plane is down', async () => {
    const down = vi.fn(async () => { throw new TypeError('Failed to fetch') })
    expect(await servedEntry(down as unknown as typeof fetch)).toBe('')
    const bad = vi.fn(async () => new Response('', { status: 502 }))
    expect(await servedEntry(bad as unknown as typeof fetch)).toBe('')
  })
})
