import { useCallback, useEffect, useRef, useState } from 'react'
import {
  Alert, Anchor, Badge, Box, Button, FileButton, Group, Loader, Progress,
  Stack, Table, Text, TextInput,
} from '@mantine/core'
import {
  IconArrowUp, IconDownload, IconFile, IconFolder, IconRefresh, IconUpload, IconX,
} from '@tabler/icons-react'
import { downloadBlob } from '~/lib/download'
import { FS, SP } from '~/theme'

/**
 * File transfer in the browser: the console's scp.
 *
 * Opens on the same single-use ticket as a terminal, through the control
 * plane to the server's agent, which reads and writes as the chosen account.
 * Every upload and download is written into the audit log with its size and
 * SHA-256, so moving a file here is no less accountable than a session.
 *
 * One operation at a time, in the order asked, which is what the agent's side
 * of the protocol expects; the buttons are disabled while one is running.
 */

export type FilesState = 'connecting' | 'live' | 'closed' | 'error'

export interface FileBrowserProps {
  /** The WebSocket endpoint as an http(s) URL; see filesEndpoint. */
  endpoint: string
  /** Fetches a ticket; see LiveTerminal for why this is a callback. */
  getTicket: () => Promise<string | null>
  target: string
  principal: string
  onStateChange?: (state: FilesState, detail?: string) => void
}

interface Entry {
  name: string
  dir: boolean
  size: number
  mode: string
  mtime: string
  link?: string
}

interface Message {
  type: string
  path?: string
  data?: string
  entries?: Entry[]
  truncated?: boolean
  size?: number
  sha256?: string
}

/** Matches the control plane's chunk, which is also its read limit. */
const CHUNK = 128 * 1024
/** Upload backpressure: wait for the socket to drain past this. */
const HIGH_WATER = 4 * 1024 * 1024

export function FileBrowser({ endpoint, getTicket, target, principal, onStateChange }: FileBrowserProps) {
  const wsRef = useRef<WebSocket | null>(null)
  // The one operation in flight: resolved by the message that ends it.
  const waiter = useRef<((m: Message) => void) | null>(null)
  const chunks = useRef<ArrayBuffer[]>([])
  const received = useRef(0)

  const [state, setState] = useState<FilesState>('connecting')
  const [path, setPath] = useState('~')
  const [typed, setTyped] = useState('~')
  const [entries, setEntries] = useState<Entry[]>([])
  const [truncated, setTruncated] = useState(false)
  const [busy, setBusy] = useState<string | null>(null)
  const [progress, setProgress] = useState<number | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [notice, setNotice] = useState<string | null>(null)
  const [conflict, setConflict] = useState<File | null>(null)

  const status = useCallback(
    (s: FilesState, d?: string) => {
      setState(s)
      onStateChange?.(s, d)
    },
    [onStateChange],
  )

  /** Sends one request and waits for the message that ends it. */
  const ask = useCallback((m: object, ends: string[]): Promise<Message> => {
    const ws = wsRef.current
    if (!ws || ws.readyState !== WebSocket.OPEN) {
      return Promise.resolve({ type: 'error', data: 'not connected' })
    }
    return new Promise((resolve) => {
      waiter.current = (reply) => {
        if (reply.type === 'error' || ends.includes(reply.type)) {
          waiter.current = null
          resolve(reply)
        }
      }
      ws.send(JSON.stringify(m))
    })
  }, [])

  const list = useCallback(
    async (p: string) => {
      setBusy('Listing')
      setError(null)
      const r = await ask({ type: 'list', path: p }, ['listing'])
      setBusy(null)
      if (r.type === 'error') {
        setError(r.data ?? 'could not list that folder')
        return
      }
      const at = r.path ?? p
      setPath(at)
      setTyped(at)
      setEntries(r.entries ?? [])
      setTruncated(Boolean(r.truncated))
    },
    [ask],
  )

  useEffect(() => {
    let disposed = false
    let ws: WebSocket | null = null
    void (async () => {
      const ticket = await getTicket()
      if (disposed) return
      if (!ticket) {
        status('error', 'could not obtain authorisation for file transfer')
        return
      }
      const url = new URL(endpoint, window.location.href)
      url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
      url.searchParams.set('target', target)
      url.searchParams.set('principal', principal)
      // Single-use and short-lived, which is the only reason it may travel in
      // the query string; see LiveTerminal.
      url.searchParams.set('ticket', ticket)
      ws = new WebSocket(url.toString())
      ws.binaryType = 'arraybuffer'
      wsRef.current = ws
      ws.onmessage = (ev) => {
        if (ev.data instanceof ArrayBuffer) {
          chunks.current.push(ev.data)
          received.current += ev.data.byteLength
          setProgress(received.current)
          return
        }
        let m: Message
        try {
          m = JSON.parse(String(ev.data)) as Message
        } catch {
          return
        }
        if (m.type === 'ready') {
          status('live')
          void list('~')
          return
        }
        if (waiter.current) {
          waiter.current(m)
          return
        }
        if (m.type === 'error') status('error', m.data)
      }
      ws.onerror = () => status('error', 'the connection to the control plane failed')
      ws.onclose = () => {
        if (!disposed) setState((s) => (s === 'error' ? s : 'closed'))
        waiter.current?.({ type: 'error', data: 'the connection closed' })
      }
    })()
    return () => {
      disposed = true
      ws?.close()
      wsRef.current = null
    }
    // getTicket is deliberately not a dependency: a new ticket is a new session.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [endpoint, target, principal])

  const join = (dir: string, name: string) => (dir.endsWith('/') ? dir + name : `${dir}/${name}`)
  const parent = (p: string) => {
    if (p === '/' || p === '~') return p
    const i = p.replace(/\/+$/, '').lastIndexOf('/')
    return i <= 0 ? '/' : p.slice(0, i)
  }

  const download = async (e: Entry) => {
    const full = join(path, e.name)
    chunks.current = []
    received.current = 0
    setProgress(0)
    setBusy(`Downloading ${e.name}`)
    setError(null)
    setNotice(null)
    const r = await ask({ type: 'download', path: full }, ['download-end'])
    setBusy(null)
    setProgress(null)
    if (r.type === 'error') {
      setError(r.data ?? 'download failed')
      return
    }
    downloadBlob(new Blob(chunks.current), e.name)
    chunks.current = []
    setNotice(`Downloaded ${e.name}: ${formatSize(r.size ?? 0)}, sha256 ${(r.sha256 ?? '').slice(0, 16)}…`)
  }

  const upload = async (file: File, overwrite = false) => {
    const ws = wsRef.current
    if (!ws) return
    const full = join(path, file.name)
    setBusy(`Uploading ${file.name}`)
    setError(null)
    setNotice(null)
    setConflict(null)
    const ready = await ask({ type: 'upload', path: full, overwrite }, ['upload-ready'])
    if (ready.type === 'error') {
      setBusy(null)
      setError(ready.data ?? 'upload refused')
      return
    }
    setProgress(0)
    const done = new Promise<Message>((resolve) => {
      waiter.current = (m) => {
        if (m.type === 'error' || m.type === 'upload-done') {
          waiter.current = null
          resolve(m)
        }
      }
    })
    for (let off = 0; off < file.size; off += CHUNK) {
      while (ws.bufferedAmount > HIGH_WATER) {
        await new Promise((r) => setTimeout(r, 20))
        if (ws.readyState !== WebSocket.OPEN) break
      }
      if (ws.readyState !== WebSocket.OPEN) break
      ws.send(await file.slice(off, off + CHUNK).arrayBuffer())
      setProgress(Math.min(off + CHUNK, file.size))
    }
    if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: 'upload-end' }))
    const r = await done
    setBusy(null)
    setProgress(null)
    if (r.type === 'error') {
      if (!overwrite && /already exists/.test(r.data ?? '')) {
        setConflict(file)
        return
      }
      setError(r.data ?? 'upload failed')
      return
    }
    setNotice(`Uploaded ${file.name}: ${formatSize(r.size ?? 0)}, sha256 ${(r.sha256 ?? '').slice(0, 16)}…`)
    void list(path)
  }

  if (state === 'connecting') {
    return (
      <Group gap="xs" p="md">
        <Loader size="xs" />
        <Text size={FS.meta} c="dimmed">Opening file transfer through the server&apos;s agent…</Text>
      </Group>
    )
  }

  const live = state === 'live'
  const segments = path.split('/').filter(Boolean)

  return (
    <Stack gap="sm" p="md">
      <Group gap="xs" wrap="nowrap">
        <Button
          size="xs"
          variant="light"
          color="slate"
          leftSection={<IconArrowUp size={14} />}
          disabled={!live || !!busy}
          onClick={() => void list(parent(path))}
        >
          Up
        </Button>
        <TextInput
          size="xs"
          style={{ flex: 1 }}
          value={typed}
          aria-label="Folder"
          disabled={!live || !!busy}
          onChange={(e) => setTyped(e.currentTarget.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') void list(typed)
          }}
        />
        <Button
          size="xs"
          variant="light"
          color="slate"
          leftSection={<IconRefresh size={14} />}
          disabled={!live || !!busy}
          onClick={() => void list(path)}
        >
          Refresh
        </Button>
        <FileButton onChange={(f) => f && void upload(f)} disabled={!live || !!busy}>
          {(props) => (
            <Button size="xs" leftSection={<IconUpload size={14} />} disabled={!live || !!busy} {...props}>
              Upload
            </Button>
          )}
        </FileButton>
      </Group>

      <Group gap={SP.tight} wrap="wrap">
        <Anchor size={FS.meta} onClick={() => live && !busy && void list('/')}>/</Anchor>
        {segments.map((s, i) => (
          <Group key={`${i}-${s}`} gap={SP.tight} wrap="nowrap">
            <Anchor
              size={FS.meta}
              onClick={() => live && !busy && void list('/' + segments.slice(0, i + 1).join('/'))}
            >
              {s}
            </Anchor>
            {i < segments.length - 1 && <Text size={FS.meta} c="dimmed">/</Text>}
          </Group>
        ))}
      </Group>

      {busy && (
        <Box>
          <Group gap={SP.snug}>
            <Loader size="xs" />
            <Text size={FS.meta}>
              {busy}
              {progress !== null ? ` · ${formatSize(progress)}` : ''}
            </Text>
          </Group>
          {progress !== null && <Progress mt={SP.snug} size="xs" value={100} animated striped />}
        </Box>
      )}
      {conflict && (
        <Alert color="amber" title={`${conflict.name} already exists here`}>
          <Group gap="xs">
            <Text size={FS.body}>Replace it? The replacement is recorded in the audit log.</Text>
            <Button size="compact-xs" color="amber" onClick={() => void upload(conflict, true)}>
              Replace
            </Button>
            <Button size="compact-xs" variant="subtle" color="slate" onClick={() => setConflict(null)}>
              Keep the existing file
            </Button>
          </Group>
        </Alert>
      )}
      {error && (
        <Alert color="rose" icon={<IconX size={16} />} withCloseButton onClose={() => setError(null)}>
          <Text size={FS.body}>{error}</Text>
        </Alert>
      )}
      {notice && (
        <Text size={FS.meta} c="teal.4">{notice}</Text>
      )}
      {!live && (
        <Alert color={state === 'error' ? 'rose' : 'slate'} title="File transfer closed">
          <Text size={FS.body}>Close this and open it again from Connect to continue.</Text>
        </Alert>
      )}

      <Table striped highlightOnHover verticalSpacing={4} fz={FS.meta}>
        <Table.Thead>
          <Table.Tr>
            <Table.Th>Name</Table.Th>
            <Table.Th style={{ textAlign: 'right' }}>Size</Table.Th>
            <Table.Th visibleFrom="sm">Permissions</Table.Th>
            <Table.Th visibleFrom="sm">Modified</Table.Th>
            <Table.Th />
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {entries.map((e) => (
            <Table.Tr key={e.name}>
              <Table.Td>
                <Group gap={SP.snug} wrap="nowrap">
                  {e.dir ? <IconFolder size={14} /> : <IconFile size={14} opacity={0.6} />}
                  {e.dir ? (
                    <Anchor size={FS.meta} onClick={() => live && !busy && void list(join(path, e.name))}>
                      {e.name}
                    </Anchor>
                  ) : (
                    <Text size={FS.meta}>{e.name}</Text>
                  )}
                  {e.link && <Badge size="xs" variant="outline" color="slate">→ {e.link}</Badge>}
                </Group>
              </Table.Td>
              <Table.Td style={{ textAlign: 'right' }}>{e.dir ? '' : formatSize(e.size)}</Table.Td>
              <Table.Td visibleFrom="sm" ff="monospace">{e.mode}</Table.Td>
              <Table.Td visibleFrom="sm">{e.mtime ? new Date(e.mtime).toLocaleString() : ''}</Table.Td>
              <Table.Td style={{ textAlign: 'right' }}>
                {!e.dir && (
                  <Button
                    size="compact-xs"
                    variant="subtle"
                    leftSection={<IconDownload size={12} />}
                    disabled={!live || !!busy}
                    onClick={() => void download(e)}
                  >
                    Download
                  </Button>
                )}
              </Table.Td>
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
      {live && entries.length === 0 && !busy && (
        <Text size={FS.meta} c="dimmed">This folder is empty.</Text>
      )}
      {truncated && (
        <Text size={FS.micro} c="dimmed">
          Showing the first 5,000 entries. Type a path to go straight to a file&apos;s folder.
        </Text>
      )}
    </Stack>
  )
}

function formatSize(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  if (n < 1024 * 1024 * 1024) return `${(n / 1024 / 1024).toFixed(1)} MB`
  return `${(n / 1024 / 1024 / 1024).toFixed(2)} GB`
}
