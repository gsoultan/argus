/*
Reading a Postgres plan for the workspace's Plan view.

The plan is asked for as EXPLAIN's text format, one row per line, because
each line is short: the JSON format arrives as one cell, and a big plan's
cell would be cut at the session's cell limit. The text format is stable
across the versions Argus supports (12 and later) for what is read here:
node lines with "(cost=… rows=… width=…)" and, under ANALYZE, "(actual
time=a..b rows=r loops=l)" or "(never executed)"; detail lines under them;
"Planning Time" and "Execution Time" at the end.

Findings are the usual problems, worded for the person who ran it. They are
hints from the plan alone: nothing here looks at the schema.
*/

export interface PlanNode {
  /** What the step is: Seq Scan, Hash Join, Sort… */
  type: string
  /** "on orders o", "using orders_pkey on orders o" -- what it reads. */
  target: string
  /** The relation a scan reads, when it reads one. */
  relation?: string
  depth: number
  estRows: number
  cost: number
  /** Under ANALYZE: rows per loop, loops, and total time per loop in ms. */
  rows?: number
  loops?: number
  totalMs?: number
  neverExecuted?: boolean
  /** Time in this step alone, children taken out, all loops. */
  selfMs?: number
  details: string[]
  children: PlanNode[]
}

export interface Finding {
  severity: 'high' | 'medium'
  title: string
  detail: string
  /** A statement the person may want to copy; never run for them. */
  fix?: string
  node: PlanNode
}

export interface Plan {
  root: PlanNode | null
  /** Every node, depth first, as the plan lists them. */
  nodes: PlanNode[]
  planningMs?: number
  executionMs?: number
  analyzed: boolean
  findings: Finding[]
  text: string
}

const NODE = /^(\s*)(?:->\s+)?(.+?)\s+\(cost=([\d.]+)\.\.([\d.]+) rows=(\d+) width=\d+\)(?:\s+\((?:actual time=([\d.]+)\.\.([\d.]+) rows=(\d+) loops=(\d+)|(never executed))\))?\s*$/

/** The leading words that name a step; the rest is what it reads. */
function splitLabel(label: string): { type: string; target: string; relation?: string } {
  const m = /^(.*?)\s+((?:using\s+\S+\s+)?on\s+(\S+)(?:\s+\S+)?)$/.exec(label)
  if (m) return { type: m[1]!, target: m[2]!, relation: m[3]! }
  return { type: label, target: '' }
}

/** Parses EXPLAIN's text output, one line per element. */
export function parsePlan(lines: string[]): Plan {
  const nodes: PlanNode[] = []
  const stack: { indent: number; node: PlanNode }[] = []
  let root: PlanNode | null = null
  let planningMs: number | undefined
  let executionMs: number | undefined
  let analyzed = false
  // Lines after "Planning:" or "JIT:" belong to the statement, not a node.
  let footer = false
  for (const raw of lines) {
    const line = raw.replace(/\s+$/, '')
    if (!line) continue
    let m = /^\s*Planning Time: ([\d.]+) ms/.exec(line)
    if (m) { planningMs = Number(m[1]); continue }
    m = /^\s*Execution Time: ([\d.]+) ms/.exec(line)
    if (m) { executionMs = Number(m[1]); analyzed = true; continue }
    if (/^(Planning|JIT|Triggers?|Settings):?/.test(line)) { footer = true; continue }
    const n = NODE.exec(line)
    if (n) {
      footer = false
      const indent = n[1]!.length
      const { type, target, relation } = splitLabel(n[2]!.trim())
      const node: PlanNode = {
        type, target, relation, depth: 0,
        cost: Number(n[4]), estRows: Number(n[5]), details: [], children: [],
      }
      if (n[6] !== undefined) {
        analyzed = true
        node.totalMs = Number(n[7])
        node.rows = Number(n[8])
        node.loops = Number(n[9])
      } else if (n[10]) {
        analyzed = true
        node.neverExecuted = true
        node.rows = 0
        node.loops = 0
        node.totalMs = 0
      }
      while (stack.length && stack[stack.length - 1]!.indent >= indent) stack.pop()
      const parent = stack[stack.length - 1]?.node
      if (parent) {
        node.depth = parent.depth + 1
        parent.children.push(node)
      } else if (!root) {
        root = node
      } else {
        // A second top-level node: an InitPlan or CTE printed flush left.
        node.depth = 1
        root.children.push(node)
      }
      stack.push({ indent, node })
      nodes.push(node)
      continue
    }
    const owner = stack[stack.length - 1]?.node
    if (owner && !footer) owner.details.push(line.trim())
  }
  for (const node of nodes) {
    if (node.totalMs === undefined) continue
    const all = node.totalMs * (node.loops || 1)
    const kids = node.children.reduce((s, c) => s + (c.totalMs ?? 0) * (c.loops || 1), 0)
    node.selfMs = Math.max(0, all - kids)
  }
  if (executionMs === undefined && root?.totalMs !== undefined) executionMs = root.totalMs
  const plan: Plan = { root, nodes, planningMs, executionMs, analyzed, findings: [], text: lines.join('\n') }
  plan.findings = findingsOf(plan)
  return plan
}

function detail(node: PlanNode, prefix: string): string | undefined {
  return node.details.find((d) => d.startsWith(prefix))?.slice(prefix.length).trim()
}

function n(x: number): string {
  if (x >= 1e6) return `${(x / 1e6).toFixed(x >= 1e7 ? 0 : 1)}M`
  if (x >= 1e4) return `${Math.round(x / 1e3)}k`
  return x.toLocaleString()
}

/** The problems worth a line above the tree, worst first. */
export function findingsOf(plan: Plan): Finding[] {
  const out: Finding[] = []
  const total = plan.executionMs ?? plan.root?.totalMs ?? 0
  const share = (node: PlanNode) => (total > 0 && node.selfMs !== undefined ? node.selfMs / total : 0)
  // Deepest first, so a misestimate is reported where it starts, not again on
  // every join above it.
  for (const node of [...plan.nodes].reverse()) {
    const actual = node.rows !== undefined ? node.rows * (node.loops || 1) : undefined
    const removed = Number(detail(node, 'Rows Removed by Filter:') ?? 0) * (node.loops || 1)

    // A sequential scan that reads far more than it keeps, or most of the time.
    if (/^(Parallel )?Seq Scan$/.test(node.type) && node.relation) {
      const read = actual !== undefined ? actual + removed : node.estRows
      const s = share(node)
      if (read >= 10000 && (s >= 0.2 || (actual !== undefined && removed > actual * 10))) {
        const filter = detail(node, 'Filter:')
        out.push({
          severity: s >= 0.5 ? 'high' : 'medium',
          title: `Seq Scan on ${node.relation} reads ${n(read)} rows${actual !== undefined ? ` to keep ${n(actual)}` : ''}${s ? ` · ${Math.round(s * 100)}% of the time` : ''}`,
          detail: filter
            ? `Every row is checked against ${filter}. An index on the columns it compares would let Postgres go straight to the matches.`
            : 'It reads the whole table. If the query joins or filters on a column of it, an index on that column may avoid this.',
          node,
        })
      }
    }

    // A row estimate far from what happened: the planner chose on bad numbers.
    if (actual !== undefined && !node.neverExecuted && node.loops) {
      const est = node.estRows * node.loops
      const big = Math.max(est, actual)
      const ratio = big / Math.max(1, Math.min(est, actual))
      const table = node.relation ?? firstRelation(node)
      // A join over a misestimated scan inherits the error: one card, on the scan.
      const said = out.some((f) => f.fix !== undefined && f.fix === `ANALYZE ${table};`)
      if (big >= 100 && ratio >= 10 && !said) {
        out.push({
          severity: ratio >= 100 ? 'high' : 'medium',
          title: `Row estimate ${Math.round(ratio)}× too ${actual > est ? 'low' : 'high'} on ${node.type}${node.relation ? ` on ${node.relation}` : ''}`,
          detail: `Postgres expected ${n(est)} rows and got ${n(actual)}, so it may have picked the wrong plan. ${table ? `Its statistics on ${table} may be stale.` : 'Its statistics may be stale.'}`,
          fix: table ? `ANALYZE ${table};` : undefined,
          node,
        })
      }
    }

    // A sort or hash that ran out of memory and went to disk.
    const sort = detail(node, 'Sort Method:')
    if (sort && /external/.test(sort)) {
      out.push({
        severity: 'medium',
        title: `${node.type} spilled to disk`,
        detail: `${sort}. More work_mem for this session would keep it in memory: SET work_mem = '64MB'.`,
        node,
      })
    }
    const batches = /Batches: (\d+)/.exec(node.details.join(' '))
    if (node.type === 'Hash' && batches && Number(batches[1]) > 1) {
      out.push({
        severity: 'medium',
        title: `Hash spilled to disk in ${batches[1]} batches`,
        detail: 'The hash table did not fit in work_mem. More work_mem for this session would keep it in memory.',
        node,
      })
    }

    // The inner side of a nested loop run very many times.
    if ((node.loops ?? 0) >= 1000 && share(node) >= 0.2) {
      out.push({
        severity: share(node) >= 0.5 ? 'high' : 'medium',
        title: `${node.type}${node.relation ? ` on ${node.relation}` : ''} runs ${n(node.loops!)} times`,
        detail: 'It is the inner side of a nested loop. A hash or merge join, or an index the loop can use, would read it once.',
        node,
      })
    }
  }
  const rank = { high: 0, medium: 1 }
  return out.sort((a, b) => rank[a.severity] - rank[b.severity] || (b.node.selfMs ?? 0) - (a.node.selfMs ?? 0)).slice(0, 6)
}

function firstRelation(node: PlanNode): string | undefined {
  for (const c of node.children) {
    if (c.relation) return c.relation
  }
  return undefined
}

/**
 * The statement to send for a plan. ANALYZE runs the statement, so the
 * caller wraps it in a transaction it rolls back; nothing here runs it.
 */
export function explainSQL(stmt: string, mode: 'plan' | 'analyze' | 'buffers'): string {
  const body = stmt.trim().replace(/;+\s*$/, '')
  const opts = mode === 'plan' ? '' : mode === 'analyze' ? '(ANALYZE) ' : '(ANALYZE, BUFFERS) '
  return `EXPLAIN ${opts}${body}`
}

/** Whether a statement is one EXPLAIN takes. */
export function explainable(stmt: string): boolean {
  const s = stmt.trim().replace(/^(\s*--[^\n]*\n|\s*\/\*[\s\S]*?\*\/)*/g, '').trim().toLowerCase()
  return /^(select|with|insert|update|delete|merge|values|table|execute|declare|create\s+table\s+\S+\s+as|create\s+materialized\s+view)\b/.test(s)
}
