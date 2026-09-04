import { describe, expect, it } from 'vitest'
import { decodeCast, deltaBetween, frameIndexAt, screenAt } from '~/lib/castDecode'

const ESC = String.fromCharCode(27)

function cast(events: Array<[number, string, string]>, header?: object): string {
  return [
    JSON.stringify({ version: 2, width: 80, height: 24, ...header }),
    ...events.map((e) => JSON.stringify(e)),
  ].join('\n')
}

describe('decodeCast', () => {
  it('separates stdout, stdin and kernel execs', () => {
    const d = decodeCast(
      cast([
        [0.1, 'o', 'hello '],
        [0.2, 'i', 'l'],
        [0.3, 'o', 'world'],
        [0.4, 'x', JSON.stringify({ pid: 9, ppid: 1, uid: 0, comm: 'ls', filename: '/bin/ls', args: ['ls', '-la'] })],
      ]),
    )
    expect(d.frames).toHaveLength(3)
    expect(d.execs).toEqual([
      expect.objectContaining({ t: 0.4, comm: 'ls', args: ['ls', '-la'] }),
    ])
    expect(d.outputBytes).toBe('hello '.length + 'world'.length)
    expect(d.duration).toBe(0.3)
  })

  it('rejects anything that is not asciicast v2', () => {
    expect(() => decodeCast(cast([], { version: 1 }))).toThrow(/version 1/)
    expect(() => decodeCast('')).toThrow(/empty recording/)
  })

  /** Recordings can be cut mid-write when a gateway dies; a torn last line
      must cost that line and nothing more. */
  it('tolerates a truncated tail', () => {
    const d = decodeCast(cast([[0.1, 'o', 'kept']]) + '\n[0.2,"o","tru')
    expect(d.frames).toHaveLength(1)
    expect(d.frames[0]!.data).toBe('kept')
  })

  it('skips an exec frame it cannot parse rather than failing the recording', () => {
    const d = decodeCast(cast([[0.1, 'o', 'out'], [0.2, 'x', '{not json']]))
    expect(d.execs).toHaveLength(0)
    expect(d.frames).toHaveLength(1)
  })

  it('takes keyframes at the requested interval', () => {
    const d = decodeCast(
      cast([[0, 'o', 'a'], [1, 'o', 'b'], [2, 'o', 'c'], [3, 'o', 'd']]),
      2_000,
    )
    expect(d.keyframes.map((k) => k.t)).toEqual([0, 2])
    expect(d.keyframes[1]!.text).toBe('abc')
  })
})

describe('frameIndexAt', () => {
  const d = decodeCast(cast([[0, 'o', 'a'], [1, 'o', 'b'], [2, 'o', 'c']]))

  it('finds the last frame at or before t', () => {
    expect(frameIndexAt(d.frames, -1)).toBe(-1)
    expect(frameIndexAt(d.frames, 0)).toBe(0)
    expect(frameIndexAt(d.frames, 1.5)).toBe(1)
    expect(frameIndexAt(d.frames, 99)).toBe(2)
  })

  it('agrees with a linear scan across the whole range', () => {
    for (let t = -0.5; t <= 3; t += 0.25) {
      let expected = -1
      d.frames.forEach((f, i) => { if (f.t <= t) expected = i })
      expect(frameIndexAt(d.frames, t)).toBe(expected)
    }
  })
})

describe('screenAt / deltaBetween', () => {
  const source = cast([
    [0, 'o', 'one '], [1, 'o', 'two '], [2, 'o', 'three '],
    [3, 'i', 'ignored'], [4, 'o', 'four'],
  ])

  it('accumulates only stdout up to t', () => {
    const d = decodeCast(source, 10_000)
    expect(screenAt(d.frames, d.keyframes, 0)).toBe('one ')
    expect(screenAt(d.frames, d.keyframes, 2)).toBe('one two three ')
    expect(screenAt(d.frames, d.keyframes, 99)).toBe('one two three four')
  })

  /** The keyframe path and the from-zero path must agree, or seeking would
      show a different screen depending on where the checkpoints happened to fall. */
  it('gives the same screen whatever the keyframe interval', () => {
    const coarse = decodeCast(source, 100_000)
    const fine = decodeCast(source, 1)
    for (const t of [0, 0.5, 1, 2.5, 4, 10]) {
      expect(screenAt(fine.frames, fine.keyframes, t)).toBe(
        screenAt(coarse.frames, coarse.keyframes, t),
      )
    }
  })

  it('returns only the output emitted in (from, to]', () => {
    const d = decodeCast(source, 10_000)
    expect(deltaBetween(d.frames, 0, 2)).toBe('two three ')
    expect(deltaBetween(d.frames, 2, 4)).toBe('four')
    expect(deltaBetween(d.frames, 4, 4)).toBe('')
    expect(deltaBetween(d.frames, 5, 1)).toBe('')
  })

  /**
   * The property the whole player rests on: replaying forward in slices must
   * produce exactly what a single seek to the end produces. If these diverge, a
   * scrubbed recording shows something the session never displayed.
   */
  it('composes: successive deltas equal one full seek', () => {
    const d = decodeCast(source, 10_000)
    let acc = ''
    for (let t = 0; t <= 5; t += 0.5) acc += deltaBetween(d.frames, t - 0.5, t)
    expect(acc).toBe(screenAt(d.frames, d.keyframes, 5))
  })

  it('carries ANSI sequences through untouched, so the emulator sees them', () => {
    const d = decodeCast(cast([[0, 'o', `${ESC}[32mgreen${ESC}[0m`]]))
    expect(screenAt(d.frames, d.keyframes, 1)).toBe(`${ESC}[32mgreen${ESC}[0m`)
  })
})
