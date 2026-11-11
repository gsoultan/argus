import { BASE, TOKEN, apiFetch, isConfigured, serverError } from '~/lib/live'
import type { ReplayActivity } from '~/types/domain'

/*
 * Only a desktop recording's page needs this, so it is loaded when one opens
 * rather than with live.ts, which every route loads.
 */

/**
 * Fetches a Remote Desktop recording, decoded to display frames.
 *
 * The control plane decodes and verifies before returning anything, so the
 * verdict arrives with the frames rather than after them — a viewer must not
 * form an impression of a session and only then be told it was altered.
 */
export async function rdpReplay(
  sessionId: string,
  onProgress?: (received: number, total: number | null) => void,
): Promise<{ buffer: ArrayBuffer; width: number; height: number; verified: string; activity: ReplayActivity[] } | null> {
  if (!isConfigured()) return null
  const res = await apiFetch(`${BASE}/api/v1/sessions/${sessionId}/rdp-replay`, {
    credentials: 'include',
    headers: TOKEN ? { Authorization: `Bearer ${TOKEN}` } : {},
  })
  // In the control plane's own words: it knows whether the recording is still
  // on the host, storage is down, or the decode failed, and a page that said
  // only "not available" for all three sent people looking in the wrong place.
  if (!res.ok) throw new Error((await serverError(res)) ?? `The control plane answered ${res.status}.`)

  let info: { width?: number; height?: number; activity?: ReplayActivity[] } = {}
  try {
    info = JSON.parse(res.headers.get('X-Argus-Replay-Info') ?? '{}')
  } catch {
    // A missing or malformed header should not lose the frames; the canvas
    // falls back to a sensible size.
  }
  return {
    buffer: await readWithProgress(res, onProgress),
    width: info.width ?? 1024,
    height: info.height ?? 768,
    verified: res.headers.get('X-Argus-Chain-Verified') ?? 'unverified',
    activity: Array.isArray(info.activity) ? info.activity : [],
  }
}

/**
 * The body of res, reporting as it arrives. A desktop replay is decoded
 * frames and runs to hundreds of megabytes for a long screen share; read in
 * one arrayBuffer() call, the page sat on "Decoding the recording…" with no
 * sign it was getting anywhere.
 */
async function readWithProgress(
  res: Response,
  onProgress?: (received: number, total: number | null) => void,
): Promise<ArrayBuffer> {
  const length = Number(res.headers.get('Content-Length'))
  const total = Number.isFinite(length) && length > 0 ? length : null
  if (!onProgress || !res.body) return res.arrayBuffer()
  const reader = res.body.getReader()
  // Straight into one buffer when the length is known, so a large replay is
  // not held twice while the chunks are joined.
  let out = new Uint8Array(total ?? 1 << 20)
  let received = 0
  onProgress(0, total)
  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    if (received + value.length > out.length) {
      const grown = new Uint8Array(Math.max(out.length * 2, received + value.length))
      grown.set(out.subarray(0, received))
      out = grown
    }
    out.set(value, received)
    received += value.length
    onProgress(received, total)
  }
  return received === out.length ? out.buffer : out.slice(0, received).buffer
}
