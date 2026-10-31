import '@testing-library/jest-dom/vitest'
import { configure } from '@testing-library/react'
import { vi } from 'vitest'

/**
 * Route tests render the whole console, lazy chunks and all. In a full run
 * every file does that at once, and findBy's default second was the slowest
 * route's time on an idle machine, not a busy one. Waiting longer costs
 * nothing when the page is there sooner.
 */
configure({ asyncUtilTimeout: 4_000 })

/**
 * jsdom implements neither of these, and Mantine uses both on mount.
 *
 * Stubbed rather than mocked away per-test: every component in this console is
 * rendered inside a MantineProvider, so a missing matchMedia is an environment
 * gap rather than anything a test should have an opinion about.
 */
if (!window.matchMedia) {
  window.matchMedia = (query: string) =>
    ({
      matches: false,
      media: query,
      onchange: null,
      addListener: vi.fn(),
      removeListener: vi.fn(),
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
      dispatchEvent: vi.fn(),
    }) as unknown as MediaQueryList
}

if (!globalThis.ResizeObserver) {
  globalThis.ResizeObserver = class {
    observe() {}
    unobserve() {}
    disconnect() {}
  } as unknown as typeof ResizeObserver
}

// Mantine 9.7's autosizing Textarea listens for web fonts finishing loading.
if (!(document as { fonts?: unknown }).fonts) {
  Object.defineProperty(document, 'fonts', {
    value: { addEventListener: vi.fn(), removeEventListener: vi.fn(), ready: Promise.resolve() },
  })
}

if (!Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = vi.fn()
}

/**
 * jsdom has no Web Worker.
 *
 * Four components construct one on mount, so without this every route that
 * replays a recording or verifies the audit chain throws before it renders and
 * the test reports a crash where the real browser shows a spinner.
 *
 * The stub stays silent on purpose. A worker that answered would need to be a
 * second implementation of the real ones, and a route test asserting against a
 * fake decoder proves nothing about the decoder. What these tests do cover is
 * the render path up to the point the worker replies — which is exactly where
 * a route that throws on mount fails.
 */
if (!globalThis.Worker) {
  globalThis.Worker = class {
    onmessage: ((e: MessageEvent) => void) | null = null
    onerror: ((e: ErrorEvent) => void) | null = null
    postMessage() {}
    terminate() {}
    addEventListener() {}
    removeEventListener() {}
    dispatchEvent() {
      return false
    }
  } as unknown as typeof Worker
}

// Used by the export helpers; jsdom implements neither.
if (!URL.createObjectURL) {
  URL.createObjectURL = () => 'blob:test'
  URL.revokeObjectURL = () => {}
}
