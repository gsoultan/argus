/**
 * asciicast v2 decoding, as pure functions.
 *
 * Separated from the worker so it can be tested directly, and so the worker
 * stays a message shell. The decoded result is deliberately *not* something the
 * main thread ever receives: see `replay.worker.ts`.
 *
 * asciicast v2 is newline-delimited JSON — a header object, then one array per
 * event, `[time, type, data]`, where type is "o" (stdout), "i" (stdin), or the
 * Argus extension "x" (kernel-observed execution).
 * https://docs.asciinema.org/manual/asciicast/v2/
 */

export interface CastHeader {
  version: number
  width: number
  height: number
  timestamp?: number
  title?: string
  env?: Record<string, string>
}

/**
 * One execution the kernel observed, carried in the recording's `x` stream.
 *
 * Inside the hash chain like every other frame, so the command list is exactly
 * as tamper-evident as the terminal output beside it.
 */
export interface KernelExec {
  t: number
  pid: number
  ppid: number
  uid: number
  comm: string
  filename: string
  args: string[]
  truncated?: boolean
}

export interface CastFrame {
  t: number
  kind: 'o' | 'i'
  data: string
}

/** Accumulated output at a checkpoint, so seeking doesn't replay from zero. */
export interface Keyframe {
  t: number
  frameIndex: number
  text: string
}

export interface Decoded {
  header: CastHeader
  frames: CastFrame[]
  keyframes: Keyframe[]
  execs: KernelExec[]
  duration: number
  outputBytes: number
}

export const DEFAULT_KEYFRAME_INTERVAL_MS = 5_000

export function decodeCast(
  source: string,
  keyframeIntervalMs = DEFAULT_KEYFRAME_INTERVAL_MS,
  onProgress?: (done: number, total: number) => void,
): Decoded {
  const lines = source.split('\n').filter((l) => l.trim().length > 0)
  if (lines.length === 0) throw new Error('empty recording')

  const header = JSON.parse(lines[0]!) as CastHeader
  if (header.version !== 2) {
    throw new Error(`unsupported asciicast version ${header.version}, expected 2`)
  }

  const frames: CastFrame[] = []
  const execs: KernelExec[] = []
  const keyframes: Keyframe[] = []
  let acc = ''
  let outputBytes = 0
  let nextKeyframeAt = 0

  for (let i = 1; i < lines.length; i++) {
    const raw = lines[i]!
    let parsed: [number, string, string]
    try {
      parsed = JSON.parse(raw) as [number, string, string]
    } catch {
      continue // tolerate a truncated tail — recordings can be cut mid-write
    }
    const [t, kind, data] = parsed

    if (kind === 'x') {
      // Kernel evidence: what actually ran, as opposed to what the terminal
      // displayed. Rendered as its own lane rather than mixed into the output,
      // because the two are different kinds of claim.
      try {
        const e = JSON.parse(data) as Omit<KernelExec, 't'>
        execs.push({ ...e, t })
      } catch {
        // A frame this build cannot read is skipped rather than failing the
        // whole replay: an unreadable command must not cost the recording.
      }
      continue
    }
    if (kind !== 'o' && kind !== 'i') continue

    frames.push({ t, kind, data })
    if (kind === 'o') {
      acc += data
      outputBytes += data.length
    }

    if (t * 1000 >= nextKeyframeAt) {
      keyframes.push({ t, frameIndex: frames.length - 1, text: acc })
      nextKeyframeAt = t * 1000 + keyframeIntervalMs
    }

    if (i % 500 === 0) onProgress?.(i, lines.length)
  }

  const last = frames.at(-1)
  return { header, frames, keyframes, execs, duration: last ? last.t : 0, outputBytes }
}

/**
 * Index of the last frame at or before `t`.
 *
 * Binary search rather than a scan: playback asks this on every advance, and a
 * linear walk makes the cost of drawing one frame proportional to how far into
 * the recording you already are.
 */
export function frameIndexAt(frames: readonly CastFrame[], t: number): number {
  let lo = 0
  let hi = frames.length - 1
  let out = -1
  while (lo <= hi) {
    const mid = (lo + hi) >> 1
    if (frames[mid]!.t <= t) {
      out = mid
      lo = mid + 1
    } else {
      hi = mid - 1
    }
  }
  return out
}

/**
 * The complete screen contents at `t`.
 *
 * Starts from the newest keyframe at or before `t` and applies only the
 * remainder, so the cost is bounded by the keyframe interval rather than by the
 * length of the recording. Used for seeking; ordinary playback uses
 * `deltaBetween`, which is cheaper still.
 */
export function screenAt(
  frames: readonly CastFrame[],
  keyframes: readonly Keyframe[],
  t: number,
): string {
  let base = ''
  let start = 0
  for (let i = keyframes.length - 1; i >= 0; i--) {
    const kf = keyframes[i]!
    if (kf.t <= t) {
      base = kf.text
      start = kf.frameIndex + 1
      break
    }
  }

  let tail = ''
  for (let i = start; i < frames.length; i++) {
    const f = frames[i]!
    if (f.t > t) break
    if (f.kind === 'o') tail += f.data
  }
  return base + tail
}

/**
 * Output emitted in `(from, to]`.
 *
 * This is what ordinary forward playback needs: a terminal emulator holds the
 * screen state itself, so replaying means handing it the new bytes and nothing
 * more. The previous player instead rebuilt the entire scrollback from the
 * start on every animation frame.
 */
export function deltaBetween(
  frames: readonly CastFrame[],
  from: number,
  to: number,
): string {
  if (to <= from) return ''
  let out = ''
  for (let i = frameIndexAt(frames, from) + 1; i < frames.length; i++) {
    const f = frames[i]!
    if (f.t > to) break
    if (f.kind === 'o') out += f.data
  }
  return out
}
