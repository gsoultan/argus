import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { ChainInput, ChainLink, ChainResponse } from '~/workers/chain.worker'
import type { CastHeader, KernelExec, ReplayResponse } from '~/workers/replay.worker'
import type { TimedCommand } from '~/lib/ansi'

/* ── Audit chain ─────────────────────────────────────────────────────────── */

export interface ChainState {
  status: 'idle' | 'working' | 'ready' | 'failed'
  links: ChainLink[]
  head: string | null
  progress: number
  ms: number | null
  error: string | null
  verified: { ok: boolean; checked: number; brokenAt: number | null; ms: number } | null
}

const IDLE: ChainState = {
  status: 'idle', links: [], head: null, progress: 0, ms: null, error: null, verified: null,
}

export function useAuditChain(events: ChainInput[] | undefined) {
  const [state, setState] = useState<ChainState>(IDLE)
  const workerRef = useRef<Worker | null>(null)

  useEffect(() => {
    const w = new Worker(new URL('../workers/chain.worker.ts', import.meta.url), {
      type: 'module',
    })
    workerRef.current = w
    w.onmessage = (ev: MessageEvent<ChainResponse>) => {
      const m = ev.data
      if (m.type === 'progress') {
        setState((s) => ({ ...s, progress: m.total ? m.done / m.total : 0 }))
      } else if (m.type === 'built') {
        setState({
          status: 'ready', links: m.links, head: m.head, progress: 1, ms: m.ms,
          error: null, verified: null,
        })
      } else if (m.type === 'verified') {
        setState((s) => ({
          ...s,
          verified: { ok: m.ok, checked: m.checked, brokenAt: m.brokenAt, ms: m.ms },
        }))
      } else if (m.type === 'error') {
        // A chain the console could not compute must not read as an intact one.
        setState((s) => ({ ...s, status: 'failed', error: m.message }))
      }
    }
    return () => {
      w.terminate()
      workerRef.current = null
    }
  }, [])

  useEffect(() => {
    if (!events || events.length === 0 || !workerRef.current) return
    setState({ ...IDLE, status: 'working' })
    workerRef.current.postMessage({ type: 'build', events })
  }, [events])

  const verify = useCallback(() => {
    if (!workerRef.current || state.links.length === 0) return
    setState((s) => ({ ...s, verified: null, progress: 0 }))
    workerRef.current.postMessage({ type: 'verify', links: state.links })
  }, [state.links])

  return { ...state, verify }
}

/* ── Replay decoding ─────────────────────────────────────────────────────── */

/**
 * What the main thread knows about a recording.
 *
 * Note what is absent: the frames. They stay in the worker, and the player asks
 * for the text it needs to draw. Everything here is O(1) or O(commands) in the
 * size of the recording, so holding it costs the same for a ten-second session
 * and a ten-hour one.
 */
export interface ReplayState {
  status: 'idle' | 'decoding' | 'ready' | 'failed'
  header: CastHeader | null
  execs: KernelExec[]
  heuristicCommands: TimedCommand[]
  frameCount: number
  duration: number
  outputBytes: number
  progress: number
  ms: number | null
  error: string | null
}

const REPLAY_IDLE: ReplayState = {
  status: 'idle', header: null, execs: [], heuristicCommands: [], frameCount: 0,
  duration: 0, outputBytes: 0, progress: 0, ms: null, error: null,
}

export interface ReplaySource extends ReplayState {
  /** Full screen contents at `t`. Used when seeking. */
  screen: (t: number) => Promise<string>
  /** Output emitted in `(from, to]`. Used for ordinary forward playback. */
  advance: (from: number, to: number) => Promise<string>
}

export function useCastDecoder(source: string | undefined): ReplaySource {
  const [state, setState] = useState<ReplayState>(REPLAY_IDLE)
  const workerRef = useRef<Worker | null>(null)
  // Correlated by id so a stale answer — a seek the user has already moved past
  // — resolves its own caller and never paints over a newer one.
  const pending = useRef(new Map<number, (text: string) => void>())
  const nextId = useRef(0)

  useEffect(() => {
    if (!source) {
      setState(REPLAY_IDLE)
      return
    }
    const w = new Worker(new URL('../workers/replay.worker.ts', import.meta.url), {
      type: 'module',
    })
    workerRef.current = w
    setState({ ...REPLAY_IDLE, status: 'decoding' })

    w.onmessage = (ev: MessageEvent<ReplayResponse>) => {
      const m = ev.data
      switch (m.type) {
        case 'progress':
          setState((s) => ({ ...s, progress: m.total ? m.done / m.total : 0 }))
          break
        case 'decoded':
          setState({
            status: 'ready',
            header: m.header,
            execs: m.execs,
            heuristicCommands: m.heuristicCommands,
            frameCount: m.frameCount,
            duration: m.duration,
            outputBytes: m.outputBytes,
            progress: 1,
            ms: m.ms,
            error: null,
          })
          break
        case 'screen':
        case 'advance': {
          const resolve = pending.current.get(m.id)
          if (resolve) {
            pending.current.delete(m.id)
            resolve(m.text)
          }
          break
        }
        case 'error':
          setState((s) => ({ ...s, status: 'failed', error: m.message }))
          break
      }
    }

    w.postMessage({ type: 'decode', source })
    return () => {
      w.terminate()
      workerRef.current = null
      pending.current.clear()
    }
  }, [source])

  const request = useCallback(
    (msg: { type: 'screen'; t: number } | { type: 'advance'; from: number; to: number }) =>
      new Promise<string>((resolve) => {
        const w = workerRef.current
        if (!w) {
          resolve('')
          return
        }
        const id = ++nextId.current
        pending.current.set(id, resolve)
        w.postMessage({ ...msg, id })
      }),
    [],
  )

  const screen = useCallback((t: number) => request({ type: 'screen', t }), [request])
  const advance = useCallback(
    (from: number, to: number) => request({ type: 'advance', from, to }),
    [request],
  )

  return useMemo(() => ({ ...state, screen, advance }), [state, screen, advance])
}
