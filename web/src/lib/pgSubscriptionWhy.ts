/*
Why a Postgres subscription stopped, in words, with the one fix that fits.

The agent (subscription.why) reads the server's log as the postgres account
and returns facts: the last error this subscription's workers logged, with
its table, key and the LSN of the publisher transaction it failed in. This
file turns those facts into a sentence and a fix. It never guesses past the
facts: with no message in the log it says so and says why.
*/

export interface PGApplyError {
  at?: string
  severity: string
  code?: string
  message: string
  detail?: string
  hint?: string
  context?: string
  worker?: 'apply' | 'sync'
  table?: string
  change?: string
  xid?: string
  lsn?: string
  constraint?: string
  keyColumns?: string
  keyValues?: string
  columns?: string
  conflict?: string
}

export interface PGSubscriptionWhy {
  name: string
  enabled: boolean
  disableOnError?: string
  binary?: string
  owner: string
  slot?: string
  origin: string
  appliedTo?: string
  applyErrors?: number | null
  syncErrors?: number | null
  conflicts?: Record<string, number>
  log?: string
  logNote?: string
  error?: PGApplyError
  errors: number
  firstAt?: string
  turnedOff?: boolean
  turnedOffAt?: string
}

/** What the page can offer as the fix, most useful first. */
export type WhyAction = 'enable' | 'skip' | 'sql' | 'connection' | 'publications' | 'refresh' | 'options'

export interface Explanation {
  kind: string
  /** One line: what happened to the subscription. */
  headline: string
  /** What went wrong, in plain words, from the server's own message. */
  cause: string
  /** The one fix for this cause. */
  fix: string
  /** A statement to open in the editor for the fix; never run from here. */
  sql?: { text: string; name: string }
  actions: WhyAction[]
}

const quoteIdent = (n: string) => (/^[a-z_][a-z0-9_$]*$/.test(n) ? n : `"${n.replace(/"/g, '""')}"`)
const quoteLiteral = (s: string) => `'${s.replace(/'/g, "''")}'`

/** schema.table as Postgres prints it in a message, ready for a statement. */
export function tableSQL(t: string): string {
  const i = t.indexOf('.')
  return i > 0 ? `${quoteIdent(t.slice(0, i))}.${quoteIdent(t.slice(i + 1))}` : quoteIdent(t)
}

/** A WHERE clause for a logged key, when its columns and values line up. */
export function keyWhere(cols?: string, vals?: string): string | null {
  if (!cols || vals === undefined) return null
  const c = cols.split(',').map((s) => s.trim()).filter(Boolean)
  const v = vals.split(', ')
  if (!c.length || c.length !== v.length) return null
  return `(${c.map(quoteIdent).join(', ')}) = (${v.map((x) => (x === 'null' ? 'NULL' : quoteLiteral(x))).join(', ')})`
}

/** A log timestamp ("2026-11-28 09:56:22.434 UTC") as a local time. */
export function whenLogged(at?: string): string {
  if (!at) return ''
  const iso = at.replace(' ', 'T').replace(/ UTC$/, 'Z').replace(/ ([+-]\d\d)$/, '$1:00')
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? at : d.toLocaleString()
}

function headlineOf(w: PGSubscriptionWhy): string {
  const at = whenLogged(w.turnedOffAt || w.error?.at)
  if (w.turnedOff || (!w.enabled && w.disableOnError === 'on' && w.error)) {
    return `${w.name} turned itself off after an error${at ? ` at ${at}` : ''}. Turning it back on replays the same change, so it stops again until the cause is fixed.`
  }
  if (w.enabled && w.error) {
    return `${w.name} keeps failing on the same change and retrying${at ? ` (last at ${at})` : ''}. Nothing after it is applied.`
  }
  if (!w.enabled && w.error) return `${w.name} is off. The last error it logged is below.`
  return `${w.name} is ${w.enabled ? 'on' : 'off'}.`
}

const has = (s: string | undefined, re: RegExp) => !!s && re.test(s)

/** Turns the agent's facts into a cause and the one fix for it. */
export function explain(w: PGSubscriptionWhy): Explanation {
  const e = w.error
  const headline = headlineOf(w)
  const skipLSN = e?.lsn
  const withSkip = (a: WhyAction[]): WhyAction[] => (skipLSN ? [...a, 'skip'] : a)
  if (!e) {
    const conflicts = Object.entries(w.conflicts ?? {}).filter(([, n]) => n > 0)
    const dup = conflicts.find(([k]) => k.endsWith('_exists'))
    if (dup) {
      return {
        kind: 'duplicate', headline,
        cause: `Postgres counted ${dup[1]} change${dup[1] === 1 ? '' : 's'} from the publisher for rows that already exist here (${dup[0]}). The log with the table and key could not be read: ${w.logNote ?? 'it was not found'}.`,
        fix: 'Find the rows this server already has that the publisher is sending again, delete them here, then Enable.',
        actions: ['enable'],
      }
    }
    return {
      kind: 'unknown', headline,
      cause: w.log
        ? `The server's log (${w.log}) has no error from ${w.name} in its last part, so the reason is older than what Argus reads.`
        : `Argus could not read the reason: ${w.logNote ?? 'the server log was not found'}.`,
      fix: 'Enable it once: if it stops again, the new error is logged and shows here.',
      actions: ['enable'],
    }
  }
  const t = e.table ? tableSQL(e.table) : ''
  const msg = e.message
  const in_ = e.table ? ` in ${e.table}` : ''
  const txn = e.xid ? `publisher transaction ${e.xid}` : 'this publisher transaction'

  // A row that is already here: a unique key, or Postgres 18's *_exists.
  if (e.code === '23505' || has(msg, /duplicate key value/) || has(e.conflict, /_exists$|multiple_unique/)) {
    const where = keyWhere(e.keyColumns, e.keyValues)
    const key = e.keyColumns ? ` with key (${e.keyColumns}) = (${e.keyValues ?? '?'})` : ''
    const sql = t && where ? [
      `-- ${w.name} stopped: the publisher sent a row that is already here.`,
      '-- 1. Look at the copy on this server:',
      `SELECT * FROM ${t} WHERE ${where};`,
      `-- 2. If it is the same row the publisher has, delete it here, then Enable ${w.name}.`,
      '--    The subscription applies the publisher\'s row and carries on; nothing is lost.',
      `-- DELETE FROM ${t} WHERE ${where};`,
    ].join('\n') : undefined
    return {
      kind: 'duplicate', headline,
      cause: `The publisher ${e.change === 'UPDATE' ? 'changed a row' : 'sent a new row'}${in_}${key}, but this server already has a row with that key${e.constraint ? ` (${e.constraint})` : ''}. It usually means the row was copied twice, or someone wrote it here directly.`,
      fix: `Delete the copy here${key ? ` (key ${e.keyColumns} = ${e.keyValues})` : ''}, then Enable: the subscription applies the publisher's row and carries on, and nothing is lost.${skipLSN ? ` If this server's copy is the one to keep, skip ${txn} instead.` : ''}`,
      sql: sql ? { text: sql, name: `Fix ${w.name}` } : undefined,
      actions: withSkip(sql ? ['sql', 'enable'] : ['enable']),
    }
  }
  if (has(msg, /is missing replicated columns?/)) {
    const cols = (e.columns ?? '').split(',').map((c) => c.trim().replace(/^"|"$/g, '')).filter(Boolean)
    const sql = t && cols.length ? [
      `-- ${w.name} stopped: ${e.table} here has no ${cols.join(', ')}, which the publisher sends.`,
      '-- Add the same column(s) as on the publisher, with the same type, then Enable.',
      ...cols.map((c) => `ALTER TABLE ${t} ADD COLUMN ${quoteIdent(c)} /* the publisher's type */;`),
    ].join('\n') : undefined
    return {
      kind: 'missing-column', headline,
      cause: `${e.table ?? 'A table'} on this server has no column ${e.columns ?? ''}, but the publisher sends it.`,
      fix: 'Add the missing column here with the same type as on the publisher, then Enable. The subscription waits at this change, so nothing is lost.',
      sql: sql ? { text: sql, name: `Fix ${w.name}` } : undefined,
      actions: sql ? ['sql', 'enable'] : ['enable'],
    }
  }
  if (has(msg, /logical replication target relation .* does not exist/) || e.code === '42P01') {
    return {
      kind: 'missing-table', headline,
      cause: `The publisher sends rows for ${e.table ?? 'a table'}, but this database has no such table.`,
      fix: `Create ${e.table ?? 'the table'} here with the same columns as on the publisher, then Enable. If it should not be copied here, remove it from the publication on the publisher and Refresh publication.`,
      sql: t ? { text: `-- ${w.name} stopped: ${e.table} does not exist here.\n-- Create it with the publisher's columns (copy its CREATE TABLE from the publisher), then Enable.\nCREATE TABLE ${t} (\n  -- the publisher's columns\n);`, name: `Fix ${w.name}` } : undefined,
      actions: t ? ['sql', 'enable', 'refresh'] : ['enable', 'refresh'],
    }
  }
  if (e.code === '42501' || has(msg, /permission denied|cannot replicate into relation with row-level security/)) {
    const rls = has(msg, /row-level security/)
    const sql = t ? (rls
      ? `-- ${w.name} runs as ${w.owner}, which row-level security stops on ${e.table}.\nALTER ROLE ${quoteIdent(w.owner)} BYPASSRLS;`
      : `-- ${w.name} runs as ${w.owner}, which may not write to ${e.table}.\nGRANT INSERT, UPDATE, DELETE, TRUNCATE ON ${t} TO ${quoteIdent(w.owner)};`) : undefined
    return {
      kind: 'permission', headline,
      cause: rls
        ? `${e.table ?? 'The table'} has row-level security, and ${w.owner}, who owns the subscription, is not allowed past it.`
        : `${w.owner}, who owns the subscription, is not allowed to write to ${e.table ?? 'the table'}. The subscription applies changes as its owner.`,
      fix: rls
        ? `Let ${w.owner} bypass row-level security (or turn it off on the table), then Enable.`
        : `Give ${w.owner} INSERT, UPDATE, DELETE and TRUNCATE on ${e.table ?? 'the table'}, then Enable. Nothing is lost meanwhile.`,
      sql: sql ? { text: sql, name: `Fix ${w.name}` } : undefined,
      actions: sql ? ['sql', 'enable'] : ['enable'],
    }
  }
  if (e.code === '23502' || has(msg, /violates not-null constraint/)) {
    const col = e.columns ? quoteIdent(e.columns) : ''
    return {
      kind: 'not-null', headline,
      cause: `${e.table ?? 'A table'} here requires a value in ${e.columns ?? 'a column'}, but the publisher's row has none there. The column is stricter here than on the publisher, or the publisher does not have it.`,
      fix: `Allow empty values in ${e.columns ?? 'that column'} here, or give it a default, then Enable.`,
      sql: t && col ? { text: `-- ${w.name} stopped: ${e.table}.${e.columns} is NOT NULL here but empty on the publisher.\nALTER TABLE ${t} ALTER COLUMN ${col} DROP NOT NULL;\n-- or: ALTER TABLE ${t} ALTER COLUMN ${col} SET DEFAULT ...;`, name: `Fix ${w.name}` } : undefined,
      actions: t && col ? ['sql', 'enable'] : withSkip(['enable']),
    }
  }
  if (e.code === '23503' || has(msg, /violates foreign key constraint/)) {
    return {
      kind: 'foreign-key', headline,
      cause: `A row for ${e.table ?? 'a table'} points at a row that is not on this server${e.constraint ? ` (${e.constraint})` : ''}${e.detail ? `: ${e.detail}` : ''}. The table it points at is not replicated, or is behind.`,
      fix: `Add the table it points at to the publication (then Refresh publication), or drop the foreign key here: a subscriber does not need it, the publisher already checks it. Then Enable.`,
      sql: t && e.constraint ? { text: `-- ${w.name} stopped on ${e.constraint}.\nALTER TABLE ${t} DROP CONSTRAINT ${quoteIdent(e.constraint)};`, name: `Fix ${w.name}` } : undefined,
      actions: withSkip(t && e.constraint ? ['sql', 'enable'] : ['enable']),
    }
  }
  if (e.code === '23514' || has(msg, /violates check constraint/)) {
    return {
      kind: 'check', headline,
      cause: `A row for ${e.table ?? 'a table'} breaks ${e.constraint ?? 'a check'} that exists here and not on the publisher.`,
      fix: `Drop or loosen ${e.constraint ?? 'the check'} here to match the publisher, then Enable.`,
      sql: t && e.constraint ? { text: `ALTER TABLE ${t} DROP CONSTRAINT ${quoteIdent(e.constraint)};`, name: `Fix ${w.name}` } : undefined,
      actions: withSkip(t && e.constraint ? ['sql', 'enable'] : ['enable']),
    }
  }
  if (has(msg, /incorrect binary data format|wrong data type|invalid input syntax for type|out of range for type/) || e.code === '22P02' || e.code === '22003' || e.code === '42804') {
    const binary = w.binary === 'on' && has(msg, /binary/)
    return {
      kind: 'type', headline,
      cause: `A column of ${e.table ?? 'a table'} has a different type here than on the publisher, so the value does not fit${binary ? ' in binary form' : ''}.`,
      fix: binary
        ? 'Turn binary off in Options (text form converts between types), then Enable.'
        : `Change the column here to the publisher's type, then Enable.`,
      actions: binary ? ['options', 'enable'] : ['enable'],
    }
  }
  if (has(msg, /could not connect to the publisher/)) {
    const why = has(msg, /password authentication failed/) ? 'the publisher refused the user name or password'
      : has(msg, /no pg_hba\.conf entry/) ? 'the publisher\'s pg_hba.conf does not let this server in'
      : has(msg, /timeout|timed out|Connection refused|could not translate host name|No route to host/i) ? 'this server cannot reach the publisher over the network'
      : 'the publisher refused the connection'
    return {
      kind: 'connection', headline,
      cause: `${w.name} cannot connect to its publisher: ${why}.`,
      fix: has(msg, /pg_hba/) ? 'Add a line for this server to the publisher\'s pg_hba.conf and reload it there, then Enable.'
        : has(msg, /password/) ? 'Enter the connection again with the right password (Change connection), then Enable.'
        : 'Check the publisher is up and reachable from this server, or fix the connection (Change connection), then Enable.',
      actions: ['connection', 'enable'],
    }
  }
  if (has(msg, /replication slot .* does not exist/)) {
    return {
      kind: 'slot-missing', headline,
      cause: `The replication slot ${w.slot ?? ''} on the publisher is gone, so the publisher no longer keeps changes for ${w.name}.`,
      fix: `Create the slot again on the publisher (SELECT pg_create_logical_replication_slot('${w.slot ?? 'slot'}', 'pgoutput');), then Enable. Changes made while it was gone are not sent; Refresh with copy data if they matter.`,
      actions: ['enable', 'refresh'],
    }
  }
  if (has(msg, /replication slot .* is active for PID/)) {
    return {
      kind: 'slot-busy', headline,
      cause: `Another subscriber is using the slot ${w.slot ?? ''} on the publisher.`,
      fix: 'Give this subscription its own slot (Options › slot_name) or stop the other subscriber, then Enable.',
      actions: ['options', 'enable'],
    }
  }
  if (has(msg, /publication .* does not exist/)) {
    return {
      kind: 'publication-missing', headline,
      cause: `The publisher has no publication by that name: ${msg.replace(/^.*?(publication "[^"]+" does not exist).*$/s, '$1')}.`,
      fix: 'Point the subscription at publications that exist on the publisher (Change publications), then Enable.',
      actions: ['publications', 'enable'],
    }
  }
  if (has(msg, /has already been removed|can no longer get changes from replication slot|slot .* has been invalidated/)) {
    return {
      kind: 'slot-lost', headline,
      cause: 'The publisher threw away changes this subscription had not received yet, so it can never catch up from where it is.',
      fix: 'Drop the subscription and create it again with Copy existing rows on; the tables are copied afresh.',
      actions: [],
    }
  }
  return {
    kind: 'other', headline,
    cause: `The server says: ${msg}${in_ ? ` (${e.table})` : ''}.`,
    fix: `Fix what the message names, then Enable.${skipLSN ? ` If the change does not matter here, skip ${txn}.` : ''}`,
    actions: withSkip(['enable']),
  }
}

/** Whether a subscription's row needs a why: off or worker-less with errors. */
export function needsWhy(s: { enabled: boolean; workerPid?: number; applyErrors?: number | null; syncErrors?: number | null }): boolean {
  const errs = (s.applyErrors ?? 0) + (s.syncErrors ?? 0)
  return errs > 0 && (!s.enabled || !s.workerPid)
}
