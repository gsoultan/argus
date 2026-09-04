import { describe, expect, it } from 'vitest'
import { buildCast } from '~/lib/cast'
import { decodeCast } from '~/lib/castDecode'

/**
 * The fixture must agree with itself.
 *
 * Every eBPF-fidelity session in the demo data replayed a recording with no
 * kernel events in it, so the badge said "eBPF" while the timeline said
 * "heuristic" -- the exact mismatch the console now flags as a possible
 * tampering. A fixture that trips its own tamper warning is not a demo.
 */
describe('buildCast fidelity', () => {
  it('carries kernel-observed executions for an eBPF session', () => {
    const d = decodeCast(buildCast('pay-06.payments.northwind.id', 'ops', '2026-08-28T09:00:00Z', 'ebpf'))
    expect(d.execs.length).toBeGreaterThan(0)
    // One exec per command the script ran, each naming what the kernel saw.
    const comms = d.execs.map((e) => e.comm)
    expect(comms).toContain('systemctl')
    expect(comms).toContain('sudo')
    // Root is uid 0; everyone else is not.
    expect(d.execs.every((e) => e.uid === 1000)).toBe(true)
    const asRoot = decodeCast(buildCast('h', 'root', '2026-08-28T09:00:00Z', 'ebpf'))
    expect(asRoot.execs.every((e) => e.uid === 0)).toBe(true)
  })

  it('carries none for a PTY-only session', () => {
    const d = decodeCast(buildCast('pay-06.payments.northwind.id', 'ops', '2026-08-28T09:00:00Z', 'pty'))
    expect(d.execs).toHaveLength(0)
    // The terminal output is identical either way; fidelity changes what the
    // kernel saw, not what the screen showed.
    const e = decodeCast(buildCast('pay-06.payments.northwind.id', 'ops', '2026-08-28T09:00:00Z', 'ebpf'))
    expect(d.outputBytes).toBe(e.outputBytes)
  })

  it('places each exec after Enter and before the first output byte', () => {
    const d = decodeCast(buildCast('h', 'ops', '2026-08-28T09:00:00Z', 'ebpf'))
    for (const x of d.execs) {
      const enter = [...d.frames].reverse().find((f) => f.kind === 'i' && f.data === '\r' && f.t <= x.t)
      const nextOut = d.frames.find((f) => f.kind === 'o' && f.t > x.t && f.data.length > 2)
      expect(enter).toBeDefined()
      expect(nextOut).toBeDefined()
    }
  })
})
