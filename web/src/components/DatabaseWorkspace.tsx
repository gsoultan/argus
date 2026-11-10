import { Suspense, createContext, lazy, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { meQuery } from '~/lib/queries'
import {
  ActionIcon, Badge, Box, Button, CloseButton, Drawer, Group, Loader, Menu, ScrollArea, Stack, Tabs, Text,
  TextInput, Tooltip, UnstyledButton,
} from '@mantine/core'
import { useDisclosure, useMediaQuery } from '@mantine/hooks'
import {
  IconChevronDown, IconChevronRight, IconDownload, IconLock, IconMenu2, IconPlayerPlay,
  IconPlayerStop, IconTable, IconEye, IconFunction, IconHash, IconActivity, IconUsersGroup, IconPuzzle,
  IconArrowsExchange, IconArchive, IconDots, IconStar, IconIndentIncrease, IconTrash, IconHistory,
  IconAdjustmentsHorizontal, IconSettings,
} from '@tabler/icons-react'
import { Link } from '@tanstack/react-router'
import { EngineMark } from '~/components/EngineMark'
import { FS, SP, INK } from '~/theme'
import type { DBAccess, DBLevel, Database } from '~/types/domain'
import { ResultsGrid } from '~/components/ResultsGrid'
import type { SaveResult } from '~/components/RowEdits'
import type { Stmt } from '~/lib/rowEdit'
import { TrashNotice, TrashPanel, type TrashItem } from '~/components/DatabaseTrash'
import { QueryParams } from '~/components/QueryParams'
import { engineVersion } from '~/lib/engines'
import { SqlEditor, pgErrorOffset, type EditorError, type SqlEditorHandle } from '~/components/SqlEditor'
import { bindParams, originalOffset, paramNames, type Bound } from '~/lib/sqlParams'
import { catalogFrom, completeSQL, type SQLCatalog, type SQLColumn } from '~/lib/sqlComplete'
import {
  ActivityPanel, RolesPanel, type Admin, type AdminOp, type AdminResult, type Collect, type Collected,
} from '~/components/DatabaseAdmin'
import { ExtensionsPanel } from '~/components/DatabaseExtensions'
import { ReplicationPanel } from '~/components/DatabaseReplication'
import { BackupsPanel } from '~/components/DatabaseBackups'
import { ExplainView, planSummary, type PlanRun } from '~/components/ExplainView'
import { SchemaDiagram } from '~/components/SchemaDiagram'
import { WriteCheckDialog, dropFactsSQL, type PGCheck, type WriteCheckState } from '~/components/WriteCheck'
import { GUARD_DROP_DB, needsCheck, type PGGuard } from '~/lib/sqlClass'
import { splitSQL } from '~/lib/sqlSplit'
import { SaveQueryDialog, SavedQueriesList } from '~/components/SavedQueries'
import { explainSQL, explainable, parsePlan } from '~/lib/explainPlan'
import {
  MAX_TABS, blankTab, loadEditorHeight, loadTabs, mayWrite, nameFromSQL, newKey, saveEditorHeight, saveTabs, txAfter,
  type QueryTab,
} from '~/lib/workspaceTabs'
import { notifyWarn } from '~/lib/notify'
import type { SavedQuery } from '~/lib/live'
import { NEEDS_APPROVAL, useStatementApproval, type CheckStatement } from '~/components/StatementApproval'
import { ApprovalBadge, EnvBadge } from '~/components/ChangeImpact'
import { ChangesWaitingBanner } from '~/components/ChangesWaiting'
import { writesNeedApproval, type CheckLike } from '~/lib/changeApproval'
import { ImportData, type ImportRun } from '~/components/ImportData'
import { ChangesPanel } from '~/components/DatabaseChanges'
import type { EditTarget } from '~/lib/rowEdit'
import { GenerateContext, NewSchemaButton, NewSchemaDialog, NewTableButton, ObjectGenMenu, TableDesigner, type GenAction, type GenContext } from '~/components/SqlGenerate'
import { DESIGN_SQL, blankColumn, ddlQuery, parseDesign, qualify, tableTemplate, type Design } from '~/lib/sqlGen'
import { TARGET_BY_NAME_SQL, parseTarget } from '~/lib/rowEdit'

// The query builder loads when it is first opened.
const QueryBuilder = lazy(() => import('~/components/QueryBuilder').then((m) => ({ default: m.QueryBuilder })))

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
  /** A panel to open on, from a link: changes, trash or backups. */
  initialPanel?: string
}

type Col = { name: string; type: string; table?: number; attnum?: number }
type Cell = string | null
type Obj = { name: string; kind: string; detail?: string; count?: number; schema?: string }

interface PGAnswer {
  /** The next piece of a value a row batch cut, for a run sent with whole. */
  part?: { row: number; col: number; text: string; last?: boolean }
  id: number
  type: 'ready' | 'cols' | 'done' | 'error' | 'schema' | 'closed' | 'admin' | 'check' | 'part'
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
  /** Why the agent stopped a statement before it ran. */
  guard?: PGGuard
  /** What a checked write would do. */
  check?: PGCheck
  /** What a DELETE, DROP or TRUNCATE put in the trash instead of losing. */
  trash?: TrashItem[]
}

type Panel = 'query' | 'activity' | 'roles' | 'extensions' | 'replication' | 'backups' | 'trash' | 'changes'
type ManagePanel = Exclude<Panel, 'query'>
type WorkspaceTab = 'query' | 'diagram' | 'manage'

interface Result {
  id: number
  sql: string
  cols: Col[]
  rows: Cell[][]
  /** Cells the agent cut at its size limit, as `row:col`: shown, not editable. */
  cut?: Set<string>
  done?: PGAnswer
  error?: PGAnswer
  startedAt: number
}

/**
 * A script: several statements run one after another on the session, each
 * its own request, so each is recorded, guarded and checked like any other.
 * `parts` holds a result per statement that has started.
 */
interface Script {
  parts: Result[]
  stmts: string[]
  state: 'running' | 'done' | 'stopped'
  /** 1-based statement being checked or run. */
  at: number
  note?: string
}

/** What the person decided in a dialog that paused a script. */
type Gate = { go: boolean; confirm?: string }

/**
 * Whether a checked write in a script touches only this session's temp
 * tables (a script's own scratch tables), so it need not stop to ask. A
 * count with no table behind it is asked about: statistics may be off.
 */
function onlyTempTouched(c: PGCheck): boolean {
  if (c.guard) return false
  const ts = c.tables ?? []
  if (ts.length === 0) return c.rows === 0
  return ts.every((t) => /^pg_temp_\d+\./.test(t.name))
}

/** The request id a row batch belongs to: its first four bytes. */
function batchId(buf: ArrayBuffer): number {
  const b = new Uint8Array(buf)
  return ((b[0]! << 24) | (b[1]! << 16) | (b[2]! << 8) | b[3]!) >>> 0
}

/** Decodes one row batch: id, then cells as uvarint(len*2+cut+1) + bytes; 0 is NULL. */
function decodeBatch(buf: ArrayBuffer, ncols: number, dec: TextDecoder): { id: number; rows: Cell[][]; cut: [number, number][] } {
  const b = new Uint8Array(buf)
  const id = batchId(buf)
  let p = 4
  const rows: Cell[][] = []
  const cuts: [number, number][] = []
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
      if (cut) cuts.push([rows.length, i])
      row[i] = dec.decode(b.subarray(p, p + len)) + (cut ? ' …' : '')
      p += len
    }
    rows.push(row)
  }
  return { id, rows, cut: cuts }
}

const KIND_ICON: Record<string, typeof IconTable> = {
  table: IconTable, view: IconEye, matview: IconEye, function: IconFunction, procedure: IconFunction,
  sequence: IconHash,
}

type Job =
  // text is what the person wrote; bound is what is sent (its :names as $n,
  // with their values); at is where text begins in the editor.
  | { tab: string; kind: 'run'; text: string; maxRows: number; confirm?: string; bound: Bound; at: number }
  // Each statement with where it starts in the editor, for error marks.
  | { tab: string; kind: 'script'; stmts: { sql: string; at: number }[]; maxRows: number }
  | { tab: string; kind: 'explain'; text: string; mode: PlanRun['mode']; bound: Bound }

const DEFAULT_H = { wide: 170, narrow: 110 }

/*
MySQL and MariaDB open this workspace too: the session speaks the same frames.
What is Postgres's alone -- explain plans, the diagram, Manage, generating
SQL from the tree, the query builder, dry runs -- reads Postgres's catalogue
or writes Postgres's dialect, so it is left out for MySQL rather than shown
working wrongly. The tree quotes names with backticks there.
*/
const MySQLDialect = createContext(false)

export function DatabaseWorkspace({ database, access, getTicket, endpoint, onAskForAccess, orgAdmin, initialPanel }: WorkspaceProps) {
  const mysql = database.engine === 'mysql'
  const engineName = mysql ? 'MySQL' : 'Postgres'
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
  const [panel, setPanel] = useState<Panel>(!mysql && (initialPanel === 'changes' || initialPanel === 'trash' || initialPanel === 'backups') ? initialPanel : 'query')
  const [view, setView] = useState<'query' | 'diagram'>('query')
  // The three tabs under the header. Manage is any administration panel;
  // it reopens the one last looked at.
  const lastManage = useRef<ManagePanel>(panel === 'query' ? 'activity' : panel)
  if (panel !== 'query') lastManage.current = panel
  const tabNow: WorkspaceTab = view === 'diagram' ? 'diagram' : panel === 'query' ? 'query' : 'manage'
  const goTab = (t: WorkspaceTab) => {
    setView(t === 'diagram' ? 'diagram' : 'query')
    setPanel(t === 'manage' ? lastManage.current : 'query')
  }

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
  const [writeCheck, setWriteCheck] = useState<WriteCheckState | null>(null)
  // A script per tab, while it runs and after; a single statement clears it.
  const [scripts, setScripts] = useState<Record<string, Script>>({})
  // A dialog that paused a script resolves this with the person's answer.
  const gate = useRef<((g: Gate) => void) | null>(null)
  const [gateStep, setGateStep] = useState<{ n: number; total: number } | undefined>(undefined)
  // Tabs whose script the person cancelled: it stops before the next statement.
  const scriptStop = useRef(new Set<string>())
  const tabsRef = useRef(tabs)
  tabsRef.current = tabs
  // Where Postgres said each tab's last statement failed, until it is edited.
  const [marks, setMarks] = useState<Record<string, EditorError | null>>({})
  const result = results[tab.key] ?? null
  const plan = plans[tab.key]
  const resultView = shown[tab.key] ?? 'results'
  const script = scripts[tab.key]
  // Answers the dialog a script is paused on; false if none is.
  const answerGate = (g: Gate) => {
    const f = gate.current
    if (!f) return false
    gate.current = null
    setGateStep(undefined)
    f(g)
    return true
  }

  const patchTab = useCallback((key: string, f: (t: QueryTab) => QueryTab) => {
    setTabs((ts) => ts.map((t) => (t.key === key ? f(t) : t)))
  }, [])
  const setSql = useCallback((v: string) => {
    patchTab(activeRef.current, (t) => ({ ...t, sql: v, name: t.named ? t.name : (nameFromSQL(v) ?? t.name) }))
    setMarks((m) => (m[activeRef.current] ? { ...m, [activeRef.current]: null } : m))
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
          const { rows, cut } = decodeBatch(ev.data, e.r.cols.length, dec)
          // Append in place and publish a new object once per batch, so React
          // re-renders per batch rather than per row.
          if (cut.length) {
            e.r.cut ??= new Set()
            for (const [ri, ci] of cut) e.r.cut.add(`${e.r.rows.length + ri}:${ci}`)
          }
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
        if (a.type === 'part' && a.part) {
          // The rest of a long value, after the batch that cut it.
          const e = live.current.get(a.id)
          const row = e?.r.rows[a.part.row]
          if (!e || !row) return
          row[a.part.col] = (row[a.part.col] ?? '') + a.part.text
          if (a.part.last) e.r.cut?.delete(`${a.part.row}:${a.part.col}`)
          setResults((prev) => ({ ...prev, [e.tab]: { ...e.r } }))
          return
        }
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

  // Before an ask, each statement is run once and rolled back, with the
  // first rows it changes, so the approver sees what it does.
  const checkForApproval: CheckStatement = useCallback((sql, params) => new Promise<CheckLike | null>((resolve) => {
    const id = nextId.current++
    pending.current.set(id, (a) => {
      pending.current.delete(id)
      resolve(a.type === 'check' && a.check ? { rows: a.check.rows, tables: a.check.tables, preview: a.check.preview ?? null } : null)
    })
    if (!send({ id, op: 'check', sql, params: params?.length ? params : undefined, preview: true })) {
      pending.current.delete(id)
      resolve(null)
    }
  }), [send])
  const { data: me } = useQuery(meQuery())
  const { ask: askApproval, dialog: approvalDialog } = useStatementApproval(database.id, database.dbname,
    { label: database.label, me: me?.email, check: mysql ? undefined : checkForApproval })

  // collect runs one statement for a panel and resolves with everything it
  // returned. Recorded like any other statement; it is one. onId hears the
  // request id, so a plan in progress can be cancelled.
  const collectWithId = useCallback((text: string, max = 1000, onId?: (id: number) => void, params?: (string | null)[], approval?: string) => new Promise<Collected>((resolve) => {
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
      else { c.tag = a.tag; if (a.trash?.length) c.trashed = true }
      resolve(c)
    })
    if (!send({ id, op: 'run', sql: text, maxRows: max, params: params?.length ? params : undefined, approval, whole: true })) {
      pending.current.delete(id)
      collectors.current.delete(id)
      resolve({ cols: [], rows: [], error: { error: 'the session ended' } })
    }
  }), [send])
  // A panel's statement that changes a production database (a restore from
  // the trash, a grant) waits for approval and is sent again with it.
  const collect: Collect = useCallback(async (text, max, params) => {
    const r = await collectWithId(text, max, undefined, params)
    if (r.error?.code !== NEEDS_APPROVAL) return r
    const id = await askApproval([{ sql: text, params }], { why: r.error.error })
    return id ? collectWithId(text, max, undefined, params, id) : r
  }, [collectWithId, askApproval])

  // openGuard shows why the agent stopped a statement, and for a DROP or
  // TRUNCATE what it would take with it, read as the person's role.
  const openGuard = useCallback((stmt: string, key: string, guard: PGGuard) => {
    setWriteCheck({ kind: 'guard', sql: stmt, tab: key, guard, facts: guard.code === GUARD_DROP_DB || guard.verb === 'update' || guard.verb === 'delete' ? null : undefined })
    if (guard.code === GUARD_DROP_DB || guard.verb === 'update' || guard.verb === 'delete' || mysql) return
    const q = dropFactsSQL(guard)
    void (async () => {
      const facts: NonNullable<Extract<WriteCheckState, { kind: 'guard' }>['facts']> = { dependents: [] }
      if (q.size) {
        const r = await collect(q.size, 1)
        if (r.error) facts.error = r.error.error
        else if (r.rows[0]) { facts.rows = Number(r.rows[0][0] ?? 0); facts.bytes = Number(r.rows[0][1] ?? 0) }
      }
      if (q.tables) {
        const r = await collect(q.tables, 1)
        if (!r.error && r.rows[0]) facts.tables = Number(r.rows[0][0] ?? 0)
      }
      if (q.deps) {
        const r = await collect(q.deps, 50)
        if (!r.error) facts.dependents = r.rows.map((x) => x[0] ?? '').filter(Boolean)
      }
      setWriteCheck((w) => (w?.kind === 'guard' && w.sql === stmt ? { ...w, facts } : w))
    })()
  }, [collect])

  const startRef = useRef<(j: Job) => void>(() => {})
  const finished = useCallback(() => {
    runningRef.current = null
    setRunning(null)
    const next = waiting.current.shift()
    setWaitingTabs(waiting.current.map((j) => j.tab))
    if (next) startRef.current(next)
  }, [])

  const bindFor = useCallback((text: string, key: string) =>
    bindParams(text, tabsRef.current.find((x) => x.key === key)?.params ?? {}), [])
  // check asks the agent what a write would do: it runs it as the person's
  // role and rolls it back.
  const check = useCallback((text: string, key: string) => new Promise<PGAnswer>((resolve) => {
    const id = nextId.current++
    pending.current.set(id, (a) => {
      pending.current.delete(id)
      resolve(a)
    })
    const b = bindFor(text, key)
    if (!send({ id, op: 'check', sql: b.sql, params: b.params.length ? b.params : undefined })) {
      pending.current.delete(id)
      resolve({ id, type: 'closed', error: 'the session ended' })
    }
  }), [send, bindFor])

  // runPart runs one statement into the tab's result and resolves with its
  // final answer. It does not end the job: the caller does, after one
  // statement or after a script's last. `at` is where the statement starts
  // in the editor, so an error is marked where Postgres says.
  const runPart = useCallback((key: string, stmt: string, maxRows: number, at: number, confirm?: string, onStart?: (r: Result) => void, approval?: string) =>
    new Promise<{ r: Result; a: PGAnswer }>((resolve) => {
      const id = nextId.current++
      const b = bindFor(stmt, key)
      const r: Result = { id, sql: stmt, cols: [], rows: [], startedAt: Date.now() }
      live.current.set(id, { tab: key, r })
      runningRef.current = { id, tab: key }
      setRunning(runningRef.current)
      setResults((prev) => ({ ...prev, [key]: r }))
      setShown((v) => ({ ...v, [key]: 'results' }))
      onStart?.(r)
      pending.current.set(id, (a) => {
        if (a.type === 'cols') {
          r.cols = a.cols ?? []
          setResults((prev) => ({ ...prev, [key]: { ...r } }))
          return
        }
        pending.current.delete(id)
        live.current.delete(id)
        if (a.type === 'done') r.done = a
        else r.error = a
        setResults((prev) => ({ ...prev, [key]: { ...r } }))
        if (a.type === 'error' && a.position) {
          const from = pgErrorOffset(b.sql, a.position, at, (o) => originalOffset(b, o))
          setMarks((m) => ({ ...m, [key]: { from, message: a.error ?? `${engineName} refused this` } }))
        }
        txOpen.current = txAfter(txOpen.current, stmt, a.type === 'done')
        setHistory((h) => [{ sql: stmt, at: Date.now(), ok: a.type === 'done' }, ...h].slice(0, 100))
        resolve({ r, a })
      })
      if (!send({ id, op: 'run', sql: b.sql, maxRows, confirm, approval, params: b.params.length ? b.params : undefined, whole: true })) {
        pending.current.delete(id)
        live.current.delete(id)
        const a: PGAnswer = { id, type: 'closed', error: 'the session ended' }
        r.error = a
        resolve({ r, a })
      }
    }), [send, bindFor])

  const startRun = useCallback((job: Extract<Job, { kind: 'run' }>) => {
    setScripts(({ [job.tab]: _s, ...rest }) => rest)
    void (async () => {
      let { a } = await runPart(job.tab, job.text, job.maxRows, job.at, job.confirm)
      if (a.type === 'error' && a.code === NEEDS_APPROVAL) {
        const b = bindFor(job.text, job.tab)
        const id = await askApproval([{ sql: b.sql, params: b.params }], { why: a.error })
        if (id) ({ a } = await runPart(job.tab, job.text, job.maxRows, job.at, job.confirm, undefined, id))
      }
      if (a.type === 'error' && a.guard) openGuard(job.text, job.tab, a.guard)
      finished()
    })()
  }, [runPart, finished, openGuard, bindFor, askApproval])

  // ask pauses a script on a dialog until the person answers it.
  const ask = useCallback((n: number, total: number, show: () => void) => new Promise<Gate>((resolve) => {
    gate.current = resolve
    setGateStep({ n, total })
    show()
  }), [])

  // startScript runs a script's statements in order on this session and
  // stops at the first that fails, is refused, or that the person declines,
  // so nothing after a failure runs (a COMMIT least of all). A write is
  // checked first, as a single one would be; one that only fills the
  // script's own temp tables goes on without asking.
  const startScript = useCallback((job: Extract<Job, { kind: 'script' }>) => {
    const key = job.tab
    const total = job.stmts.length
    const sc: Script = { parts: [], stmts: job.stmts.map((x) => x.sql), state: 'running', at: 1 }
    const publish = () => setScripts((p) => ({ ...p, [key]: { ...sc, parts: [...sc.parts] } }))
    scriptStop.current.delete(key)
    runningRef.current = { id: -1, tab: key }
    setRunning(runningRef.current)
    publish()
    void (async () => {
      const open = () => (txOpen.current
        ? ' Your transaction is still open; nothing in it is kept until you COMMIT, and ROLLBACK ends it.'
        : '')
      let ran = 0
      // On production the first statement that needs approval asks for it
      // and the rest of the script at once, so the approver sees the whole
      // of what is left and the script waits once.
      let approval: string | undefined
      for (let i = 0; i < total; i++) {
        const n = i + 1
        const { sql: stmt, at } = job.stmts[i]!
        sc.at = n
        publish()
        if (scriptStop.current.has(key)) {
          sc.note = `Cancelled before statement ${n} of ${total}; it and the rest did not run.${open()}`
          break
        }
        let confirm: string | undefined
        if (needsCheck(stmt)) {
          const c = await check(stmt, key)
          if (c.type === 'check' && c.check) {
            if (!onlyTempTouched(c.check)) {
              const g = await ask(n, total, () => setWriteCheck({ kind: 'checked', sql: stmt, tab: key, check: c.check! }))
              if (!g.go) {
                sc.note = `Stopped before statement ${n} of ${total}, as you chose; it and the rest did not run.${open()}`
                break
              }
              confirm = g.confirm
            }
          } else if (!(c.type === 'error' && /unknown operation/.test(c.error ?? ''))) {
            // An agent from before checks answers "unknown operation" and
            // runs as it always did, guard and all. Anything else failed.
            if (c.type === 'error' && c.position) {
              const b = bindFor(stmt, key)
              setMarks((m) => ({ ...m, [key]: { from: pgErrorOffset(b.sql, c.position!, at, (o) => originalOffset(b, o)), message: c.error ?? 'Postgres refused this' } }))
            }
            await ask(n, total, () => setWriteCheck({ kind: 'failed', sql: stmt, tab: key, error: c.error ?? 'the check failed', code: c.code }))
            sc.note = `Stopped at statement ${n} of ${total}: Postgres refused it while checking; it and the rest did not run.${open()}`
            break
          }
        }
        if (scriptStop.current.has(key)) {
          sc.note = `Cancelled before statement ${n} of ${total}; it and the rest did not run.${open()}`
          break
        }
        const onStart = (r: Result) => { sc.parts[i] = r; publish() }
        let res = await runPart(key, stmt, job.maxRows, at, confirm, onStart, approval)
        if (res.a.type === 'error' && res.a.guard) {
          const guard = res.a.guard
          const g = await ask(n, total, () => openGuard(stmt, key, guard))
          if (g.go) { confirm = g.confirm; res = await runPart(key, stmt, job.maxRows, at, g.confirm, onStart, approval) }
          else {
            publish()
            sc.note = `Argus stopped statement ${n} of ${total} before it ran; it and the rest did not run.${open()}`
            break
          }
        }
        if (res.a.type === 'error' && res.a.code === NEEDS_APPROVAL && !approval) {
          const items = job.stmts.slice(i).map((x) => { const b = bindFor(x.sql, key); return { sql: b.sql, params: b.params } })
          const id = await askApproval(items, { why: res.a.error })
          if (id) {
            approval = id
            res = await runPart(key, stmt, job.maxRows, at, confirm, onStart, approval)
          }
        }
        publish()
        if (res.a.type !== 'done') {
          const left = total - n
          const rest = left ? ` The ${left === 1 ? 'statement' : `${left} statements`} after it did not run.` : ''
          const why = scriptStop.current.has(key) ? 'cancelled' : res.a.type === 'closed' ? 'the session ended' : `${engineName} refused it`
          sc.note = `Stopped at statement ${n} of ${total}: ${why}.${rest}${open()}`
          break
        }
        ran++
      }
      sc.state = ran === total ? 'done' : 'stopped'
      if (ran === total) sc.note = `All ${total} statements ran.${open()}`
      // Show the failure if there was one, otherwise the last answer with
      // columns: what a script usually ends by showing.
      const last = sc.state === 'stopped' ? sc.parts[sc.parts.length - 1] : [...sc.parts].reverse().find((p) => p.cols.length > 0) ?? sc.parts[sc.parts.length - 1]
      if (last) setResults((prev) => ({ ...prev, [key]: { ...last } }))
      publish()
      gate.current = null
      setGateStep(undefined)
      scriptStop.current.delete(key)
      finished()
    })()
  }, [runPart, check, ask, openGuard, finished, bindFor, askApproval])

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
      const stmt = explainSQL(job.bound.sql, job.mode)
      const r = await collectWithId(stmt, 20000, track, job.bound.params)
      if (wrap) await collectWithId('ROLLBACK', 1, track)
      setHistory((h) => [{ sql: stmt, at: Date.now(), ok: !r.error }, ...h].slice(0, 100))
      if (r.error) {
        done({ plan: null, error: `Postgres refused this: ${r.error.error ?? 'unknown error'}${r.error.code ? ` (${r.error.code})` : ''}` })
        return
      }
      done({ plan: parsePlan(r.rows.map((x) => x[0] ?? '')), rolledBack: wrap })
    })()
  }, [collectWithId, finished])

  // saveEdits sends the grid's row edits as one transaction: each statement
  // must touch exactly one row (an UPDATE keyed on the primary key, which
  // matches nothing once someone else changed the row), or all of it is
  // rolled back. Inside the person's own transaction it uses a savepoint, so
  // a failed save leaves their transaction as it was. The session runs one
  // statement at a time, so this holds it the way a run does.
  const saveEdits = useCallback(async (stmts: Stmt[]): Promise<SaveResult> => {
    if (runningRef.current) return { ok: false, at: 0, error: 'another statement is running; wait for it to finish.' }
    const key = activeRef.current
    runningRef.current = { id: -1, tab: key }
    setRunning(runningRef.current)
    const track = (id: number) => {
      runningRef.current = { id, tab: key }
      setRunning(runningRef.current)
    }
    const inTx = txOpen.current
    const mark = 'argus_row_edit'
    const note = (sql: string, ok: boolean) => setHistory((h) => [{ sql, at: Date.now(), ok }, ...h].slice(0, 100))
    // On production the first statement comes back asking for approval;
    // then the whole save is asked for at once and sent again under it.
    const attempt = async (approval?: string): Promise<SaveResult & { approve?: string }> => {
      const b = await collectWithId(inTx ? `SAVEPOINT ${mark}` : 'BEGIN', 1, track)
      if (b.error) return { ok: false, at: 0, error: b.error.error ?? 'could not start a transaction' }
      const returned = []
      for (let i = 0; i < stmts.length; i++) {
        const s = stmts[i]!
        const r = await collectWithId(s.sql, 2, track, s.params, approval)
        // A DELETE has no RETURNING, so the trash can keep the row: its tag
        // says how many rows it removed.
        const one = !r.error && (s.deletes ? r.tag === 'DELETE 1' : r.rows.length === 1)
        note(s.sql, one)
        if (!one) {
          await collectWithId(inTx ? `ROLLBACK TO SAVEPOINT ${mark}` : 'ROLLBACK', 1, track)
          if (inTx) await collectWithId(`RELEASE SAVEPOINT ${mark}`, 1, track)
          const error = r.error
            ? `${r.error.error ?? 'Postgres refused it'}${r.error.code ? ` (${r.error.code})` : ''}`
            : r.rows.length === 0 || s.deletes
              ? 'the row is gone, or someone changed it since you read it. Run the query again to see it as it is now.'
              : 'it matched more than one row.'
          return { ok: false, at: i, error, approve: r.error?.code === NEEDS_APPROVAL && !approval && i === 0 ? r.error.error ?? '' : undefined }
        }
        returned.push(r)
      }
      const c = await collectWithId(inTx ? `RELEASE SAVEPOINT ${mark}` : 'COMMIT', 1, track)
      if (c.error) return { ok: false, at: stmts.length - 1, error: c.error.error ?? 'the commit failed' }
      return { ok: true, returned }
    }
    try {
      const first = await attempt()
      if (first.ok || first.approve === undefined) return first
      const id = await askApproval(stmts.map((s) => ({ sql: s.sql, params: s.params })),
        { why: first.approve, summary: { rows: stmts.length } })
      if (!id) return { ok: false, at: 0, error: 'it needs approval on production, and none was given. Nothing was saved.' }
      return await attempt(id)
    } finally {
      finished()
    }
  }, [collectWithId, finished, askApproval])

  // runImport sends an import's INSERT statements in one transaction (a
  // savepoint inside the person's own), and rolls all of it back at the first
  // that fails. On production the whole import is asked for at once.
  const runImport: ImportRun = useCallback(async (stmts, progress) => {
    if (runningRef.current) return { ok: false, at: 0, error: 'another statement is running; wait for it to finish.' }
    const key = activeRef.current
    runningRef.current = { id: -1, tab: key }
    setRunning(runningRef.current)
    const track = (id: number) => { runningRef.current = { id, tab: key }; setRunning(runningRef.current) }
    const inTx = txOpen.current
    const mark = 'argus_import'
    const undo = async () => {
      await collectWithId(inTx ? `ROLLBACK TO SAVEPOINT ${mark}` : 'ROLLBACK', 1, track)
      if (inTx) await collectWithId(`RELEASE SAVEPOINT ${mark}`, 1, track)
    }
    const attempt = async (approval?: string): Promise<{ ok: true; rows: number } | { ok: false; at: number; error: string; approve?: string }> => {
      const b = await collectWithId(inTx ? `SAVEPOINT ${mark}` : 'BEGIN', 1, track)
      if (b.error) return { ok: false, at: 0, error: b.error.error ?? 'could not start a transaction' }
      let rows = 0
      for (let i = 0; i < stmts.length; i++) {
        const s = stmts[i]!
        const r = await collectWithId(s.sql, 1, track, s.params, approval)
        setHistory((h) => [{ sql: s.sql.length > 2000 ? `${s.sql.slice(0, 2000)}…` : s.sql, at: Date.now(), ok: !r.error }, ...h].slice(0, 100))
        if (r.error) {
          await undo()
          return { ok: false, at: i, error: `${r.error.error ?? 'Postgres refused it'}${r.error.code ? ` (${r.error.code})` : ''}`,
            approve: r.error.code === NEEDS_APPROVAL && !approval && i === 0 ? r.error.error ?? '' : undefined }
        }
        rows += Number(/^INSERT \d+ (\d+)/.exec(r.tag ?? '')?.[1] ?? 0)
        progress(s.last - 1)
      }
      const c = await collectWithId(inTx ? `RELEASE SAVEPOINT ${mark}` : 'COMMIT', 1, track)
      if (c.error) return { ok: false, at: stmts.length - 1, error: c.error.error ?? 'the commit failed' }
      return { ok: true, rows }
    }
    try {
      const first = await attempt()
      if (first.ok || first.approve === undefined) return first
      const id = await askApproval(stmts.map((s) => ({ sql: s.sql, params: s.params })),
        { why: first.approve, summary: { import: true, rows: stmts.reduce((n, s) => n + (s.last - s.first + 1), 0) } })
      if (!id) return { ok: false, at: 0, error: 'it needs approval on production, and none was given' }
      return await attempt(id)
    } finally {
      finished()
    }
  }, [collectWithId, finished, askApproval])

  startRef.current = (j: Job) => (j.kind === 'run' ? startRun(j) : j.kind === 'script' ? startScript(j) : startExplain(j))
  const enqueue = useCallback((j: Job) => {
    if (runningRef.current) {
      // One waiting job per tab: pressing Run again replaces it.
      waiting.current = [...waiting.current.filter((w) => w.tab !== j.tab), j]
      setWaitingTabs(waiting.current.map((w) => w.tab))
      return
    }
    startRef.current(j)
  }, [])

  const run = useCallback((text: string, key = activeRef.current, confirm?: string, at = 0) => {
    const stmt = text.trim()
    if (!stmt) return
    const t = tabs.find((x) => x.key === key)
    at += text.length - text.trimStart().length
    enqueue({ tab: key, kind: 'run', text: stmt, maxRows: t?.maxRows ?? 1000, confirm, bound: bindFor(stmt, key), at })
  }, [enqueue, tabs, bindFor])

  // The person's Run: a write that may touch many rows is counted first;
  // everything else goes straight to the agent, whose guard stops a DROP,
  // TRUNCATE or an UPDATE/DELETE without WHERE and answers why.
  const checkAt = useRef(0)
  // A script of several statements runs them in turn (startScript), each
  // checked and guarded on its own.
  const runChecked = useCallback((text: string, key = activeRef.current, at = 0) => {
    const parts = splitSQL(text)
    if (parts.length === 0) return
    if (parts.length > 1) {
      const t = tabs.find((x) => x.key === key)
      enqueue({ tab: key, kind: 'script', stmts: parts.map((x) => ({ sql: x.sql, at: at + x.from })), maxRows: t?.maxRows ?? 1000 })
      return
    }
    const stmt = parts[0]!.sql
    at += parts[0]!.from
    if (!needsCheck(stmt)) {
      run(stmt, key, undefined, at)
      return
    }
    checkAt.current = at
    setWriteCheck({ kind: 'checking', sql: stmt, tab: key })
    void check(stmt, key).then((a) => {
      if (a.type === 'check' && a.check) {
        setWriteCheck((w) => (w?.sql === stmt ? { kind: 'checked', sql: stmt, tab: key, check: a.check! } : w))
        return
      }
      if (a.type === 'error' && /unknown operation/.test(a.error ?? '')) {
        // An agent from before checks: run as it always did, guard and all.
        setWriteCheck(null)
        run(stmt, key, undefined, at)
        return
      }
      setWriteCheck((w) => (w?.sql === stmt ? { kind: 'failed', sql: stmt, tab: key, error: a.error ?? 'the check failed', code: a.code } : w))
      if (a.type === 'error' && a.position) {
        const b = bindFor(stmt, key)
        setMarks((m) => ({ ...m, [key]: { from: pgErrorOffset(b.sql, a.position!, at, (o) => originalOffset(b, o)), message: a.error ?? 'Postgres refused this' } }))
      }
    })
  }, [check, run, bindFor, enqueue, tabs])

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
    // A script stops before its next statement, as well as this one.
    if (r) scriptStop.current.add(r.tab)
    if (r && r.id > 0) send({ id: r.id, op: 'cancel' })
    // What was waiting for this tab goes too.
    waiting.current = waiting.current.filter((w) => w.tab !== activeRef.current)
    setWaitingTabs(waiting.current.map((w) => w.tab))
  }, [send])

  const editorRef = useRef<SqlEditorHandle | null>(null)

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
  const selectionAt = useCallback(() => editorRef.current?.selection() ?? { text: sql, from: 0 }, [sql])
  const selectionOrAll = useCallback(() => selectionAt().text, [selectionAt])
  const runSelectionOrAll = useCallback(() => {
    const s = selectionAt()
    runChecked(s.text, activeRef.current, s.from)
  }, [runChecked, selectionAt])
  const explain = useCallback((mode: PlanRun['mode']) => {
    const text = selectionOrAll().trim()
    if (!text) return
    if (!explainable(text)) {
      setPlans((p) => ({ ...p, [activeRef.current]: { mode, plan: null, running: false,
        error: 'EXPLAIN takes a SELECT, INSERT, UPDATE, DELETE, MERGE, VALUES or EXECUTE. Select just that statement and try again.' } }))
      setShown((v) => ({ ...v, [activeRef.current]: 'plan' }))
      return
    }
    enqueue({ tab: activeRef.current, kind: 'explain', text, mode, bound: bindFor(text, activeRef.current) })
  }, [enqueue, selectionOrAll, bindFor])

  const insert = (text: string) => { editorRef.current?.insert(text) }

  // Format lays the statement out; the formatter loads the first time.
  const format = useCallback(() => {
    const s = selectionAt()
    if (!s.text.trim()) return
    void import('sql-formatter').then(({ format: fmt }) => {
      try {
        editorRef.current?.replaceSelectionOrAll(fmt(s.text, { language: mysql ? 'mysql' : 'postgresql', keywordCase: 'preserve', paramTypes: { named: [':'], positional: true, numbered: ['$'] } }))
      } catch (e) {
        notifyWarn('Format could not read this statement', e instanceof Error ? (e.message.split('\n')[0] ?? '') : String(e))
      }
    })
  }, [selectionAt])

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
    setScripts(({ [key]: _s, ...rest }) => rest)
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

  // The tree's generators: what they write opens in a new tab, to be read and
  // run there. The catalogue is read as the person's role, like anything else.
  const [designer, setDesigner] = useState<Design | null>(null)
  const [newSchema, setNewSchema] = useState(false)
  const [importing, setImporting] = useState<EditTarget | null>(null)
  const [building, setBuilding] = useState<{ start?: { schema: string; name: string } } | null>(null)
  const genAct = useCallback(async (a: GenAction) => {
    if (narrow) tree.close()
    const show = (text: string, name: string) => {
      if (openTab({ sql: text, name })) { setPanel('query'); setView('query') }
    }
    if (a.what === 'new-schema') { setNewSchema(true); return }
    if (a.what === 'new-table') {
      setDesigner({ schema: a.schema, name: 'new_table', columns: [{ ...blankColumn(1), name: 'id', type: 'bigint', default: 'GENERATED ALWAYS AS IDENTITY', pk: true, notNull: true }] })
      return
    }
    if (a.what === 'build') { setBuilding({ start: { schema: a.schema, name: a.name } }); return }
    const q = qualify(a.schema, a.name)
    if (a.what === 'ddl') {
      const dq = ddlQuery(a.kind, a.schema, a.name)
      if (!dq) return
      const r = await collect(dq.sql, 1, dq.params)
      if (r.error || !r.rows[0]?.[0]) {
        notifyWarn('No DDL', r.error?.error ?? `Postgres has nothing to show for ${q}.`)
        return
      }
      show(r.rows[0][0], `${a.name} DDL`)
      return
    }
    if (a.what === 'design') {
      const r = await collect(DESIGN_SQL, 2000, [q])
      if (r.error) { notifyWarn('Cannot read the table', r.error.error ?? 'Postgres refused it'); return }
      setDesigner(parseDesign(a.schema, a.name, r.rows))
      return
    }
    const r = await collect(TARGET_BY_NAME_SQL, 2000, [q])
    const t = r.error ? null : parseTarget(0, r.rows)
    if (!t) { notifyWarn('Cannot read the table', r.error?.error ?? `${q} has no columns you may see.`); return }
    if (a.what === 'import') { setImporting(t); return }
    show(tableTemplate(t, a.what), `${a.what} ${a.name}`)
  }, [collect, openTab, narrow, tree])
  const ownerRole = (level === 'owner' || level === 'dba') ? ready?.levels?.owner : undefined
  const gen: GenContext = useMemo(() => ({ act: (a) => void genAct(a), canWrite: Boolean(level) && level !== 'read', ownerRole }), [genAct, level, ownerRole])

  const bar = (
    <MySQLDialect.Provider value={mysql}><GenerateContext.Provider value={mysql ? null : gen}><LeftBar
      database={database}
      schema={schema}
      onPick={(q) => { setView('query'); insert(q); if (narrow) tree.close() }}
      onRun={(q) => { setSql(q); setPanel('query'); setView('query'); run(q); if (narrow) tree.close() }}
      history={history}
      onOpenSaved={openSaved}
      orgAdmin={Boolean(orgAdmin)}
    /></GenerateContext.Provider></MySQLDialect.Provider>
  )
  const adminProps = {
    collect, admin, level, levels: ready?.levels ?? {}, self: ready?.role ?? '', narrow,
    onAskForAccess,
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
  const names = useMemo(() => paramNames(sql), [sql])
  const runLabel = runningHere ? 'Running…' : busyElsewhere ? (waitingHere ? `Waiting for ${runningName}` : `Run after ${runningName}`) : 'Run'

  const toolbar = (
    <Group gap="xs" px={narrow ? 'sm' : 'md'} py={SP.cozy} wrap={narrow ? 'nowrap' : 'wrap'} style={{ borderBottom: '1px solid var(--color-line)' }}>
      <Button size="xs" leftSection={runningHere ? <Loader size={12} color="white" /> : <IconPlayerPlay size={14} />} onClick={runSelectionOrAll}
        disabled={!canRun} style={narrow ? { flex: 1, minWidth: 0 } : undefined}
        styles={{ label: { overflow: 'hidden', textOverflow: 'ellipsis' } }}>
        {runLabel}
      </Button>
      {!mysql && explainMenu}
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
              <Menu.Item leftSection={<IconIndentIncrease size={14} />} onClick={format} disabled={!sql.trim()}>Format</Menu.Item>
              {!mysql && <Menu.Item leftSection={<IconAdjustmentsHorizontal size={14} />} onClick={() => setBuilding({})}>Build a query</Menu.Item>}
              <Menu.Item leftSection={<IconDownload size={14} />} onClick={exportCsv} disabled={!result || result.rows.length === 0}>Export CSV</Menu.Item>
              <Menu.Divider />
              <Box px="sm" py={6}><Group gap={6}><Text size={FS.micro} c="dimmed">Show up to</Text>{rowLimit}</Group></Box>
            </Menu.Dropdown>
          </Menu>
        </>
      ) : (
        <>
          <Box w={1} h={20} mx={2} style={{ background: 'var(--color-line)' }} />
          <Tooltip label={dirty ? 'Save changes to the saved query (⌘/Ctrl+S)' : 'Save the query (⌘/Ctrl+S)'} withinPortal>
            <ActionIcon size={30} variant={dirty ? 'light' : 'subtle'} color={dirty ? 'azure' : 'slate'} onClick={() => setSaving(true)} disabled={!sql.trim()}
              aria-label={dirty ? 'Save changes' : 'Save'}>
              <IconStar size={15} />
            </ActionIcon>
          </Tooltip>
          <Tooltip label="Format (Shift+Alt+F)" withinPortal>
            <ActionIcon size={30} variant="subtle" color="slate" onClick={format} disabled={!sql.trim()} aria-label="Format">
              <IconIndentIncrease size={15} />
            </ActionIcon>
          </Tooltip>
          {!mysql && (
            <Tooltip label="Build a query by picking tables and columns" withinPortal>
              <ActionIcon size={30} variant="subtle" color="slate" onClick={() => setBuilding({})} aria-label="Build">
                <IconAdjustmentsHorizontal size={15} />
              </ActionIcon>
            </Tooltip>
          )}
          <Tooltip label="Cancel the running statement" withinPortal>
            <ActionIcon size={30} variant="subtle" color="slate" onClick={cancel} disabled={!runningHere && !waitingHere} aria-label="Cancel">
              <IconPlayerStop size={15} />
            </ActionIcon>
          </Tooltip>
          <Box w={1} h={20} mx={2} style={{ background: 'var(--color-line)' }} />
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
    <Box style={{ display: 'flex', height: narrow ? 'auto' : 'calc(100dvh - var(--app-shell-header-offset, 60px))', minHeight: 480 }}>
      {narrow ? (
        <Drawer opened={treeOpen} onClose={tree.close} size="85%" padding="xs" title={database.dbname}>
          {bar}
        </Drawer>
      ) : (
        <Box w={tabNow === 'manage' ? 220 : 280} style={{ flexShrink: 0, borderRight: '1px solid var(--color-line)', overflow: 'hidden', background: 'var(--color-surface)' }}>
          <ScrollArea h="100%" type="auto">
            {tabNow === 'manage' ? <ManageNav panel={panel as ManagePanel} level={level} narrow={false} onPick={setPanel} /> : bar}
          </ScrollArea>
        </Box>
      )}

      <Box ref={area} style={{ flex: 1, minWidth: 0, display: 'flex', flexDirection: 'column' }}>
        <WorkspaceHeader database={database} narrow={Boolean(narrow)} level={level} status={status} version={ready?.version} viaPooler={ready?.via === 'pgbouncer'}
          canAsk={Boolean((access.approvable || onAskForAccess) && level === 'read')} onAskForAccess={onAskForAccess}
          settings={orgAdmin} tab={tabNow} onTab={goTab} onOpenTree={tree.open} />
        {tabNow === 'manage' && narrow && <ManageNav panel={panel as ManagePanel} level={level} narrow onPick={setPanel} />}

        {writesNeedApproval(database) && <ChangesWaitingBanner database={database} me={me?.email} />}
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
        {view === 'query' && panel === 'trash' && <TrashPanel collect={collect} level={level} narrow={narrow} />}
        {view === 'query' && panel === 'changes' && <ChangesPanel databaseId={database.id} narrow={Boolean(narrow)} />}
        {view === 'query' && panel === 'backups' && <BackupsPanel database={database} level={level} narrow={narrow} admin={Boolean(orgAdmin)} />}
        {view === 'query' && panel === 'query' && <>
        {tabStrip}
        <Box style={{ flexShrink: 0 }}>
          <SqlEditor
            ref={editorRef}
            value={sql}
            onChange={setSql}
            complete={completeHere}
            refresh={columnsVersion}
            onRun={runSelectionOrAll}
            onSave={() => { if (tabsRef.current.find((x) => x.key === activeRef.current)?.sql.trim()) setSaving(true) }}
            onFormat={format}
            error={marks[tab.key] ?? null}
            height={editorH}
            narrow={narrow}
            placeholder={narrow ? 'SELECT …' : 'SELECT … (⌘/Ctrl+Enter runs; select part of it to run only that; Ctrl+Space suggests; ⌘/Ctrl+F finds; Shift+Alt+F formats; :name adds a parameter)'}
          />
        </Box>
        <QueryParams names={names} values={tab.params ?? {}} narrow={narrow}
          onChange={(v) => patchTab(tab.key, (t) => ({ ...t, params: v }))} />
        {grip}
        {toolbar}

        <Box style={{ flex: 1, minHeight: narrow ? 320 : 0, display: 'flex', flexDirection: 'column' }}>
          {resultTabs}
          {plan && resultView !== 'results'
            ? <ExplainView run={plan} raw={resultView === 'raw'} narrow={narrow} />
            : <>
              {script && <ScriptSteps script={script} shownId={result?.id} narrow={narrow}
                onPick={(r) => setResults((prev) => ({ ...prev, [tab.key]: { ...r } }))} />}
              {result?.done?.trash && result.done.trash.length > 0 && (
                <TrashNotice key={result.id} items={result.done.trash} collect={collect} canRestore={level !== 'read'} busy={running !== null} />
              )}
              <ResultsGrid result={result} narrow={narrow} collect={collect}
                level={level} canAsk={Boolean(access.approvable || onAskForAccess)}
                onAddToQuery={(text, params) => { const k = openTab({ sql: text, params }); if (k) setView('query') }}
                edit={level && level !== 'read' ? { allowed: true, busy: running !== null, inTransaction: txOpen.current, cut: result?.cut, save: saveEdits } : undefined}
                onRows={(rows) => setResults((prev) => {
                  const r = prev[tab.key]
                  return r ? { ...prev, [tab.key]: { ...r, rows } } : prev
                })} />
            </>}
        </Box>
        </>}
      </Box>
      {approvalDialog}
      {importing && <ImportData target={importing} narrow={Boolean(narrow)} onClose={() => setImporting(null)} run={runImport} />}
      {building && (
        <Suspense fallback={null}>
          <QueryBuilder collect={collect} start={building.start} narrow={Boolean(narrow)} onClose={() => setBuilding(null)}
            onOpen={(text, name) => { setBuilding(null); if (openTab({ sql: text, name })) { setPanel('query'); setView('query') } }} />
        </Suspense>
      )}
      {designer && <TableDesigner start={designer} narrow={narrow} onClose={() => setDesigner(null)}
        onOpen={(text, name) => { setDesigner(null); if (openTab({ sql: text, name })) { setPanel('query'); setView('query') } }} />}
      {newSchema && ownerRole && <NewSchemaDialog ownerRole={ownerRole} onClose={() => setNewSchema(false)}
        onOpen={(text, name) => { if (openTab({ sql: text, name })) { setPanel('query'); setView('query') } }} />}
      <WriteCheckDialog state={writeCheck} label={database.label} step={gateStep} databaseId={database.id} canBackup={level === 'owner' || level === 'dba'}
        onClose={() => { setWriteCheck(null); answerGate({ go: false }) }}
        onBackups={() => { setWriteCheck(null); answerGate({ go: false }); setView('query'); setPanel('backups') }}
        onRun={(confirm) => {
          const w = writeCheck
          setWriteCheck(null)
          if (!answerGate({ go: true, confirm }) && w) run(w.sql, w.tab, confirm, checkAt.current)
        }} />
      {saving && (
        <SaveQueryDialog opened={saving} onClose={() => setSaving(false)} databaseId={database.id} databaseName={database.dbname}
          initial={{ id: tab.savedId, name: tab.name, sql: tab.sql, shared: false }}
          onSaved={(q) => patchTab(tab.key, (t) => ({ ...t, name: q.name, named: true, savedId: q.id, savedSql: q.sql }))} />
      )}
    </Box>
  )
}

/* ── Results ────────────────────────────────────────────────────────────── */

/**
 * A script's statements in a row: each one's place, what Postgres said, and
 * a click to show its result below. The note says where it stopped and
 * whether a transaction is still open.
 */
function ScriptSteps({ script, shownId, narrow, onPick }: {
  script: Script
  shownId?: number
  narrow: boolean
  onPick: (r: Result) => void
}) {
  const total = script.stmts.length
  return (
    <Box px={narrow ? 'sm' : 'md'} py={6} style={{ borderBottom: '1px solid var(--color-line)' }}>
      <Group gap={6} wrap="nowrap" mb={4}>
        <Text size={FS.meta} fw={600} style={{ whiteSpace: 'nowrap' }}>Script</Text>
        <Text size={FS.meta} c={script.state === 'stopped' ? 'rose' : 'dimmed'} style={{ minWidth: 0 }} lineClamp={2}>
          {script.state === 'running'
            ? `Running statement ${script.at} of ${total}…`
            : script.note}
        </Text>
      </Group>
      <ScrollArea type="auto" scrollbarSize={6} offsetScrollbars="x">
        <Group gap={4} wrap="nowrap" pb={2}>
          {script.stmts.map((stmt, i) => {
            const r = script.parts[i]
            const failed = Boolean(r?.error)
            const label = r?.done
              ? `${r.done.tag ?? 'done'}${r.cols.length ? ` · ${r.rows.length.toLocaleString()}` : ''}`
              : failed ? 'failed' : r ? 'running…' : 'not run'
            const here = r !== undefined && r.id === shownId
            return (
              <Tooltip key={i} label={stmt.length > 300 ? stmt.slice(0, 300) + '…' : stmt} multiline maw={420}
                styles={{ tooltip: { fontFamily: 'var(--font-mono)', fontSize: 11, whiteSpace: 'pre-wrap' } }} openDelay={300}>
                <UnstyledButton disabled={!r} onClick={() => r && onPick(r)} aria-label={`Statement ${i + 1}: ${label}`}
                  style={{
                    flexShrink: 0, padding: '2px 8px', borderRadius: 6, whiteSpace: 'nowrap',
                    border: `1px solid ${here ? 'var(--color-brand)' : 'var(--color-line)'}`,
                    background: failed ? 'var(--mantine-color-rose-light)' : here ? 'var(--color-raised)' : undefined,
                    opacity: r ? 1 : 0.5, cursor: r ? 'pointer' : 'default',
                  }}>
                  <Text size={FS.micro} ff="monospace" c={failed ? 'rose' : undefined}>
                    <Text span size={FS.micro} c="dimmed">{i + 1}</Text> {label}
                  </Text>
                </UnstyledButton>
              </Tooltip>
            )
          })}
        </Group>
      </ScrollArea>
    </Box>
  )
}

/* ── Left bar ───────────────────────────────────────────────────────────── */

function LeftBar({ database, schema, onPick, onRun, history, onOpenSaved, orgAdmin }: {
  database: Database
  schema: (kind: string, schemaName?: string, object?: string) => Promise<Obj[]>
  onPick: (text: string) => void
  onRun: (sql: string) => void
  history: { sql: string; at: number; ok: boolean }[]
  onOpenSaved: (q: SavedQuery) => void
  orgAdmin: boolean
}) {
  const [schemas, setSchemas] = useState<Obj[] | null>(null)
  useEffect(() => { void schema('schemas').then(setSchemas) }, [schema])
  const [tab, setTab] = useState<string | null>('objects')

  return (
    <Stack gap={SP.tight} p="xs">
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
          <Group gap={2} wrap="nowrap" justify="space-between" pr={2}>
            <Text size={FS.micro} c="dimmed" tt="uppercase" fw={600} px={4} py={4} style={{ letterSpacing: '.06em' }}>Schemas</Text>
            <NewSchemaButton />
          </Group>
          {schemas === null && <Loader size={14} ml="sm" />}
          {schemas?.length === 0 && <Text size={FS.meta} c="dimmed" px="xs">Nothing your access can see.</Text>}
          {schemas?.map((s) => (
            <SchemaNode key={s.name} s={s} schema={schema} onPick={onPick} onRun={onRun} />
          ))}
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
  const mysql = useContext(MySQLDialect)
  const [open, setOpen] = useState(s.name === 'public' || mysql)
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
      <Group gap={2} wrap="nowrap">
        <UnstyledButton onClick={() => setOpen((o) => !o)} px={4} py={3} style={{ display: 'flex', alignItems: 'center', gap: 6, flex: 1, minWidth: 0, borderRadius: 6 }}>
          {open ? <IconChevronDown size={12} /> : <IconChevronRight size={12} />}
          <Text size={FS.body}>{s.name}</Text>
          {s.count ? <Text size={FS.micro} c="dimmed" ml="auto">{s.count}</Text> : null}
        </UnstyledButton>
        <NewTableButton schema={s.name} />
      </Group>
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

/** An estimated row count, short enough for the tree: 940, 12k, 1.2M. */
export function compactCount(n: number): string {
  if (n < 1000) return String(Math.round(n))
  const [d, u] = n < 999_500 ? [1e3, 'k'] : n < 999_500_000 ? [1e6, 'M'] : [1e9, 'B']
  const v = n / d
  return `${v < 10 ? Math.round(v * 10) / 10 : Math.round(v)}${u}`
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
  const mysql = useContext(MySQLDialect)
  const q = (n: string) => (/^[a-z_][a-z0-9_]*$/.test(n) ? n : mysql ? `\`${n.replace(/`/g, '``')}\`` : `"${n.replace(/"/g, '""')}"`)
  // A MySQL session is in its database already; the name alone is enough.
  const qualified = mysql ? q(o.name) : `${q(schemaName)}.${q(o.name)}`
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
          {(o.kind === 'table' || o.kind === 'matview') && o.count && o.count > 0 ? (
            <Text size={FS.micro} c="dimmed" ff="monospace" ml="auto" style={{ whiteSpace: 'nowrap', flexShrink: 0 }}
              aria-label={`about ${o.count.toLocaleString()} rows`}>≈{compactCount(o.count)}</Text>
          ) : o.detail && o.kind !== 'function' && o.kind !== 'procedure' && (
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
        <ObjectGenMenu kind={o.kind} schema={schemaName} name={o.name} />
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

/* ── Header and Manage ─────────────────────────────────────────────────── */

/*
The workspace's one header. The name, server, engine version, label and
approval rule used to appear in a breadcrumb row, in this header and again in
a card at the top of the object tree; they are said once, here. A production
database carries a rose edge across the top, so which world the next
statement lands in is visible before it is read. Under it, three tabs: the
editor, the diagram, and Manage, which holds every administration panel that
used to hang under the object tree.
*/
/** The server's version as the header says it: "Postgres 16.4", "MySQL 8.0.46", "MariaDB 10.11.14". */
function WorkspaceHeader({ database, narrow, level, status, version, viaPooler, canAsk, onAskForAccess, settings, tab, onTab, onOpenTree }: {
  database: Database
  narrow: boolean
  level: DBLevel | ''
  status: 'connecting' | 'ready' | 'closed'
  version?: string
  viaPooler: boolean
  canAsk: boolean
  onAskForAccess?: () => void
  /** An organization admin: links to who can connect and the settings. */
  settings?: boolean
  tab: WorkspaceTab
  onTab: (t: WorkspaceTab) => void
  onOpenTree: () => void
}) {
  const prod = database.label === 'production'
  const where = `${database.hostname}${version ? ` · ${engineVersion(database.engine, version)}` : ''}${viaPooler ? ' · via PgBouncer' : ''}`
  const recorded = status === 'ready' ? 'Recorded' : status === 'connecting' ? 'Connecting' : 'Closed'
  const tabs: { key: WorkspaceTab; label: string; hint: string }[] = database.engine === 'mysql'
    ? [{ key: 'query', label: 'Query', hint: 'Write and run statements' }]
    : [
      { key: 'query', label: 'Query', hint: 'Write and run statements' },
      { key: 'diagram', label: 'Diagram', hint: 'Tables and how they link' },
      { key: 'manage', label: 'Manage', hint: 'Activity, backups, data changes, trash, roles, extensions, replication' },
    ]
  return (
    <Box data-env={database.label} style={{ borderBottom: '1px solid var(--color-line)', background: 'var(--color-surface)',
      boxShadow: prod ? 'inset 0 3px 0 var(--mantine-color-rose-6)' : undefined, paddingTop: prod ? 3 : 0 }}>
      <Group gap="sm" px={narrow ? 'sm' : 'md'} pt={narrow ? 8 : 10} wrap="nowrap">
        {narrow && (
          <ActionIcon variant="subtle" onClick={onOpenTree} aria-label="Open the object tree">
            <IconMenu2 size={18} />
          </ActionIcon>
        )}
        <EngineMark engine={database.engine} size={narrow ? 26 : 30} />
        <Box style={{ minWidth: 0, flexShrink: narrow ? 1 : 0 }}>
          <Text fw={650} size={narrow ? FS.body : 'md'} style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{database.dbname}</Text>
          {narrow && <Text size={FS.micro} c="dimmed" ff="monospace" truncate>{where}</Text>}
        </Box>
        {!narrow && <Text size={FS.micro} c="dimmed" ff="monospace" style={{ whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis', minWidth: 0 }}>{where}</Text>}
        <Box style={{ flexShrink: 0 }} ml={narrow ? 'auto' : undefined}><EnvBadge label={database.label} /></Box>
        {!narrow && writesNeedApproval(database) && <Box style={{ flexShrink: 0 }}><ApprovalBadge /></Box>}
        {!narrow && (
          <Group gap={6} ml="auto" wrap="nowrap" style={{ flexShrink: 0 }}>
            <Badge variant="light" color={level === 'read' ? 'slate' : 'azure'} radius="sm">
              You can: {level ? level.toUpperCase() : '…'}
            </Badge>
            {canAsk && (
              <Button size="xs" variant="default" onClick={onAskForAccess} leftSection={<IconLock size={12} />}>
                Ask for write access
              </Button>
            )}
            <Badge variant="light" color="rose" radius="sm" leftSection={<span style={{ display: 'inline-block', width: 6, height: 6, borderRadius: 3, background: 'currentColor' }} />}>
              {recorded}
            </Badge>
            {settings && (
              <Tooltip label="Who can connect, and this database's settings" withArrow>
                <Link to="/databases/$dbId/settings" params={{ dbId: database.id }} aria-label="Who can connect and settings"
                  style={{ display: 'inline-flex', padding: 6, borderRadius: 6, color: 'var(--mantine-color-dimmed)' }}>
                  <IconSettings size={16} />
                </Link>
              </Tooltip>
            )}
          </Group>
        )}
      </Group>
      <Group gap={narrow ? 0 : 2} px={narrow ? 4 : 'xs'} mt={narrow ? 4 : 6} wrap="nowrap" role="tablist" aria-label="View">
        {tabs.map((t) => {
          const on = t.key === tab
          return (
            <UnstyledButton key={t.key} role="tab" aria-selected={on} onClick={() => onTab(t.key)} title={t.hint}
              px={narrow ? 10 : 12} py={8}
              style={{ borderBottom: `2px solid ${on ? 'var(--color-brand)' : 'transparent'}`, marginBottom: -1 }}>
              <Text size={FS.body} fw={on ? 600 : 400} c={on ? undefined : 'dimmed'}>{t.label}</Text>
            </UnstyledButton>
          )
        })}
        {narrow && (
          <Group gap={6} ml="auto" pr="xs" wrap="nowrap">
            <Badge variant="light" color={level === 'read' ? 'slate' : 'azure'} radius="sm">{level ? level.toUpperCase() : '…'}</Badge>
            {canAsk && (
              <Button size="compact-xs" variant="default" onClick={onAskForAccess} leftSection={<IconLock size={12} />}>Ask</Button>
            )}
            <Badge variant="light" color="rose" radius="sm">{recorded}</Badge>
          </Group>
        )}
      </Group>
    </Box>
  )
}

const MANAGE: { key: ManagePanel; label: string; icon: typeof IconTable; hint: string; dba?: boolean; group: 'care' | 'dba' }[] = [
  { key: 'activity', label: 'Activity & locks', icon: IconActivity, hint: 'What is running on this database now, and what waits on what', group: 'care' },
  { key: 'backups', label: 'Backups', icon: IconArchive, hint: 'Backups of this database, and putting one back', group: 'care' },
  { key: 'changes', label: 'Data changes', icon: IconHistory, hint: 'Who inserted, updated or deleted what, from the console', group: 'care' },
  { key: 'trash', label: 'Trash', icon: IconTrash, hint: 'Deleted rows, dropped tables and schemas, kept so they can be put back', group: 'care' },
  { key: 'roles', label: 'Roles & access', icon: IconUsersGroup, hint: 'Who holds which level; give or take a level from an application role', dba: true, group: 'dba' },
  { key: 'extensions', label: 'Extensions', icon: IconPuzzle, hint: 'Installed and available extensions; install, update, remove', dba: true, group: 'dba' },
  { key: 'replication', label: 'Replication', icon: IconArrowsExchange, hint: 'Publications and subscriptions: logical replication in and out of this database', dba: true, group: 'dba' },
]

/** The list of administration panels: a column on a wide screen, a row of chips on a phone. */
function ManageNav({ panel, level, narrow, onPick }: {
  panel: ManagePanel; level: DBLevel | ''; narrow: boolean; onPick: (p: ManagePanel) => void
}) {
  if (narrow) {
    return (
      <Group gap={6} px="sm" py={8} wrap="nowrap" role="tablist" aria-label="Manage"
        style={{ overflowX: 'auto', scrollbarWidth: 'none', borderBottom: '1px solid var(--color-line)' }}>
        {MANAGE.map((m) => (
          <UnstyledButton key={m.key} role="tab" aria-selected={m.key === panel} onClick={() => onPick(m.key)}
            px={10} py={5}
            style={{ flexShrink: 0, borderRadius: 999, border: '1px solid', fontSize: 12.5,
              borderColor: m.key === panel ? 'var(--color-brand)' : 'var(--color-line)',
              background: m.key === panel ? 'var(--mantine-color-azure-light)' : 'var(--color-surface)',
              color: m.key === panel ? 'var(--mantine-color-azure-light-color)' : undefined }}>
            {m.label}
          </UnstyledButton>
        ))}
      </Group>
    )
  }
  const section = (group: 'care' | 'dba', title: string) => (
    <Box mb="sm">
      <Text size={FS.micro} c="dimmed" tt="uppercase" fw={600} px={10} py={4} style={{ letterSpacing: '.06em' }}>{title}</Text>
      {MANAGE.filter((m) => m.group === group).map((m) => {
        const on = m.key === panel
        const Icon = m.icon
        return (
          <Tooltip key={m.key} label={m.dba && level !== 'dba' ? 'Needs DBA access' : m.hint} withArrow position="right" multiline w={240}>
            <UnstyledButton onClick={() => onPick(m.key)} aria-current={on ? 'page' : undefined} px={10} py={7}
              style={{ display: 'flex', alignItems: 'center', gap: 9, width: '100%', borderRadius: 7, marginBottom: 1,
                background: on ? 'var(--mantine-color-azure-light)' : undefined,
                color: on ? 'var(--mantine-color-azure-light-color)' : undefined,
                opacity: m.dba && level !== 'dba' ? 0.7 : 1 }}>
              <Icon size={15} />
              <Text size={FS.body} fw={on ? 600 : 400}>{m.label}</Text>
              {m.dba && <Badge size="xs" variant="light" color="slate" ml="auto">DBA</Badge>}
            </UnstyledButton>
          </Tooltip>
        )
      })}
    </Box>
  )
  return (
    <Box p="xs" pt="sm" aria-label="Manage">
      {section('care', 'Look after')}
      {section('dba', 'DBA')}
    </Box>
  )
}
