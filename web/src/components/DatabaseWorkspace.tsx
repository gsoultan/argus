import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  ActionIcon, Badge, Box, Button, Drawer, Group, Loader, ScrollArea, Stack, Tabs, Text, Textarea, Tooltip,
  UnstyledButton,
} from '@mantine/core'
import { useDisclosure, useMediaQuery } from '@mantine/hooks'
import {
  IconChevronDown, IconChevronRight, IconDatabase, IconDownload, IconLock, IconMenu2, IconPlayerPlay,
  IconPlayerStop, IconTable, IconEye, IconFunction, IconHash,
} from '@tabler/icons-react'
import { FS, SP } from '~/theme'
import type { DBAccess, DBLevel, Database } from '~/types/domain'

/*
The query workspace: a left bar with the object tree, a SQL editor, and a
results grid, over one WebSocket to the control plane's relay.

Built to stay light on a laptop and usable on a phone:

- Rows arrive as binary batches in Postgres's own text format and are decoded
  into plain string arrays, never objects; the grid renders only the rows in
  view (windowed), so a 100,000-row result costs the strings and nothing per
  cell.
- The tree is loaded lazily per node, as the person's role, so it shows only
  what they may see and the first paint needs one small query.
- Narrow screens fold the tree into a drawer and stack editor over results.
*/

export interface WorkspaceProps {
  database: Database
  access: DBAccess
  /** Fetches a ticket for one session; see LiveTerminal for why a callback. */
  getTicket: (level?: DBLevel) => Promise<{ ticket: string; role: string; level: DBLevel } | null>
  endpoint: string
  onAskForAccess?: () => void
}

type Col = { name: string; type: string }
type Cell = string | null
type Obj = { name: string; kind: string; detail?: string; count?: number }

interface PGAnswer {
  id: number
  type: 'ready' | 'cols' | 'done' | 'error' | 'schema' | 'closed'
  cols?: Col[]
  tag?: string
  rows?: number
  ms?: number
  truncated?: boolean
  error?: string
  code?: string
  position?: number
  hint?: string
  objects?: Obj[]
  role?: string
  level?: DBLevel
  version?: string
  session?: string
}

interface Result {
  id: number
  sql: string
  cols: Col[]
  rows: Cell[][]
  done?: PGAnswer
  error?: PGAnswer
  startedAt: number
}

/** Decodes one row batch: id, then cells as uvarint(len*2+cut+1) + bytes; 0 is NULL. */
function decodeBatch(buf: ArrayBuffer, ncols: number, dec: TextDecoder): { id: number; rows: Cell[][] } {
  const b = new Uint8Array(buf)
  const id = ((b[0]! << 24) | (b[1]! << 16) | (b[2]! << 8) | b[3]!) >>> 0
  let p = 4
  const rows: Cell[][] = []
  while (p < b.length) {
    const row: Cell[] = new Array(ncols)
    for (let i = 0; i < ncols; i++) {
      let v = 0
      let shift = 0
      let byte: number
      do {
        byte = b[p++]!
        v += (byte & 0x7f) * 2 ** shift
        shift += 7
      } while (byte & 0x80)
      if (v === 0) {
        row[i] = null
        continue
      }
      const len = Math.floor((v - 1) / 2)
      const cut = (v - 1) % 2 === 1
      row[i] = dec.decode(b.subarray(p, p + len)) + (cut ? ' …' : '')
      p += len
    }
    rows.push(row)
  }
  return { id, rows }
}

const KIND_ICON: Record<string, typeof IconTable> = {
  table: IconTable, view: IconEye, matview: IconEye, function: IconFunction, procedure: IconFunction,
  sequence: IconHash,
}

export function DatabaseWorkspace({ database, access, getTicket, endpoint, onAskForAccess }: WorkspaceProps) {
  const narrow = useMediaQuery('(max-width: 48em)')
  const [treeOpen, tree] = useDisclosure(false)
  const wsRef = useRef<WebSocket | null>(null)
  const nextId = useRef(1)
  const dec = useMemo(() => new TextDecoder(), [])
  const [status, setStatus] = useState<'connecting' | 'ready' | 'closed'>('connecting')
  const [fatal, setFatal] = useState<string | null>(null)
  const [ready, setReady] = useState<PGAnswer | null>(null)
  const [sql, setSql] = useState('')
  const [result, setResult] = useState<Result | null>(null)
  const [running, setRunning] = useState<number | null>(null)
  const [history, setHistory] = useState<{ sql: string; at: number; ok: boolean }[]>([])
  const resultRef = useRef<Result | null>(null)
  const pending = useRef(new Map<number, (a: PGAnswer) => void>())
  // Requests made before the session is ready (the tree asks for schemas on
  // mount) wait here and go out together on 'ready'.
  const queue = useRef<string[]>([])
  const [maxRows, setMaxRows] = useState(1000)

  const failPendingRef = useRef<() => void>(() => {})
  // One socket per mount; a dropped socket is a new session, by design.
  useEffect(() => {
    const failPending = () => failPendingRef.current()
    let ws: WebSocket | null = null
    let disposed = false
    void (async () => {
      const t = await getTicket()
      if (disposed) return
      if (!t) {
        setFatal('could not obtain authorisation for this session')
        setStatus('closed')
        return
      }
      const url = new URL(endpoint, window.location.href)
      url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
      url.searchParams.set('database', database.id)
      url.searchParams.set('principal', `${t.role}@${t.level}`)
      url.searchParams.set('ticket', t.ticket)
      ws = new WebSocket(url.toString())
      ws.binaryType = 'arraybuffer'
      wsRef.current = ws
      ws.onmessage = (ev) => {
        if (ev.data instanceof ArrayBuffer) {
          const r = resultRef.current
          if (!r) return
          const { id, rows } = decodeBatch(ev.data, r.cols.length, dec)
          if (id !== r.id) return
          // Append in place and publish a new object once per batch, so React
          // re-renders per batch rather than per row.
          r.rows.push(...rows)
          setResult({ ...r })
          return
        }
        const m = JSON.parse(ev.data as string) as { type: string; ans?: PGAnswer; data?: string }
        if (m.type === 'ready') {
          setStatus('ready')
          for (const frame of queue.current) ws?.send(frame)
          queue.current = []
          return
        }
        if (m.type === 'error') {
          setFatal(m.data ?? 'the session ended')
          setStatus('closed')
          failPending()
          return
        }
        const a = m.ans
        if (!a) return
        if (a.type === 'ready') {
          setReady(a)
          return
        }
        if (a.type === 'closed') {
          setFatal(a.error ?? 'the session was closed')
          setStatus('closed')
          failPending()
          return
        }
        const cb = pending.current.get(a.id)
        if (cb) cb(a)
      }
      ws.onclose = () => {
        setStatus('closed')
        setFatal((f) => f ?? 'the session ended')
        failPending()
      }
    })()
    return () => {
      disposed = true
      ws?.close()
      wsRef.current = null
    }
    // getTicket is deliberately not a dependency: a new ticket is a new session.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [database.id, endpoint])

  const statusRef = useRef(status)
  statusRef.current = status
  // Everything waiting gets a closed answer, so no promise is left hanging.
  failPendingRef.current = () => {
    queue.current = []
    for (const [id, cb] of pending.current) cb({ id, type: 'closed', error: 'the session ended' })
    pending.current.clear()
  }
  const send = useCallback((req: Record<string, unknown>) => {
    const frame = JSON.stringify({ type: 'req', req })
    if (statusRef.current === 'closed') return false
    const ws = wsRef.current
    if (statusRef.current === 'ready' && ws && ws.readyState === WebSocket.OPEN) {
      ws.send(frame)
    } else {
      queue.current.push(frame)
    }
    return true
  }, [])

  const schema = useCallback((kind: string, schemaName?: string, object?: string) =>
    new Promise<Obj[]>((resolve) => {
      const id = nextId.current++
      pending.current.set(id, (a) => {
        pending.current.delete(id)
        resolve(a.type === 'schema' ? (a.objects ?? []) : [])
      })
      if (!send({ id, op: 'schema', kind, schema: schemaName, object })) {
        pending.current.delete(id)
        resolve([])
      }
    }), [send])

  const run = useCallback((text: string) => {
    const stmt = text.trim()
    if (!stmt || running !== null) return
    const id = nextId.current++
    const r: Result = { id, sql: stmt, cols: [], rows: [], startedAt: Date.now() }
    resultRef.current = r
    setResult(r)
    setRunning(id)
    pending.current.set(id, (a) => {
      if (a.type === 'cols') {
        r.cols = a.cols ?? []
        setResult({ ...r })
        return
      }
      pending.current.delete(id)
      if (a.type === 'done') r.done = a
      else r.error = a
      setResult({ ...r })
      setRunning(null)
      setHistory((h) => [{ sql: stmt, at: Date.now(), ok: a.type === 'done' }, ...h].slice(0, 100))
    })
    if (!send({ id, op: 'run', sql: stmt, maxRows })) {
      pending.current.delete(id)
      setRunning(null)
    }
  }, [send, running, maxRows])

  const cancel = useCallback(() => {
    if (running !== null) send({ id: running, op: 'cancel' })
  }, [running, send])

  const editorRef = useRef<HTMLTextAreaElement | null>(null)
  const runSelectionOrAll = useCallback(() => {
    const el = editorRef.current
    if (el && el.selectionStart !== el.selectionEnd) {
      run(el.value.slice(el.selectionStart, el.selectionEnd))
    } else {
      run(sql)
    }
  }, [run, sql])

  const onKey = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
      e.preventDefault()
      runSelectionOrAll()
    }
  }

  const insert = (text: string) => {
    const el = editorRef.current
    if (!el) {
      setSql((s) => s + text)
      return
    }
    const a = el.selectionStart
    const b = el.selectionEnd
    const next = el.value.slice(0, a) + text + el.value.slice(b)
    setSql(next)
    requestAnimationFrame(() => {
      el.focus()
      el.selectionStart = el.selectionEnd = a + text.length
    })
  }

  const exportCsv = () => {
    const r = result
    if (!r || r.rows.length === 0) return
    const esc = (c: Cell) => {
      if (c === null) return ''
      // A cell that starts like a formula is quoted so a spreadsheet shows
      // it rather than runs it.
      const safe = /^[=+\-@\t\r]/.test(c) ? `'${c}` : c
      return /[",\n\r]/.test(safe) ? `"${safe.replace(/"/g, '""')}"` : safe
    }
    const lines = [r.cols.map((c) => esc(c.name)).join(',')]
    for (const row of r.rows) lines.push(row.map(esc).join(','))
    const blob = new Blob([lines.join('\n')], { type: 'text/csv' })
    const a = document.createElement('a')
    a.href = URL.createObjectURL(blob)
    a.download = `${database.dbname}-${new Date().toISOString().slice(0, 19).replace(/[:T]/g, '-')}.csv`
    a.click()
    URL.revokeObjectURL(a.href)
  }

  const level = ready?.level ?? access.level
  const bar = (
    <LeftBar
      database={database}
      level={level}
      schema={schema}
      onPick={(q) => { insert(q); if (narrow) tree.close() }}
      onRun={(q) => { setSql(q); run(q); if (narrow) tree.close() }}
      history={history}
    />
  )

  return (
    <Box style={{ display: 'flex', height: narrow ? 'auto' : 'calc(100dvh - 120px)', minHeight: 480 }}>
      {narrow ? (
        <Drawer opened={treeOpen} onClose={tree.close} size="85%" padding="xs" title={database.dbname}>
          {bar}
        </Drawer>
      ) : (
        <Box w={280} style={{ flexShrink: 0, borderRight: '1px solid var(--color-line)', overflow: 'hidden' }}>
          <ScrollArea h="100%" type="auto">{bar}</ScrollArea>
        </Box>
      )}

      <Box style={{ flex: 1, minWidth: 0, display: 'flex', flexDirection: 'column' }}>
        <Group gap="xs" px="md" py={SP.cozy} wrap="nowrap" style={{ borderBottom: '1px solid var(--color-line)' }}>
          {narrow && (
            <ActionIcon variant="subtle" onClick={tree.open} aria-label="Open the object tree">
              <IconMenu2 size={18} />
            </ActionIcon>
          )}
          <IconDatabase size={16} color="var(--color-brand)" />
          <Text fw={650} size={FS.body}>{database.dbname}</Text>
          {!narrow && <Text size={FS.micro} c="dimmed" ff="monospace">{database.hostname}{ready?.version ? ` · Postgres ${ready.version.split(' ')[0]}` : ''}</Text>}
          <Badge variant="light" color={{ production: 'rose', staging: 'amber', development: 'teal' }[database.label]} radius="sm">
            {database.label}
          </Badge>
          <Group gap={6} ml="auto" wrap="nowrap">
            <Badge variant="light" color={level === 'read' ? 'slate' : 'azure'} radius="sm">
              You can: {level ? level.toUpperCase() : '…'}
            </Badge>
            {(access.approvable || onAskForAccess) && level === 'read' && (
              <Button size="xs" variant="default" onClick={onAskForAccess} leftSection={<IconLock size={12} />}>
                {narrow ? 'Ask' : 'Ask for write access'}
              </Button>
            )}
            <Badge variant="light" color="rose" radius="sm" leftSection={<span style={{ display: 'inline-block', width: 6, height: 6, borderRadius: 3, background: 'currentColor' }} />}>
              {status === 'ready' ? 'Recorded' : status === 'connecting' ? 'Connecting' : 'Closed'}
            </Badge>
          </Group>
        </Group>

        {fatal && (
          <Box px="md" py={SP.cozy} style={{ background: 'var(--mantine-color-rose-light)', color: 'var(--mantine-color-rose-light-color)' }}>
            <Text size={FS.meta}>{fatal}</Text>
          </Box>
        )}

        <Box style={{ borderBottom: '1px solid var(--color-line)' }}>
          <Textarea
            ref={editorRef}
            value={sql}
            onChange={(e) => setSql(e.currentTarget.value)}
            onKeyDown={onKey}
            placeholder="SELECT … (⌘/Ctrl+Enter runs; select part of it to run only that)"
            autosize
            minRows={narrow ? 4 : 7}
            maxRows={16}
            spellCheck={false}
            autoCapitalize="off"
            autoCorrect="off"
            aria-label="SQL"
            styles={{ input: { fontFamily: 'var(--font-mono)', fontSize: 13, lineHeight: '21px', border: 0, borderRadius: 0 } }}
          />
        </Box>
        <Group gap="xs" px="md" py={SP.cozy} wrap="wrap" style={{ borderBottom: '1px solid var(--color-line)' }}>
          <Button size="xs" leftSection={<IconPlayerPlay size={14} />} onClick={runSelectionOrAll}
            disabled={status !== 'ready' || running !== null || !sql.trim()}>
            Run
          </Button>
          <Button size="xs" variant="default" leftSection={<IconPlayerStop size={14} />} onClick={cancel}
            disabled={running === null}>
            Cancel
          </Button>
          <Group gap={4} wrap="nowrap">
            <Text size={FS.micro} c="dimmed">Show up to</Text>
            <select value={maxRows} onChange={(e) => setMaxRows(Number(e.currentTarget.value))}
              style={{ fontSize: 12, padding: '2px 6px', borderRadius: 6, border: '1px solid var(--color-line)', background: 'var(--color-surface)', color: 'inherit' }}
              aria-label="Row limit">
              {[100, 1000, 10000, 100000].map((n) => <option key={n} value={n}>{n.toLocaleString()} rows</option>)}
            </select>
          </Group>
          {running !== null && <Loader size={14} />}
          <Button size="xs" variant="subtle" ml="auto" leftSection={<IconDownload size={14} />} onClick={exportCsv}
            disabled={!result || result.rows.length === 0}>
            Export CSV
          </Button>
        </Group>

        <Box style={{ flex: 1, minHeight: narrow ? 320 : 0, display: 'flex', flexDirection: 'column' }}>
          <Results result={result} narrow={narrow} />
        </Box>
      </Box>
    </Box>
  )
}

/* ── Results ────────────────────────────────────────────────────────────── */

const ROW_H = 28

function Results({ result, narrow }: { result: Result | null; narrow: boolean }) {
  const viewport = useRef<HTMLDivElement | null>(null)
  const [scrollTop, setScrollTop] = useState(0)
  const [height, setHeight] = useState(400)
  useEffect(() => {
    const el = viewport.current
    if (!el) return
    const ro = new ResizeObserver(() => setHeight(el.clientHeight))
    ro.observe(el)
    setHeight(el.clientHeight)
    return () => ro.disconnect()
  }, [])
  useEffect(() => { setScrollTop(0); if (viewport.current) viewport.current.scrollTop = 0 }, [result?.id])

  if (!result) {
    return (
      <Text size={FS.meta} c="dimmed" p="md">
        Results appear here. Pick a table on the left to start.
      </Text>
    )
  }
  const summary = result.error
    ? null
    : result.done
      ? `${result.done.tag ?? ''}${result.done.truncated ? ' · stopped at the row limit' : ''} · ${result.done.ms ?? 0} ms`
      : `${result.rows.length.toLocaleString()} rows so far…`

  // Windowed rows: only what is visible plus a margin is in the DOM.
  const total = result.rows.length
  const first = Math.max(0, Math.floor(scrollTop / ROW_H) - 10)
  const last = Math.min(total, Math.ceil((scrollTop + height) / ROW_H) + 10)
  const slice = result.rows.slice(first, last)

  return (
    <>
      <Group gap="md" px="md" py={6} style={{ borderBottom: '1px solid var(--color-line)' }} wrap="nowrap">
        <Text size={FS.meta} fw={600}>{result.error ? 'Message' : 'Results'}</Text>
        {summary && <Text size={FS.meta} c="dimmed" style={{ whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>{summary}</Text>}
      </Group>
      {result.error ? (
        <Box p="md">
          <Box p="sm" style={{ borderRadius: 8, background: 'var(--mantine-color-rose-light)', color: 'var(--mantine-color-rose-light-color)' }}>
            <Text size={FS.body} fw={600}>
              Postgres refused this: {result.error.error}
              {result.error.code && <Text span size={FS.micro} ff="monospace" ml={6}>({result.error.code})</Text>}
            </Text>
            {result.error.hint && <Text size={FS.meta} mt={4}>{result.error.hint}</Text>}
            {result.error.code === '42501' && (
              <Text size={FS.meta} mt={4}>
                Your access to this database does not allow that. Nothing was changed. Use <b>Ask for write access</b> above if you need it.
              </Text>
            )}
          </Box>
        </Box>
      ) : (
        <div
          ref={viewport}
          onScroll={(e) => setScrollTop(e.currentTarget.scrollTop)}
          style={{ flex: 1, overflow: 'auto', fontFamily: 'var(--font-mono)', fontSize: 12, position: 'relative' }}
        >
          <table style={{ borderCollapse: 'collapse', minWidth: '100%' }}>
            <thead style={{ position: 'sticky', top: 0, zIndex: 1, background: 'var(--color-raised)' }}>
              <tr>
                <th style={th}>#</th>
                {result.cols.map((c, i) => (
                  <th key={i} style={th}>{c.name} <span style={{ fontWeight: 400, color: 'var(--mantine-color-dimmed)' }}>{narrow ? '' : c.type}</span></th>
                ))}
              </tr>
            </thead>
            <tbody>
              {first > 0 && <tr><td style={{ height: first * ROW_H, padding: 0 }} colSpan={result.cols.length + 1} /></tr>}
              {slice.map((row, i) => (
                <tr key={first + i} style={{ height: ROW_H }}>
                  <td style={{ ...td, color: 'var(--mantine-color-dimmed)', background: 'var(--color-void)', textAlign: 'right' }}>{first + i + 1}</td>
                  {row.map((cell, j) => (
                    <td key={j} style={{ ...td, color: cell === null ? 'var(--mantine-color-dimmed)' : undefined, fontStyle: cell === null ? 'italic' : undefined }} title={cell ?? 'NULL'}>
                      {cell === null ? 'null' : cell.length > 200 ? cell.slice(0, 200) + '…' : cell}
                    </td>
                  ))}
                </tr>
              ))}
              {last < total && <tr><td style={{ height: (total - last) * ROW_H, padding: 0 }} colSpan={result.cols.length + 1} /></tr>}
            </tbody>
          </table>
        </div>
      )}
    </>
  )
}

const th: React.CSSProperties = {
  textAlign: 'left', padding: '6px 12px', borderRight: '1px solid var(--color-line)',
  borderBottom: '1px solid var(--color-line)', whiteSpace: 'nowrap', fontWeight: 600,
}
const td: React.CSSProperties = {
  padding: '0 12px', borderRight: '1px solid var(--color-line)', borderBottom: '1px solid var(--color-line)',
  whiteSpace: 'nowrap', maxWidth: 480, overflow: 'hidden', textOverflow: 'ellipsis',
}

/* ── Left bar ───────────────────────────────────────────────────────────── */

function LeftBar({ database, level, schema, onPick, onRun, history }: {
  database: Database
  level: DBLevel | ''
  schema: (kind: string, schemaName?: string, object?: string) => Promise<Obj[]>
  onPick: (text: string) => void
  onRun: (sql: string) => void
  history: { sql: string; at: number; ok: boolean }[]
}) {
  const [schemas, setSchemas] = useState<Obj[] | null>(null)
  useEffect(() => { void schema('schemas').then(setSchemas) }, [schema])
  const [tab, setTab] = useState<string | null>('objects')

  return (
    <Stack gap={SP.tight} p="xs">
      <Box p="xs" style={{ background: 'var(--color-raised)', borderRadius: 8 }}>
        <Group gap={6} wrap="nowrap">
          <span style={{ width: 7, height: 7, borderRadius: 4, background: 'var(--mantine-color-teal-6)' }} />
          <Text fw={600} size={FS.body}>{database.dbname}</Text>
          <Text size={FS.micro} c="dimmed" ff="monospace">{database.hostname}</Text>
        </Group>
        <Group gap={4} mt={4}>
          <Badge size="xs" variant="light" color={{ production: 'rose', staging: 'amber', development: 'teal' }[database.label]} radius="sm">{database.label}</Badge>
          <Badge size="xs" variant="light" color={level === 'read' ? 'slate' : 'azure'} radius="sm">{level || '…'}</Badge>
          <Badge size="xs" variant="outline" color="slate" radius="sm">recorded</Badge>
        </Group>
      </Box>
      <Tabs value={tab} onChange={setTab} variant="pills" radius="sm">
        <Tabs.List grow>
          <Tabs.Tab value="objects" fz={FS.meta}>Objects</Tabs.Tab>
          <Tabs.Tab value="history" fz={FS.meta}>History</Tabs.Tab>
        </Tabs.List>
      </Tabs>
      {tab === 'objects' && (
        <Box>
          <Text size={FS.micro} c="dimmed" tt="uppercase" fw={600} px={4} py={4} style={{ letterSpacing: '.06em' }}>Schemas</Text>
          {schemas === null && <Loader size={14} ml="sm" />}
          {schemas?.length === 0 && <Text size={FS.meta} c="dimmed" px="xs">Nothing your access can see.</Text>}
          {schemas?.map((s) => (
            <SchemaNode key={s.name} s={s} schema={schema} onPick={onPick} onRun={onRun} />
          ))}
          <Box mt="sm" px={4}>
            <Text size={FS.micro} c="dimmed" tt="uppercase" fw={600} py={4} style={{ letterSpacing: '.06em' }}>Administration</Text>
            <Tooltip label={level === 'dba' ? 'Coming next' : 'Needs DBA access'} withArrow>
              <Group gap={6} px={4} py={4} style={{ opacity: .6 }}>
                <IconChevronRight size={12} /><Text size={FS.meta}>Roles &amp; access</Text>
                <Badge size="xs" variant="light" color="slate" ml="auto">DBA</Badge>
              </Group>
            </Tooltip>
            <Group gap={6} px={4} py={4} style={{ opacity: .6 }}>
              <IconChevronRight size={12} /><Text size={FS.meta}>Activity &amp; locks</Text>
            </Group>
          </Box>
        </Box>
      )}
      {tab === 'history' && (
        <Stack gap={2}>
          {history.length === 0 && <Text size={FS.meta} c="dimmed" px="xs">Statements you run this session appear here.</Text>}
          {history.map((h, i) => (
            <UnstyledButton key={i} onClick={() => onPick(h.sql)} px="xs" py={4} style={{ borderRadius: 6 }}>
              <Text size={FS.micro} ff="monospace" c={h.ok ? undefined : 'rose'} lineClamp={2}>{h.sql}</Text>
              <Text size={FS.micro} c="dimmed">{new Date(h.at).toLocaleTimeString()}</Text>
            </UnstyledButton>
          ))}
        </Stack>
      )}
    </Stack>
  )
}

function SchemaNode({ s, schema, onPick, onRun }: {
  s: Obj
  schema: (kind: string, schemaName?: string, object?: string) => Promise<Obj[]>
  onPick: (t: string) => void
  onRun: (q: string) => void
}) {
  const [open, setOpen] = useState(s.name === 'public')
  const [objs, setObjs] = useState<Obj[] | null>(null)
  useEffect(() => {
    if (open && objs === null) void schema('objects', s.name).then(setObjs)
  }, [open, objs, s.name, schema])
  const groups = useMemo(() => {
    const g = new Map<string, Obj[]>()
    for (const o of objs ?? []) {
      const k = o.kind === 'procedure' ? 'function' : o.kind
      g.set(k, [...(g.get(k) ?? []), o])
    }
    return g
  }, [objs])
  const order = ['table', 'view', 'matview', 'function', 'sequence']
  const plural: Record<string, string> = { table: 'Tables', view: 'Views', matview: 'Materialized views', function: 'Functions', sequence: 'Sequences' }

  return (
    <Box>
      <UnstyledButton onClick={() => setOpen((o) => !o)} px={4} py={3} style={{ display: 'flex', alignItems: 'center', gap: 6, width: '100%', borderRadius: 6 }}>
        {open ? <IconChevronDown size={12} /> : <IconChevronRight size={12} />}
        <Text size={FS.body}>{s.name}</Text>
        {s.count ? <Text size={FS.micro} c="dimmed" ml="auto">{s.count}</Text> : null}
      </UnstyledButton>
      {open && (
        <Box pl="md">
          {objs === null && <Loader size={12} ml="sm" />}
          {order.filter((k) => groups.has(k)).map((k) => (
            <KindGroup key={k} label={plural[k] ?? k} items={groups.get(k)!} schemaName={s.name} schema={schema} onPick={onPick} onRun={onRun} />
          ))}
          {objs?.length === 0 && <Text size={FS.micro} c="dimmed" px={4}>empty, or nothing you may see</Text>}
        </Box>
      )}
    </Box>
  )
}

function KindGroup({ label, items, schemaName, schema, onPick, onRun }: {
  label: string; items: Obj[]; schemaName: string
  schema: (kind: string, schemaName?: string, object?: string) => Promise<Obj[]>
  onPick: (t: string) => void; onRun: (q: string) => void
}) {
  const [open, setOpen] = useState(label === 'Tables')
  const [limit, setLimit] = useState(50)
  return (
    <Box>
      <UnstyledButton onClick={() => setOpen((o) => !o)} px={4} py={3} style={{ display: 'flex', alignItems: 'center', gap: 6, width: '100%' }}>
        {open ? <IconChevronDown size={12} /> : <IconChevronRight size={12} />}
        <Text size={FS.meta}>{label}</Text>
        <Text size={FS.micro} c="dimmed" ml="auto">{items.length}</Text>
      </UnstyledButton>
      {open && (
        <Box pl="sm">
          {items.slice(0, limit).map((o) => (
            <ObjectNode key={o.name} o={o} schemaName={schemaName} schema={schema} onPick={onPick} onRun={onRun} />
          ))}
          {items.length > limit && (
            <UnstyledButton onClick={() => setLimit((l) => l + 200)} px={4} py={2}>
              <Text size={FS.micro} c="dimmed">… {items.length - limit} more</Text>
            </UnstyledButton>
          )}
        </Box>
      )}
    </Box>
  )
}

function ObjectNode({ o, schemaName, schema, onPick, onRun }: {
  o: Obj; schemaName: string
  schema: (kind: string, schemaName?: string, object?: string) => Promise<Obj[]>
  onPick: (t: string) => void; onRun: (q: string) => void
}) {
  const [open, setOpen] = useState(false)
  const [cols, setCols] = useState<Obj[] | null>(null)
  const Icon = KIND_ICON[o.kind] ?? IconTable
  const q = (n: string) => (/^[a-z_][a-z0-9_]*$/.test(n) ? n : `"${n.replace(/"/g, '""')}"`)
  const qualified = `${q(schemaName)}.${q(o.name)}`
  const expandable = o.kind === 'table' || o.kind === 'view' || o.kind === 'matview'
  useEffect(() => {
    if (open && expandable && cols === null) void schema('columns', schemaName, o.name).then(setCols)
  }, [open, expandable, cols, schemaName, o.name, schema])
  return (
    <Box>
      <Group gap={4} wrap="nowrap" px={4} py={2} style={{ borderRadius: 6 }}>
        <UnstyledButton onClick={() => expandable && setOpen((v) => !v)} style={{ display: 'flex', alignItems: 'center', gap: 4, flex: 1, minWidth: 0 }}
          onDoubleClick={() => onPick(qualified)} title={o.detail || qualified}>
          {expandable ? (open ? <IconChevronDown size={11} /> : <IconChevronRight size={11} />) : <span style={{ width: 11 }} />}
          <Icon size={12} style={{ color: 'var(--mantine-color-dimmed)', flexShrink: 0 }} />
          <Text size={FS.meta} ff="monospace" style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{o.name}</Text>
        </UnstyledButton>
        {expandable && (
          <Tooltip label="Select the first 100 rows" withArrow>
            <ActionIcon size="xs" variant="subtle" aria-label={`Select from ${o.name}`}
              onClick={() => onRun(`SELECT * FROM ${qualified} LIMIT 100;`)}>
              <IconPlayerPlay size={11} />
            </ActionIcon>
          </Tooltip>
        )}
      </Group>
      {open && (
        <Box pl="lg">
          {cols === null && <Loader size={10} />}
          {cols?.map((c) => (
            <UnstyledButton key={c.name} onClick={() => onPick(q(c.name))} px={4} py={1} style={{ display: 'block', width: '100%' }} title={c.detail}>
              <Text size={FS.micro} ff="monospace" style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                {c.name} <span style={{ color: 'var(--mantine-color-dimmed)' }}>{c.detail}</span>
              </Text>
            </UnstyledButton>
          ))}
        </Box>
      )}
    </Box>
  )
}
