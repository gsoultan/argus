import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  ActionIcon, Badge, Box, Button, CloseButton, Drawer, Group, Loader, Menu, ScrollArea, SegmentedControl, Stack, Tabs, Text,
  Textarea, TextInput, Tooltip, UnstyledButton,
} from '@mantine/core'
import { useDisclosure, useMediaQuery } from '@mantine/hooks'
import {
  IconChevronDown, IconChevronRight, IconDatabase, IconDownload, IconLock, IconMenu2, IconPlayerPlay,
  IconPlayerStop, IconTable, IconEye, IconFunction, IconHash, IconActivity, IconUsersGroup, IconPuzzle,
  IconArrowsExchange, IconArchive, IconDots, IconStar,
} from '@tabler/icons-react'
import { FS, SP, INK } from '~/theme'
import type { DBAccess, DBLevel, Database } from '~/types/domain'
import { useCompletion } from '~/components/Autocomplete'
import { catalogFrom, completeSQL, type SQLCatalog, type SQLColumn } from '~/lib/sqlComplete'
import {
  ActivityPanel, RolesPanel, type Admin, type AdminOp, type AdminResult, type Collect, type Collected,
} from '~/components/DatabaseAdmin'
import { ExtensionsPanel } from '~/components/DatabaseExtensions'
import { ReplicationPanel } from '~/components/DatabaseReplication'
import { BackupsPanel } from '~/components/DatabaseBackups'
import { ExplainView, planSummary, type PlanRun } from '~/components/ExplainView'
import { SchemaDiagram } from '~/components/SchemaDiagram'
import { SaveQueryDialog, SavedQueriesList } from '~/components/SavedQueries'
import { explainSQL, explainable, parsePlan } from '~/lib/explainPlan'
import {
  MAX_TABS, blankTab, loadEditorHeight, loadTabs, mayWrite, nameFromSQL, newKey, saveEditorHeight, saveTabs, txAfter,
  type QueryTab,
} from '~/lib/workspaceTabs'
import { notifyWarn } from '~/lib/notify'
import type { SavedQuery } from '~/lib/live'

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
- The editor's autocomplete reads names the same way as the tree, as the
  person's role: one "completion" answer for schemas, tables and functions,
  then a table's columns once a statement names it, kept for the session.
*/

export interface WorkspaceProps {
  database: Database
  access: DBAccess
  /** Fetches a ticket for one session; see LiveTerminal for why a callback. */
  getTicket: (level?: DBLevel) => Promise<{ ticket: string; role: string; level: DBLevel } | null>
  endpoint: string
  onAskForAccess?: () => void
  /** The person is an organization admin: the backups panel links them to Settings. */
  orgAdmin?: boolean
}

type Col = { name: string; type: string }
type Cell = string | null
type Obj = { name: string; kind: string; detail?: string; count?: number; schema?: string }

interface PGAnswer {
  id: number
  type: 'ready' | 'cols' | 'done' | 'error' | 'schema' | 'closed' | 'admin'
  cols?: Col[]
  tag?: string
  rows?: number
  ms?: number
  // Another part of this answer follows.
  more?: boolean
  truncated?: boolean
  error?: string
  code?: string
  position?: number
  detail?: string
  hint?: string
  objects?: Obj[]
  admin?: unknown
  role?: string
  level?: DBLevel
  version?: string
  session?: string
  via?: string
  levels?: Partial<Record<DBLevel, string>>
}

type Panel = 'query' | 'activity' | 'roles' | 'extensions' | 'replication' | 'backups'

interface Result {
  id: number
  sql: string
  cols: Col[]
  rows: Cell[][]
  done?: PGAnswer
  error?: PGAnswer
  startedAt: number
}

/** The request id a row batch belongs to: its first four bytes. */
function batchId(buf: ArrayBuffer): number {
  const b = new Uint8Array(buf)
  return ((b[0]! << 24) | (b[1]! << 16) | (b[2]! << 8) | b[3]!) >>> 0
}

/** Decodes one row batch: id, then cells as uvarint(len*2+cut+1) + bytes; 0 is NULL. */
function decodeBatch(buf: ArrayBuffer, ncols: number, dec: TextDecoder): { id: number; rows: Cell[][] } {
  const b = new Uint8Array(buf)
  const id = batchId(buf)
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

type Job =
  | { tab: string; kind: 'run'; text: string; maxRows: number }
  | { tab: string; kind: 'explain'; text: string; mode: PlanRun['mode'] }

const DEFAULT_H = { wide: 170, narrow: 110 }

export function DatabaseWorkspace({ database, access, getTicket, endpoint, onAskForAccess, orgAdmin }: WorkspaceProps) {
  const narrow = useMediaQuery('(max-width: 48em)')
  const [treeOpen, tree] = useDisclosure(false)
  const wsRef = useRef<WebSocket | null>(null)
  const nextId = useRef(1)
  const dec = useMemo(() => new TextDecoder(), [])
  const [status, setStatus] = useState<'connecting' | 'ready' | 'closed'>('connecting')
  const [fatal, setFatal] = useState<string | null>(null)
  const [ready, setReady] = useState<PGAnswer | null>(null)
  const [history, setHistory] = useState<{ sql: string; at: number; ok: boolean }[]>([])
  const pending = useRef(new Map<number, (a: PGAnswer) => void>())
  // Requests made before the session is ready (the tree asks for schemas on
  // mount) wait here and go out together on 'ready'.
  const queue = useRef<string[]>([])
  // Statements the administration panels run: collected whole, by request
  // id, without touching the editor's result.
  const collectors = useRef(new Map<number, Collected>())
  const [panel, setPanel] = useState<Panel>('query')
  const [view, setView] = useState<'query' | 'diagram'>('query')

  // Tabs: each its own editor text, row limit, result and plan. Text is
  // kept in this browser per database; results never are.
  const initial = useMemo(() => loadTabs(database.id), [database.id])
  const [tabs, setTabs] = useState<QueryTab[]>(initial.tabs)
  const [active, setActive] = useState(initial.active)
  const activeRef = useRef(active)
  activeRef.current = active
  const tab = tabs.find((t) => t.key === active) ?? tabs[0]!
  const sql = tab.sql
  useEffect(() => { saveTabs(database.id, tabs, active) }, [database.id, tabs, active])
  const [results, setResults] = useState<Record<string, Result | null>>({})
  const [plans, setPlans] = useState<Record<string, PlanRun>>({})
  const [shown, setShown] = useState<Record<string, 'results' | 'plan' | 'raw'>>({})
  // What each running request belongs to, so a row batch finds its tab.
  const live = useRef(new Map<number, { tab: string; r: Result }>())
  // One statement at a time on the session: the rest wait their turn.
  const [running, setRunning] = useState<{ id: number; tab: string } | null>(null)
  const runningRef = useRef<{ id: number; tab: string } | null>(null)
  const waiting = useRef<Job[]>([])
  const [waitingTabs, setWaitingTabs] = useState<string[]>([])
  // Whether the person's own statements left a transaction open.
  const txOpen = useRef(false)
  const [renaming, setRenaming] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  const result = results[tab.key] ?? null
  const plan = plans[tab.key]
  const resultView = shown[tab.key] ?? 'results'

  const patchTab = useCallback((key: string, f: (t: QueryTab) => QueryTab) => {
    setTabs((ts) => ts.map((t) => (t.key === key ? f(t) : t)))
  }, [])
  const setSql = useCallback((v: string) => {
    patchTab(activeRef.current, (t) => ({ ...t, sql: v, name: t.named ? t.name : (nameFromSQL(v) ?? t.name) }))
  }, [patchTab])

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
          const bid = batchId(ev.data)
          const c = collectors.current.get(bid)
          if (c) {
            c.rows.push(...decodeBatch(ev.data, c.cols.length, dec).rows)
            return
          }
          const e = live.current.get(bid)
          if (!e) return
          const { rows } = decodeBatch(ev.data, e.r.cols.length, dec)
          // Append in place and publish a new object once per batch, so React
          // re-renders per batch rather than per row.
          e.r.rows.push(...rows)
          setResults((prev) => ({ ...prev, [e.tab]: { ...e.r } }))
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
    waiting.current = []
    setWaitingTabs([])
    for (const [id, cb] of pending.current) cb({ id, type: 'closed', error: 'the session ended' })
    pending.current.clear()
    collectors.current.clear()
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
      // A long answer, such as a large database's catalogue, comes in parts.
      const parts: Obj[] = []
      pending.current.set(id, (a) => {
        if (a.type === 'schema') {
          parts.push(...(a.objects ?? []))
          if (a.more) return
        }
        pending.current.delete(id)
        resolve(a.type === 'schema' ? parts : [])
      })
      if (!send({ id, op: 'schema', kind, schema: schemaName, object })) {
        pending.current.delete(id)
        resolve([])
      }
    }), [send])

  // collect runs one statement for a panel and resolves with everything it
  // returned. Recorded like any other statement; it is one. onId hears the
  // request id, so a plan in progress can be cancelled.
  const collectWithId = useCallback((text: string, max = 1000, onId?: (id: number) => void) => new Promise<Collected>((resolve) => {
    const id = nextId.current++
    onId?.(id)
    const c: Collected = { cols: [], rows: [] }
    collectors.current.set(id, c)
    pending.current.set(id, (a) => {
      if (a.type === 'cols') {
        c.cols = a.cols ?? []
        return
      }
      pending.current.delete(id)
      collectors.current.delete(id)
      if (a.type !== 'done') c.error = { error: a.error, code: a.code }
      resolve(c)
    })
    if (!send({ id, op: 'run', sql: text, maxRows: max })) {
      pending.current.delete(id)
      collectors.current.delete(id)
      resolve({ cols: [], rows: [], error: { error: 'the session ended' } })
    }
  }), [send])
  const collect: Collect = useCallback((text, max) => collectWithId(text, max), [collectWithId])

  const startRef = useRef<(j: Job) => void>(() => {})
  const finished = useCallback(() => {
    runningRef.current = null
    setRunning(null)
    const next = waiting.current.shift()
    setWaitingTabs(waiting.current.map((j) => j.tab))
    if (next) startRef.current(next)
  }, [])

  const startRun = useCallback((job: Extract<Job, { kind: 'run' }>) => {
    const stmt = job.text
    const id = nextId.current++
    const r: Result = { id, sql: stmt, cols: [], rows: [], startedAt: Date.now() }
    live.current.set(id, { tab: job.tab, r })
    runningRef.current = { id, tab: job.tab }
    setRunning(runningRef.current)
    setResults((prev) => ({ ...prev, [job.tab]: r }))
    setShown((v) => ({ ...v, [job.tab]: 'results' }))
    pending.current.set(id, (a) => {
      if (a.type === 'cols') {
        r.cols = a.cols ?? []
        setResults((prev) => ({ ...prev, [job.tab]: { ...r } }))
        return
      }
      pending.current.delete(id)
      live.current.delete(id)
      if (a.type === 'done') r.done = a
      else r.error = a
      setResults((prev) => ({ ...prev, [job.tab]: { ...r } }))
      txOpen.current = txAfter(txOpen.current, stmt, a.type === 'done')
      setHistory((h) => [{ sql: stmt, at: Date.now(), ok: a.type === 'done' }, ...h].slice(0, 100))
      finished()
    })
    if (!send({ id, op: 'run', sql: stmt, maxRows: job.maxRows })) {
      pending.current.delete(id)
      live.current.delete(id)
      finished()
    }
  }, [send, finished])

  const startExplain = useCallback((job: Extract<Job, { kind: 'explain' }>) => {
    const key = job.tab
    runningRef.current = { id: -1, tab: key }
    setRunning(runningRef.current)
    setPlans((p) => ({ ...p, [key]: { mode: job.mode, plan: null, running: true } }))
    setShown((v) => ({ ...v, [key]: 'plan' }))
    const done = (run: Omit<PlanRun, 'mode' | 'running'>) => {
      setPlans((p) => ({ ...p, [key]: { mode: job.mode, running: false, ...run } }))
      finished()
    }
    void (async () => {
      const track = (id: number) => {
        runningRef.current = { id, tab: key }
        setRunning(runningRef.current)
      }
      // ANALYZE runs the statement. One that may write runs inside a
      // transaction that is rolled back, so nothing it does stays; inside the
      // person's own transaction that would roll back their work too, so it
      // is refused instead.
      const wrap = job.mode !== 'plan' && mayWrite(job.text)
      if (wrap && txOpen.current) {
        done({ plan: null, error: 'You are inside a transaction of your own. Finish it with COMMIT or ROLLBACK first, or use Explain, which runs nothing.' })
        return
      }
      if (wrap) {
        const b = await collectWithId('BEGIN', 1, track)
        if (b.error) {
          done({ plan: null, error: b.error.error ?? 'could not start a transaction to roll back' })
          return
        }
      }
      const stmt = explainSQL(job.text, job.mode)
      const r = await collectWithId(stmt, 20000, track)
      if (wrap) await collectWithId('ROLLBACK', 1, track)
      setHistory((h) => [{ sql: stmt, at: Date.now(), ok: !r.error }, ...h].slice(0, 100))
      if (r.error) {
        done({ plan: null, error: `Postgres refused this: ${r.error.error ?? 'unknown error'}${r.error.code ? ` (${r.error.code})` : ''}` })
        return
      }
      done({ plan: parsePlan(r.rows.map((x) => x[0] ?? '')), rolledBack: wrap })
    })()
  }, [collectWithId, finished])

  startRef.current = (j: Job) => (j.kind === 'run' ? startRun(j) : startExplain(j))
  const enqueue = useCallback((j: Job) => {
    if (runningRef.current) {
      // One waiting job per tab: pressing Run again replaces it.
      waiting.current = [...waiting.current.filter((w) => w.tab !== j.tab), j]
      setWaitingTabs(waiting.current.map((w) => w.tab))
      return
    }
    startRef.current(j)
  }, [])

  const run = useCallback((text: string, key = activeRef.current) => {
    const stmt = text.trim()
    if (!stmt) return
    const t = tabs.find((x) => x.key === key)
    enqueue({ tab: key, kind: 'run', text: stmt, maxRows: t?.maxRows ?? 1000 })
  }, [enqueue, tabs])

  // admin sends one administrative operation to the agent's helper. The
  // agent records a mutation (redacted) before running it and refuses the
  // whole thing below dba, whatever this console asked.
  const admin = useCallback(<T,>(op: AdminOp) => new Promise<AdminResult<T>>((resolve) => {
    const id = nextId.current++
    pending.current.set(id, (a) => {
      pending.current.delete(id)
      if (a.type === 'admin') resolve({ data: a.admin as T, tag: a.tag, ms: a.ms })
      else resolve({ error: { error: a.error, code: a.code, detail: a.detail, hint: a.hint } })
    })
    if (!send({ id, op: 'admin', admin: op })) {
      pending.current.delete(id)
      resolve({ error: { error: 'the session ended' } })
    }
  }), [send]) as Admin

  const cancel = useCallback(() => {
    const r = runningRef.current
    if (r && r.id > 0) send({ id: r.id, op: 'cancel' })
    // What was waiting for this tab goes too.
    waiting.current = waiting.current.filter((w) => w.tab !== activeRef.current)
    setWaitingTabs(waiting.current.map((w) => w.tab))
  }, [send])

  const editorRef = useRef<HTMLTextAreaElement | null>(null)

  // Autocomplete. The catalogue is asked for once per session; an agent too
  // old to answer it leaves keywords and built-in functions, which need none.
  const [catalog, setCatalog] = useState<SQLCatalog | null>(null)
  useEffect(() => { void schema('completion').then((o) => setCatalog(catalogFrom(o))) }, [schema])
  // Columns by `schema.table`, loaded when a statement names the table.
  // Bounded: the keys come from the catalogue, but a long session can name
  // a great many tables.
  const columns = useRef(new Map<string, SQLColumn[] | 'loading'>())
  const [columnsVersion, setColumnsVersion] = useState(0)
  const columnsOf = useCallback((sch: string, name: string) => {
    const c = columns.current.get(`${sch}.${name}`)
    return c === 'loading' ? undefined : c
  }, [])
  const loadColumns = useCallback((sch: string, name: string) => {
    const key = `${sch}.${name}`
    if (columns.current.has(key)) return
    if (columns.current.size >= 500) columns.current.delete(columns.current.keys().next().value!)
    columns.current.set(key, 'loading')
    void schema('columns', sch, name).then((objs) => {
      columns.current.set(key, objs.filter((o) => !o.detail?.includes('hidden'))
        .map((o) => ({ name: o.name, type: (o.detail ?? '').split(' · ')[0]!.replace(/ not null$/, '') })))
      setColumnsVersion((v) => v + 1)
    })
  }, [schema])
  const completeHere = useCallback((text: string, cursor: number, explicit: boolean) => {
    const c = completeSQL(text, cursor, catalog, columnsOf, explicit)
    for (const n of c?.needs ?? []) loadColumns(n.schema, n.name)
    return c
  }, [catalog, columnsOf, loadColumns])
  const completion = useCompletion({
    ref: editorRef, value: sql, setValue: setSql, complete: completeHere, refresh: columnsVersion,
  })
  const selectionOrAll = useCallback(() => {
    const el = editorRef.current
    return el && el.selectionStart !== el.selectionEnd ? el.value.slice(el.selectionStart, el.selectionEnd) : sql
  }, [sql])
  const runSelectionOrAll = useCallback(() => run(selectionOrAll()), [run, selectionOrAll])
  const explain = useCallback((mode: PlanRun['mode']) => {
    const text = selectionOrAll().trim()
    if (!text) return
    if (!explainable(text)) {
      setPlans((p) => ({ ...p, [activeRef.current]: { mode, plan: null, running: false,
        error: 'EXPLAIN takes a SELECT, INSERT, UPDATE, DELETE, MERGE, VALUES or EXECUTE. Select just that statement and try again.' } }))
      setShown((v) => ({ ...v, [activeRef.current]: 'plan' }))
      return
    }
    enqueue({ tab: activeRef.current, kind: 'explain', text, mode })
  }, [enqueue, selectionOrAll])

  const onKey = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (completion.onKeyDown(e)) return
    if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
      e.preventDefault()
      runSelectionOrAll()
    }
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 's') {
      e.preventDefault()
      if (sql.trim()) setSaving(true)
    }
  }

  const insert = (text: string) => {
    const el = editorRef.current
    if (!el) {
      setSql(sql + text)
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

  /* Tabs */
  const tabsRef = useRef(tabs)
  tabsRef.current = tabs
  const openTab = useCallback((init?: Partial<QueryTab>) => {
    const ts = tabsRef.current
    if (ts.length >= MAX_TABS) {
      notifyWarn('Too many tabs', `Close one first; a database keeps up to ${MAX_TABS}.`)
      return ''
    }
    const t = { ...blankTab(ts, init?.sql ?? '', init?.name), ...init, key: newKey() }
    tabsRef.current = [...ts, t]
    setTabs(tabsRef.current)
    setActive(t.key)
    activeRef.current = t.key
    return t.key
  }, [])
  const closeTab = (key: string) => {
    if (running?.tab === key) return
    waiting.current = waiting.current.filter((w) => w.tab !== key)
    setWaitingTabs(waiting.current.map((w) => w.tab))
    setResults(({ [key]: _r, ...rest }) => rest)
    setPlans(({ [key]: _p, ...rest }) => rest)
    setTabs((ts) => {
      const i = ts.findIndex((t) => t.key === key)
      const left = ts.filter((t) => t.key !== key)
      const next = left.length ? left : [blankTab([])]
      if (key === activeRef.current) setActive(next[Math.max(0, Math.min(i, next.length - 1))]!.key)
      return next
    })
  }
  const openSaved = (q: SavedQuery) => {
    const had = tabs.find((t) => t.savedId && t.savedId === q.id)
    if (had) setActive(had.key)
    else openTab({ name: q.name, named: true, sql: q.sql, savedId: q.mine ? q.id : undefined, savedSql: q.mine ? q.sql : undefined })
    setPanel('query')
    setView('query')
    if (narrow) tree.close()
  }

  /* Editor height: dragged, double-click resets, kept in this browser. */
  const defaultH = narrow ? DEFAULT_H.narrow : DEFAULT_H.wide
  const [editorH, setEditorH] = useState(() => loadEditorHeight(defaultH))
  const area = useRef<HTMLDivElement | null>(null)
  const dragH = useRef<{ y: number; h: number } | null>(null)
  const [dragging, setDragging] = useState(false)
  const onGripDown = (e: React.PointerEvent) => {
    e.preventDefault()
    ;(e.currentTarget as Element).setPointerCapture(e.pointerId)
    dragH.current = { y: e.clientY, h: editorH }
    setDragging(true)
  }
  const onGripMove = (e: React.PointerEvent) => {
    const d = dragH.current
    if (!d) return
    const room = narrow ? window.innerHeight - 220 : (area.current?.clientHeight ?? 600) - 180
    setEditorH(Math.round(Math.max(80, Math.min(Math.max(120, room), d.h + e.clientY - d.y))))
  }
  const onGripUp = () => {
    if (!dragH.current) return
    dragH.current = null
    setDragging(false)
    saveEditorHeight(editorH)
  }
  const onGripKey = (e: React.KeyboardEvent) => {
    if (e.key === 'ArrowUp' || e.key === 'ArrowDown') {
      e.preventDefault()
      const h = Math.max(80, editorH + (e.key === 'ArrowUp' ? -20 : 20))
      setEditorH(h)
      saveEditorHeight(h)
    }
  }

  const [schemaNames, setSchemaNames] = useState<string[] | null>(null)
  useEffect(() => {
    if (view === 'diagram' && schemaNames === null) void schema('schemas').then((o) => setSchemaNames(o.map((s) => s.name)))
  }, [view, schemaNames, schema])

  const level = ready?.level ?? access.level
  const bar = (
    <LeftBar
      database={database}
      level={level}
      schema={schema}
      onPick={(q) => { setView('query'); insert(q); if (narrow) tree.close() }}
      onRun={(q) => { setSql(q); setPanel('query'); setView('query'); run(q); if (narrow) tree.close() }}
      history={history}
      panel={panel}
      onPanel={(p) => { setPanel(p); setView('query'); if (narrow) tree.close() }}
      onOpenSaved={openSaved}
      orgAdmin={Boolean(orgAdmin)}
    />
  )
  const adminProps = {
    collect, admin, level, levels: ready?.levels ?? {}, self: ready?.role ?? '', narrow,
    onBack: () => setPanel('query'), onAskForAccess,
  }
  const busyElsewhere = running !== null && running.tab !== tab.key
  const waitingHere = waitingTabs.includes(tab.key)
  const runningHere = running?.tab === tab.key
  const runningName = running ? tabs.find((t) => t.key === running.tab)?.name ?? 'another tab' : ''
  const dirty = Boolean(tab.savedId) && tab.sql !== tab.savedSql
  const canRun = status === 'ready' && Boolean(sql.trim()) && !runningHere

  const tabStrip = (
    <Group gap={2} px={narrow ? 6 : 'sm'} pt={6} wrap="nowrap" align="flex-end"
      style={{ borderBottom: '1px solid var(--color-line)', background: 'var(--color-void)', overflowX: 'auto', scrollbarWidth: 'none' }}
      role="tablist" aria-label="Query tabs">
      {tabs.map((t) => {
        const on = t.key === tab.key
        const r = results[t.key]
        const p = plans[t.key]
        const isRunning = running?.tab === t.key
        const failed = !isRunning && (Boolean(r?.error) || Boolean(p?.error))
        const changed = Boolean(t.savedId) && t.sql !== t.savedSql
        return (
          <Group key={t.key} gap={6} wrap="nowrap" role="tab" aria-selected={on} tabIndex={0}
            onClick={() => setActive(t.key)} onKeyDown={(e) => { if (e.key === 'Enter') setActive(t.key) }}
            onDoubleClick={() => setRenaming(t.key)}
            px={narrow ? 9 : 12} py={7}
            style={{ flexShrink: 0, maxWidth: narrow ? 160 : 230, cursor: 'pointer', borderRadius: '7px 7px 0 0',
              border: '1px solid', borderColor: on ? 'var(--color-line)' : 'transparent', borderBottom: 0,
              marginBottom: on ? -1 : 0, background: on ? 'var(--color-surface)' : undefined }}>
            {isRunning && <Box w={8} h={8} style={{ borderRadius: 4, background: 'var(--color-brand)', boxShadow: '0 0 0 3px var(--mantine-color-azure-light)', flexShrink: 0 }} aria-label="running" />}
            {!isRunning && waitingTabs.includes(t.key) && <Loader size={10} />}
            {failed && <Box w={8} h={8} style={{ borderRadius: 4, background: 'var(--mantine-color-rose-6)', flexShrink: 0 }} aria-label="failed" />}
            {renaming === t.key ? (
              <TextInput size="xs" autoFocus defaultValue={t.name} w={140} onClick={(e) => e.stopPropagation()}
                onBlur={(e) => { const v = e.currentTarget.value.trim(); if (v) patchTab(t.key, (x) => ({ ...x, name: v.slice(0, 60), named: true })); setRenaming(null) }}
                onKeyDown={(e) => { if (e.key === 'Enter') e.currentTarget.blur(); if (e.key === 'Escape') setRenaming(null) }}
                aria-label="Tab name" />
            ) : (
              <Text size={FS.meta} fw={on ? 500 : 400} c={on ? undefined : 'dimmed'} style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
                title={narrow ? t.name : `${t.name} · double-click to rename`}>
                {t.name}{changed ? ' ●' : ''}
              </Text>
            )}
            {!narrow && r?.done && !isRunning && (
              <Badge size="xs" variant="light" color="slate" radius="xl" style={{ flexShrink: 0 }}>{r.rows.length.toLocaleString()}</Badge>
            )}
            {(on || !narrow) && tabs.length > 1 && (
              <CloseButton size="xs" aria-label={`Close ${t.name}`} disabled={isRunning}
                onClick={(e) => { e.stopPropagation(); closeTab(t.key) }} />
            )}
          </Group>
        )
      })}
      <Tooltip label={tabs.length >= MAX_TABS ? `Up to ${MAX_TABS} tabs` : 'New tab'} withArrow>
        <UnstyledButton onClick={() => openTab()} px={10} py={7} disabled={tabs.length >= MAX_TABS}
          style={{ flexShrink: 0, color: INK.brand, fontWeight: 600, fontSize: 13, position: narrow ? 'sticky' : undefined, right: 0,
            background: narrow ? 'var(--color-void)' : undefined, marginLeft: narrow ? 'auto' : undefined }}
          aria-label="New tab">
          {narrow ? '+' : '+ New tab'}
        </UnstyledButton>
      </Tooltip>
    </Group>
  )

  const rowLimit = (
    <select value={tab.maxRows} onChange={(e) => { const n = Number(e.currentTarget.value); patchTab(tab.key, (t) => ({ ...t, maxRows: n })) }}
      style={{ fontSize: 12, padding: '2px 6px', borderRadius: 6, border: '1px solid var(--color-line)', background: 'var(--color-surface)', color: 'inherit' }}
      aria-label="Row limit">
      {[100, 1000, 10000, 100000].map((n) => <option key={n} value={n}>{n.toLocaleString()} rows</option>)}
    </select>
  )
  const explainMenu = (
    <Menu position="bottom-start" withinPortal width={280}>
      <Menu.Target>
        <Button size="xs" variant="default" rightSection={<IconChevronDown size={12} />} disabled={status !== 'ready' || !sql.trim() || runningHere}>
          Explain
        </Button>
      </Menu.Target>
      <Menu.Dropdown>
        <Menu.Item onClick={() => explain('plan')}>
          <Text size={FS.meta} fw={600}>Explain</Text>
          <Text size={FS.micro} c="dimmed">Shows the plan. Runs nothing.</Text>
        </Menu.Item>
        <Menu.Item onClick={() => explain('analyze')}>
          <Text size={FS.meta} fw={600}>Explain analyze</Text>
          <Text size={FS.micro} c="dimmed">Runs it to time each step. A statement that writes is rolled back.</Text>
        </Menu.Item>
        <Menu.Item onClick={() => explain('buffers')}>
          <Text size={FS.meta} fw={600}>Explain analyze, buffers</Text>
          <Text size={FS.micro} c="dimmed">Also shows cache hits and disk reads.</Text>
        </Menu.Item>
      </Menu.Dropdown>
    </Menu>
  )
  const runLabel = runningHere ? 'Running…' : busyElsewhere ? (waitingHere ? `Waiting for ${runningName}` : `Run after ${runningName}`) : 'Run'

  const toolbar = (
    <Group gap="xs" px={narrow ? 'sm' : 'md'} py={SP.cozy} wrap={narrow ? 'nowrap' : 'wrap'} style={{ borderBottom: '1px solid var(--color-line)' }}>
      <Button size="xs" leftSection={runningHere ? <Loader size={12} color="white" /> : <IconPlayerPlay size={14} />} onClick={runSelectionOrAll}
        disabled={!canRun} style={narrow ? { flex: 1, minWidth: 0 } : undefined}
        styles={{ label: { overflow: 'hidden', textOverflow: 'ellipsis' } }}>
        {runLabel}
      </Button>
      {explainMenu}
      {narrow ? (
        <>
          <ActionIcon size={30} variant="default" onClick={() => setSaving(true)} disabled={!sql.trim()} aria-label="Save query">
            <IconStar size={14} />
          </ActionIcon>
          <Menu position="bottom-end" withinPortal>
            <Menu.Target>
              <ActionIcon size={30} variant="default" aria-label="More"><IconDots size={14} /></ActionIcon>
            </Menu.Target>
            <Menu.Dropdown>
              <Menu.Item leftSection={<IconPlayerStop size={14} />} onClick={cancel} disabled={!runningHere && !waitingHere}>Cancel</Menu.Item>
              <Menu.Item leftSection={<IconDownload size={14} />} onClick={exportCsv} disabled={!result || result.rows.length === 0}>Export CSV</Menu.Item>
              <Menu.Divider />
              <Box px="sm" py={6}><Group gap={6}><Text size={FS.micro} c="dimmed">Show up to</Text>{rowLimit}</Group></Box>
            </Menu.Dropdown>
          </Menu>
        </>
      ) : (
        <>
          <Button size="xs" variant="default" leftSection={<IconStar size={14} />} onClick={() => setSaving(true)} disabled={!sql.trim()}>
            {dirty ? 'Save changes' : 'Save'}
          </Button>
          <Button size="xs" variant="default" leftSection={<IconPlayerStop size={14} />} onClick={cancel}
            disabled={!runningHere && !waitingHere}>
            Cancel
          </Button>
          <Group gap={4} wrap="nowrap">
            <Text size={FS.micro} c="dimmed">Show up to</Text>
            {rowLimit}
          </Group>
          {busyElsewhere && (
            <Group gap={6} wrap="nowrap">
              <Loader size={12} />
              <Text size={FS.micro} c={INK.brand}>{runningName} is running</Text>
            </Group>
          )}
          <Button size="xs" variant="subtle" ml="auto" leftSection={<IconDownload size={14} />} onClick={exportCsv}
            disabled={!result || result.rows.length === 0}>
            Export CSV
          </Button>
        </>
      )}
    </Group>
  )

  const grip = (
    <Box role="separator" aria-orientation="horizontal" aria-label="Resize the editor" tabIndex={0}
      onPointerDown={onGripDown} onPointerMove={onGripMove} onPointerUp={onGripUp} onPointerCancel={onGripUp}
      onDoubleClick={() => { setEditorH(defaultH); saveEditorHeight(null) }} onKeyDown={onGripKey}
      title="Drag to resize · double-click to reset"
      style={{ height: narrow ? 16 : 9, flexShrink: 0, cursor: 'row-resize', touchAction: 'none', display: 'flex', alignItems: 'center', justifyContent: 'center',
        background: dragging ? 'var(--mantine-color-azure-light)' : 'var(--color-void)', borderBottom: '1px solid var(--color-line)' }}>
      <Box w={36} h={3} style={{ borderRadius: 2, background: dragging ? 'var(--color-brand)' : 'var(--mantine-color-dimmed)', opacity: dragging ? 1 : 0.6 }} />
    </Box>
  )

  const resultTabs = plan && (
    <Group gap="md" px={narrow ? 'sm' : 'md'} wrap="nowrap" style={{ borderBottom: '1px solid var(--color-line)' }}>
      {(['results', 'plan', 'raw'] as const).map((v) => (
        <UnstyledButton key={v} onClick={() => setShown((s) => ({ ...s, [tab.key]: v }))} py={8}
          style={{ borderBottom: resultView === v ? '2px solid var(--color-brand)' : '2px solid transparent', marginBottom: -1 }}>
          <Text size={FS.meta} fw={resultView === v ? 600 : 400} c={resultView === v ? undefined : 'dimmed'}>
            {v === 'results' ? 'Results' : v === 'plan' ? 'Plan' : 'Raw'}
          </Text>
        </UnstyledButton>
      ))}
      <Text size={FS.micro} c="dimmed" ml="auto" style={{ whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>
        {plan.running ? 'Asking for the plan…' : planSummary(plan)}
      </Text>
    </Group>
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

      <Box ref={area} style={{ flex: 1, minWidth: 0, display: 'flex', flexDirection: 'column' }}>
        <Group gap="xs" px={narrow ? 'sm' : 'md'} py={SP.cozy} wrap="nowrap" style={{ borderBottom: '1px solid var(--color-line)' }}>
          {narrow && (
            <ActionIcon variant="subtle" onClick={tree.open} aria-label="Open the object tree">
              <IconMenu2 size={18} />
            </ActionIcon>
          )}
          {!narrow && <IconDatabase size={16} color="var(--color-brand)" />}
          <Text fw={650} size={FS.body} style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{database.dbname}</Text>
          {!narrow && <Text size={FS.micro} c="dimmed" ff="monospace" style={{ whiteSpace: 'nowrap' }}>{database.hostname}{ready?.version ? ` · Postgres ${ready.version.split(' ')[0]}` : ''}{ready?.via === 'pgbouncer' ? ' · via PgBouncer' : ''}</Text>}
          <Badge variant="light" color={{ production: 'rose', staging: 'amber', development: 'teal' }[database.label]} radius="sm" style={{ flexShrink: 0 }}>
            {narrow ? database.label.slice(0, 4) : database.label}
          </Badge>
          <SegmentedControl size="xs" ml={narrow ? 'auto' : 'sm'} value={view} onChange={(v) => { setView(v as 'query' | 'diagram'); setPanel('query') }}
            data={[{ label: 'Query', value: 'query' }, { label: 'Diagram', value: 'diagram' }]} aria-label="View" />
          {!narrow && (
            <Group gap={6} ml="auto" wrap="nowrap">
              <Badge variant="light" color={level === 'read' ? 'slate' : 'azure'} radius="sm">
                You can: {level ? level.toUpperCase() : '…'}
              </Badge>
              {(access.approvable || onAskForAccess) && level === 'read' && (
                <Button size="xs" variant="default" onClick={onAskForAccess} leftSection={<IconLock size={12} />}>
                  Ask for write access
                </Button>
              )}
              <Badge variant="light" color="rose" radius="sm" leftSection={<span style={{ display: 'inline-block', width: 6, height: 6, borderRadius: 3, background: 'currentColor' }} />}>
                {status === 'ready' ? 'Recorded' : status === 'connecting' ? 'Connecting' : 'Closed'}
              </Badge>
            </Group>
          )}
        </Group>
        {narrow && (
          <Group gap={6} px="sm" py={6} wrap="nowrap" style={{ borderBottom: '1px solid var(--color-line)' }}>
            <Badge variant="light" color={level === 'read' ? 'slate' : 'azure'} radius="sm">You can: {level ? level.toUpperCase() : '…'}</Badge>
            {(access.approvable || onAskForAccess) && level === 'read' && (
              <Button size="compact-xs" variant="default" onClick={onAskForAccess} leftSection={<IconLock size={12} />}>Ask</Button>
            )}
            <Badge variant="light" color="rose" radius="sm" ml="auto">{status === 'ready' ? 'Recorded' : status === 'connecting' ? 'Connecting' : 'Closed'}</Badge>
          </Group>
        )}

        {fatal && (
          <Box px="md" py={SP.cozy} style={{ background: 'var(--mantine-color-rose-light)', color: 'var(--mantine-color-rose-light-color)' }}>
            <Text size={FS.meta}>{fatal}</Text>
          </Box>
        )}

        {view === 'diagram' && (
          <Box style={{ flex: 1, minHeight: narrow ? 0 : 0 }}>
            {schemaNames === null ? <Loader size={14} m="md" /> : (
              <SchemaDiagram databaseId={database.id} schemas={schemaNames} collect={collect} narrow={narrow}
                onQuery={(text, name) => { const k = openTab({ sql: text, name, named: true }); setView('query'); if (k) run(text, k) }} />
            )}
          </Box>
        )}
        {view === 'query' && panel === 'activity' && <ActivityPanel {...adminProps} />}
        {view === 'query' && panel === 'roles' && <RolesPanel {...adminProps} />}
        {view === 'query' && panel === 'extensions' && <ExtensionsPanel {...adminProps} />}
        {view === 'query' && panel === 'replication' && <ReplicationPanel {...adminProps} />}
        {view === 'query' && panel === 'backups' && <BackupsPanel database={database} level={level} narrow={narrow} onBack={() => setPanel('query')} admin={Boolean(orgAdmin)} />}
        {view === 'query' && panel === 'query' && <>
        {tabStrip}
        <Box style={{ flexShrink: 0 }}>
          <Textarea
            ref={editorRef}
            value={sql}
            {...completion.editorProps}
            onChange={(e) => { completion.onTyped(); setSql(e.currentTarget.value) }}
            onKeyDown={onKey}
            placeholder={narrow ? 'SELECT …' : 'SELECT … (⌘/Ctrl+Enter runs; select part of it to run only that; Ctrl+Space suggests; Ctrl+S saves)'}
            spellCheck={false}
            autoCapitalize="off"
            autoCorrect="off"
            aria-label="SQL"
            styles={{ input: { fontFamily: 'var(--font-mono)', fontSize: narrow ? 12 : 13, lineHeight: '21px', border: 0, borderRadius: 0,
              height: editorH, minHeight: 80, resize: 'none' } }}
          />
          {completion.popup}
        </Box>
        {grip}
        {toolbar}

        <Box style={{ flex: 1, minHeight: narrow ? 320 : 0, display: 'flex', flexDirection: 'column' }}>
          {resultTabs}
          {plan && resultView !== 'results'
            ? <ExplainView run={plan} raw={resultView === 'raw'} narrow={narrow} />
            : <Results result={result} narrow={narrow} />}
        </Box>
        </>}
      </Box>
      {saving && (
        <SaveQueryDialog opened={saving} onClose={() => setSaving(false)} databaseId={database.id} databaseName={database.dbname}
          initial={{ id: tab.savedId, name: tab.name, sql: tab.sql, shared: false }}
          onSaved={(q) => patchTab(tab.key, (t) => ({ ...t, name: q.name, named: true, savedId: q.id, savedSql: q.sql }))} />
      )}
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

function LeftBar({ database, level, schema, onPick, onRun, history, panel, onPanel, onOpenSaved, orgAdmin }: {
  database: Database
  level: DBLevel | ''
  schema: (kind: string, schemaName?: string, object?: string) => Promise<Obj[]>
  onPick: (text: string) => void
  onRun: (sql: string) => void
  history: { sql: string; at: number; ok: boolean }[]
  panel: Panel
  onPanel: (p: Panel) => void
  onOpenSaved: (q: SavedQuery) => void
  orgAdmin: boolean
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
          <Tabs.Tab value="saved" fz={FS.meta}>Saved</Tabs.Tab>
          <Tabs.Tab value="history" fz={FS.meta}>History</Tabs.Tab>
        </Tabs.List>
      </Tabs>
      {tab === 'saved' && <SavedQueriesList databaseId={database.id} onOpen={onOpenSaved} admin={orgAdmin} />}
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
            <UnstyledButton onClick={() => onPanel(panel === 'activity' ? 'query' : 'activity')} px={4} py={4}
              style={{ display: 'flex', alignItems: 'center', gap: 6, width: '100%', borderRadius: 6, background: panel === 'activity' ? 'var(--color-raised)' : undefined }}
              aria-pressed={panel === 'activity'}>
              <IconActivity size={13} /><Text size={FS.meta}>Activity &amp; locks</Text>
            </UnstyledButton>
            <Tooltip label={level === 'dba' ? 'Who holds which level; give or take a level from an application role' : 'Needs DBA access'} withArrow>
              <UnstyledButton onClick={() => onPanel(panel === 'roles' ? 'query' : 'roles')} px={4} py={4}
                style={{ display: 'flex', alignItems: 'center', gap: 6, width: '100%', borderRadius: 6, background: panel === 'roles' ? 'var(--color-raised)' : undefined, opacity: level === 'dba' ? 1 : .7 }}
                aria-pressed={panel === 'roles'}>
                <IconUsersGroup size={13} /><Text size={FS.meta}>Roles &amp; access</Text>
                <Badge size="xs" variant="light" color="slate" ml="auto">DBA</Badge>
              </UnstyledButton>
            </Tooltip>
            <Tooltip label={level === 'dba' ? 'Installed and available extensions; install, update, remove' : 'Needs DBA access'} withArrow>
              <UnstyledButton onClick={() => onPanel(panel === 'extensions' ? 'query' : 'extensions')} px={4} py={4}
                style={{ display: 'flex', alignItems: 'center', gap: 6, width: '100%', borderRadius: 6, background: panel === 'extensions' ? 'var(--color-raised)' : undefined, opacity: level === 'dba' ? 1 : .7 }}
                aria-pressed={panel === 'extensions'}>
                <IconPuzzle size={13} /><Text size={FS.meta}>Extensions</Text>
                <Badge size="xs" variant="light" color="slate" ml="auto">DBA</Badge>
              </UnstyledButton>
            </Tooltip>
            <Tooltip label={level === 'dba' ? 'Publications and subscriptions: logical replication in and out of this database' : 'Needs DBA access'} withArrow>
              <UnstyledButton onClick={() => onPanel(panel === 'replication' ? 'query' : 'replication')} px={4} py={4}
                style={{ display: 'flex', alignItems: 'center', gap: 6, width: '100%', borderRadius: 6, background: panel === 'replication' ? 'var(--color-raised)' : undefined, opacity: level === 'dba' ? 1 : .7 }}
                aria-pressed={panel === 'replication'}>
                <IconArrowsExchange size={13} /><Text size={FS.meta}>Replication</Text>
                <Badge size="xs" variant="light" color="slate" ml="auto">DBA</Badge>
              </UnstyledButton>
            </Tooltip>
            <UnstyledButton onClick={() => onPanel(panel === 'backups' ? 'query' : 'backups')} px={4} py={4}
              style={{ display: 'flex', alignItems: 'center', gap: 6, width: '100%', borderRadius: 6, background: panel === 'backups' ? 'var(--color-raised)' : undefined }}
              aria-pressed={panel === 'backups'}>
              <IconArchive size={13} /><Text size={FS.meta}>Backups</Text>
            </UnstyledButton>
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

/** The hover text for an object: its size and estimated rows for a table. */
function objectTitle(o: Obj, qualified: string): string {
  if (o.kind === 'function' || o.kind === 'procedure') return o.detail ? `${o.name}(${o.detail})` : qualified
  const parts = [qualified]
  if (o.detail) parts.push(o.detail)
  if (o.count && (o.kind === 'table' || o.kind === 'matview')) parts.push(`about ${o.count.toLocaleString()} rows`)
  return parts.join(' · ')
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
          onDoubleClick={() => onPick(qualified)} title={objectTitle(o, qualified)}>
          {expandable ? (open ? <IconChevronDown size={11} /> : <IconChevronRight size={11} />) : <span style={{ width: 11 }} />}
          <Icon size={12} style={{ color: 'var(--mantine-color-dimmed)', flexShrink: 0 }} />
          <Text size={FS.meta} ff="monospace" style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{o.name}</Text>
          {o.detail && o.kind !== 'function' && o.kind !== 'procedure' && (
            <Text size={FS.micro} c="dimmed" ml="auto" style={{ whiteSpace: 'nowrap', flexShrink: 0 }}>{o.detail}</Text>
          )}
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
