import { useCallback, useEffect, useRef, useState } from 'react'
import type { ChainInput, ChainLink, ChainResponse } from '~/workers/chain.worker'
import type { CastFrame, CastHeader, Keyframe, ReplayResponse } from '~/workers/replay.worker'

/* ── Audit chain ─────────────────────────────────────────────────────────── */

export interface ChainState {
  status: 'idle' | 'working' | 'ready' | 'failed'
  links: ChainLink[]
  head: string | null
  progress: number
  ms: number | null
  verified: { ok: boolean; checked: number; brokenAt: number | null; ms: number } | null
}

const IDLE: ChainState = {
  status: 'idle', links: [], head: null, progress: 0, ms: null, verified: null,
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
          status: 'ready', links: m.links, head: m.head, progress: 1, ms: m.ms, verified: null,
        })
      } else if (m.type === 'verified') {
        setState((s) => ({
          ...s,
          verified: { ok: m.ok, checked: m.checked, brokenAt: m.brokenAt, ms: m.ms },
        }))
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

export interface ReplayState {
  status: 'idle' | 'decoding' | 'ready' | 'failed'
  header: CastHeader | null
  frames: CastFrame[]
  keyframes: Keyframe[]
  duration: number
  outputBytes: number
  progress: number
  ms: number | null
  error: string | null
}

const REPLAY_IDLE: ReplayState = {
  status: 'idle', header: null, frames: [], keyframes: [], duration: 0,
  outputBytes: 0, progress: 0, ms: null, error: null,
}

export function useCastDecoder(source: string | undefined) {
  const [state, setState] = useState<ReplayState>(REPLAY_IDLE)

  useEffect(() => {
    if (!source) {
      setState(REPLAY_IDLE)
      return
    }
    const w = new Worker(new URL('../workers/replay.worker.ts', import.meta.url), {
      type: 'module',
    })
    setState({ ...REPLAY_IDLE, status: 'decoding' })

    w.onmessage = (ev: MessageEvent<ReplayResponse>) => {
      const m = ev.data
      if (m.type === 'progress') {
        setState((s) => ({ ...s, progress: m.total ? m.done / m.total : 0 }))
      } else if (m.type === 'decoded') {
        setState({
          status: 'ready',
          header: m.header,
          frames: m.frames,
          keyframes: m.keyframes,
          duration: m.duration,
          outputBytes: m.outputBytes,
          progress: 1,
          ms: m.ms,
          error: null,
        })
      } else {
        setState((s) => ({ ...s, status: 'failed', error: m.message }))
      }
    }

    w.postMessage({ type: 'decode', source })
    return () => w.terminate()
  }, [source])

  return state
}
