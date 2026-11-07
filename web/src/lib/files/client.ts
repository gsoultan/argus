/**
 * The browser's side of the files service (internal/control/files.go).
 *
 * One WebSocket, one operation at a time: the agent's side of the protocol
 * answers requests in the order they were asked, so every call here waits its
 * turn in a queue rather than racing another on the same socket. Errors end
 * the operation they belong to, never the connection.
 */

export interface FileEntry {
  name: string
  dir: boolean
  size: number
  mode: string
  mtime: string
  link?: string
}

export interface Listing {
  path: string
  entries: FileEntry[]
  truncated: boolean
}

export interface Transferred {
  path: string
  size: number
  sha256: string
}

export interface Preview {
  path: string
  size: number
  truncated: boolean
  from: 'start' | 'end'
  text: string
}

export type FilesState = 'connecting' | 'live' | 'closed' | 'error'

interface Message {
  type: string
  op?: string
  path?: string
  to?: string
  from?: string
  data?: string
  entries?: FileEntry[]
  truncated?: boolean
  size?: number
  sha256?: string
  content?: string
}

/** A refusal from the server for one operation. */
export class FileOpError extends Error {
  readonly path?: string
  constructor(message: string, path?: string) {
    super(message)
    this.path = path
  }
  get exists(): boolean {
    return /already exists/.test(this.message)
  }
  /** An agent older than the console, asked for an operation it lacks. */
  get unsupported(): boolean {
    return /unknown operation/.test(this.message)
  }
}

/** Matches the control plane's chunk, which is also its read limit. */
export const CHUNK = 128 * 1024
/** Upload backpressure: wait for the socket to drain past this. */
const HIGH_WATER = 4 * 1024 * 1024

export interface FilesClientOptions {
  endpoint: string
  getTicket: () => Promise<string | null>
  target: string
  principal: string
  onState: (state: FilesState, detail?: string) => void
}

export class FilesClient {
  private ws: WebSocket | null = null
  private waiter: ((m: Message) => void) | null = null
  private onBinary: ((b: ArrayBuffer) => void) | null = null
  private queue: Promise<unknown> = Promise.resolve()
  private disposed = false
  state: FilesState = 'connecting'

  private readonly opts: FilesClientOptions
  constructor(opts: FilesClientOptions) {
    this.opts = opts
  }

  async open(): Promise<void> {
    const ticket = await this.opts.getTicket()
    if (this.disposed) return
    if (!ticket) {
      this.setState('error', 'could not obtain authorisation for file transfer')
      return
    }
    const url = new URL(this.opts.endpoint, window.location.href)
    url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
    url.searchParams.set('target', this.opts.target)
    url.searchParams.set('principal', this.opts.principal)
    // Single-use and short-lived, which is the only reason it may travel in
    // the query string; see LiveTerminal.
    url.searchParams.set('ticket', ticket)
    const ws = new WebSocket(url.toString())
    ws.binaryType = 'arraybuffer'
    this.ws = ws
    ws.onmessage = (ev) => {
      if (ev.data instanceof ArrayBuffer) {
        this.onBinary?.(ev.data)
        return
      }
      let m: Message
      try {
        m = JSON.parse(String(ev.data)) as Message
      } catch {
        return
      }
      if (m.type === 'ready') {
        this.setState('live')
        return
      }
      if (this.waiter) {
        this.waiter(m)
        return
      }
      if (m.type === 'error') this.setState('error', m.data)
    }
    ws.onerror = () => this.setState('error', 'the connection to the control plane failed')
    ws.onclose = () => {
      if (!this.disposed && this.state !== 'error') this.setState('closed')
      this.waiter?.({ type: 'error', data: 'the connection closed' })
    }
  }

  close() {
    this.disposed = true
    this.ws?.close()
    this.ws = null
  }

  private setState(s: FilesState, detail?: string) {
    this.state = s
    if (!this.disposed) this.opts.onState(s, detail)
  }

  /** Runs fn when every operation asked before it has finished. */
  private serial<T>(fn: () => Promise<T>): Promise<T> {
    const run = this.queue.then(fn, fn)
    this.queue = run.catch(() => undefined)
    return run
  }

  /** Sends one request and waits for the message that ends it. */
  private ask(m: object, ends: string[]): Promise<Message> {
    const ws = this.ws
    if (!ws || ws.readyState !== WebSocket.OPEN) {
      return Promise.reject(new FileOpError('not connected'))
    }
    return new Promise((resolve, reject) => {
      this.waiter = (reply) => {
        if (reply.type === 'error') {
          this.waiter = null
          reject(new FileOpError(reply.data ?? 'the operation failed', reply.path))
        } else if (ends.includes(reply.type)) {
          this.waiter = null
          resolve(reply)
        }
      }
      ws.send(JSON.stringify(m))
    })
  }

  list(path: string): Promise<Listing> {
    return this.serial(async () => {
      const r = await this.ask({ type: 'list', path }, ['listing'])
      return { path: r.path ?? path, entries: r.entries ?? [], truncated: Boolean(r.truncated) }
    })
  }

  download(path: string, onProgress?: (bytes: number) => void): Promise<Transferred & { data: Blob }> {
    return this.serial(async () => {
      const chunks: ArrayBuffer[] = []
      let n = 0
      this.onBinary = (b) => {
        chunks.push(b)
        n += b.byteLength
        onProgress?.(n)
      }
      try {
        const r = await this.ask({ type: 'download', path }, ['download-end'])
        return { path: r.path ?? path, size: r.size ?? n, sha256: r.sha256 ?? '', data: new Blob(chunks) }
      } finally {
        this.onBinary = null
      }
    })
  }

  upload(path: string, file: Blob, overwrite: boolean, onProgress?: (bytes: number) => void): Promise<Transferred> {
    return this.serial(async () => {
      const ws = this.ws
      await this.ask({ type: 'upload', path, overwrite }, ['upload-ready'])
      if (!ws) throw new FileOpError('not connected')
      const done = new Promise<Message>((resolve, reject) => {
        this.waiter = (m) => {
          if (m.type === 'error') {
            this.waiter = null
            reject(new FileOpError(m.data ?? 'upload failed', m.path))
          } else if (m.type === 'upload-done') {
            this.waiter = null
            resolve(m)
          }
        }
      })
      for (let off = 0; off < file.size; off += CHUNK) {
        while (ws.bufferedAmount > HIGH_WATER && ws.readyState === WebSocket.OPEN) {
          await new Promise((r) => setTimeout(r, 20))
        }
        if (ws.readyState !== WebSocket.OPEN) break
        ws.send(await file.slice(off, off + CHUNK).arrayBuffer())
        // What has left this browser, less what is still queued in it.
        onProgress?.(Math.max(0, Math.min(off + CHUNK, file.size) - ws.bufferedAmount))
      }
      if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: 'upload-end' }))
      const r = await done
      onProgress?.(file.size)
      return { path: r.path ?? path, size: r.size ?? file.size, sha256: r.sha256 ?? '' }
    })
  }

  mkdir(path: string): Promise<string> {
    return this.serial(async () => (await this.ask({ type: 'mkdir', path }, ['done'])).path ?? path)
  }

  rename(path: string, to: string): Promise<string> {
    return this.serial(async () => (await this.ask({ type: 'rename', path, to }, ['done'])).to ?? to)
  }

  remove(path: string): Promise<void> {
    return this.serial(async () => {
      await this.ask({ type: 'delete', path }, ['done'])
    })
  }

  preview(path: string, from: 'start' | 'end'): Promise<Preview> {
    return this.serial(async () => {
      const r = await this.ask({ type: 'preview', path, from }, ['preview'])
      return {
        path: r.path ?? path,
        size: r.size ?? 0,
        truncated: Boolean(r.truncated),
        from: r.from === 'end' ? 'end' : 'start',
        text: decodeText(r.content ?? ''),
      }
    })
  }
}

/** Base64 from the control plane to text, replacing bytes that are not UTF-8. */
export function decodeText(b64: string): string {
  const bin = atob(b64)
  const bytes = new Uint8Array(bin.length)
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i)
  return new TextDecoder('utf-8', { fatal: false }).decode(bytes)
}

/** Whether text looks like a binary file decoded by mistake. */
export function looksBinary(text: string): boolean {
  const sample = text.slice(0, 4096)
  if (!sample) return false
  let odd = 0
  for (const ch of sample) {
    const c = ch.charCodeAt(0)
    if (c === 0 || c === 0xfffd || (c < 32 && c !== 9 && c !== 10 && c !== 13)) odd++
  }
  return odd / sample.length > 0.05
}
