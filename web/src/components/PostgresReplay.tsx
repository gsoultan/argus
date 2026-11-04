import { Anchor, Badge, Box, Group, Stack, Text } from '@mantine/core'
import { useMemo, useState } from 'react'
import { EmptyState } from '~/components/page'
import { IconDatabaseOff } from '@tabler/icons-react'
import { FS } from '~/theme'

/*
A database session's replay: the statements (or Redis and RabbitMQ commands),
in order, each with what the engine answered. The recording is JSON lines; each line was written before its
statement ran and hashes the line before it, so the list is what happened,
not what the person says happened.

Parsed once per recording and rendered as a plain list: a session is at most
a few thousand lines, so there is nothing to window.

An `admin` line is an administrative operation (an extension, a publication,
a subscription) the agent ran through its helper as the server's postgres
account rather than as the person's role; it is shown with that said, and any
password in it was redacted before it was recorded.
*/

interface Line {
  seq: number
  t: string
  kind: 'open' | 'stmt' | 'check' | 'admin' | 'done' | 'error' | 'cancel' | 'close'
  sql?: string
  req?: number
  tag?: string
  rows?: number
  ms?: number
  code?: string
  error?: string
  truncated?: boolean
  actor?: string
  role?: string
  level?: string
}

interface Statement {
  seq: number
  t: string
  sql: string
  tag?: string
  rows?: number
  ms?: number
  code?: string
  error?: string
  truncated?: boolean
  cancelled?: boolean
  /** Ran through the helper as the server's postgres account. */
  admin?: boolean
  /** Run inside a transaction that was rolled back: counted, not kept. */
  check?: boolean
}

export function parseRecording(text: string): { open?: Line; close?: Line; statements: Statement[] } {
  const statements: Statement[] = []
  const byReq = new Map<number, Statement>()
  let open: Line | undefined
  let close: Line | undefined
  for (const raw of text.split('\n')) {
    if (!raw.trim()) continue
    let l: Line
    try {
      l = JSON.parse(raw) as Line
    } catch {
      continue
    }
    switch (l.kind) {
      case 'open':
        open = l
        break
      case 'close':
        close = l
        break
      case 'stmt':
      case 'check':
      case 'admin': {
        const s: Statement = { seq: l.seq, t: l.t, sql: l.sql ?? '', admin: l.kind === 'admin' || undefined, check: l.kind === 'check' || undefined }
        statements.push(s)
        if (l.req) byReq.set(l.req, s)
        break
      }
      case 'done':
      case 'error':
      case 'cancel': {
        const s = l.req ? byReq.get(l.req) : undefined
        if (!s) {
          // A command the agent refused before sending it (Redis and
          // RabbitMQ) is one line: what was asked and why not.
          if (l.kind === 'error' && l.sql) statements.push({ seq: l.seq, t: l.t, sql: l.sql, error: l.error })
          break
        }
        if (l.kind === 'done') {
          s.tag = l.tag; s.rows = l.rows; s.ms = l.ms; s.truncated = l.truncated
        } else if (l.kind === 'error') {
          s.code = l.code; s.error = l.error; s.ms = l.ms
        } else {
          s.cancelled = true
        }
        break
      }
    }
  }
  return { open, close, statements }
}

/**
 * How much of a statement is drawn before "Show all". The recording holds
 * every statement whole (up to the 192 KB a statement may be); this is only
 * how much a list item shows until someone asks for the rest, so a session
 * with a few huge INSERTs stays readable.
 */
export const STATEMENT_PREVIEW_CHARS = 1500

export function StatementText({ sql }: { sql: string }) {
  const [all, setAll] = useState(false)
  const long = sql.length > STATEMENT_PREVIEW_CHARS
  return (
    <>
      <Text size={FS.meta} ff="monospace" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>
        {long && !all ? sql.slice(0, STATEMENT_PREVIEW_CHARS) + ' …' : sql}
      </Text>
      {long && (
        <Anchor component="button" type="button" size="xs" onClick={() => setAll((v) => !v)}>
          {all ? 'Show less' : `Show all ${sql.length.toLocaleString()} characters`}
        </Anchor>
      )}
    </>
  )
}

export function PostgresReplay({ text, verified, error, live }: {
  text?: string; verified?: string; error?: string
  /** A session being watched: the list is still growing. */
  live?: boolean
}) {
  const parsed = useMemo(() => (text ? parseRecording(text) : undefined), [text])
  if (!text) {
    return (
      <Box p="lg">
        <EmptyState icon={IconDatabaseOff} title="This recording is not available to replay"
          description={error ?? 'Waiting for the control plane.'} />
      </Box>
    )
  }
  const { open, statements } = parsed!
  return (
    <Box>
      <Group px="md" py="xs" gap="sm" style={{ borderBottom: '1px solid var(--color-line)' }} wrap="wrap">
        <Text size={FS.meta} fw={600}>Statements</Text>
        <Text size={FS.meta} c="dimmed">{statements.length}</Text>
        {open?.role && <Badge size="xs" variant="outline" color="slate">as {open.role}</Badge>}
        {open?.level && <Badge size="xs" variant="light" color={open.level === 'read' ? 'slate' : 'azure'}>{open.level}</Badge>}
        {verified && (
          <Badge size="xs" variant="light" color={verified === 'intact' ? 'teal' : verified === 'tampered' ? 'rose' : 'amber'} ml="auto">
            chain {verified}
          </Badge>
        )}
      </Group>
      {statements.length === 0 && (
        <Text size={FS.meta} c="dimmed" p="md">{live ? 'No statements yet.' : 'No statements were run in this session.'}</Text>
      )}
      <Stack gap={0}>
        {statements.map((s) => {
          const t0 = open ? Date.parse(open.t) : Date.parse(statements[0]!.t)
          const at = Math.max(0, Date.parse(s.t) - t0)
          return (
            <Box key={s.seq} px="md" py="xs" style={{ borderBottom: '1px solid var(--color-line)' }}>
              <Group gap="sm" wrap="nowrap" align="flex-start">
                <Text size={FS.micro} c="dimmed" ff="monospace" w={56} style={{ flexShrink: 0 }}>
                  +{fmtOffset(at)}
                </Text>
                <Box style={{ minWidth: 0, flex: 1 }}>
                  <StatementText sql={s.sql} />
                  <Group gap={6} mt={4} wrap="wrap">
                    {s.admin && <Badge size="xs" variant="outline" color="amber">as the server’s postgres account</Badge>}
                    {s.check && <Badge size="xs" variant="outline" color="slate">checked, rolled back</Badge>}
                    {s.error ? (
                      <Badge size="xs" variant="light" color="rose">
                        {s.code === '42501' ? 'refused by access' : `error ${s.code ?? ''}`}
                      </Badge>
                    ) : s.cancelled ? (
                      <Badge size="xs" variant="light" color="amber">cancelled</Badge>
                    ) : s.tag ? (
                      <Badge size="xs" variant="light" color="slate">{s.tag}</Badge>
                    ) : live ? (
                      <Badge size="xs" variant="light" color="amber">running</Badge>
                    ) : null}
                    {typeof s.rows === 'number' && s.tag?.startsWith('SELECT') && (
                      <Text size={FS.micro} c="dimmed">{s.rows.toLocaleString()} rows{s.truncated ? ' (cut at the limit)' : ''}</Text>
                    )}
                    {typeof s.ms === 'number' && <Text size={FS.micro} c="dimmed">{s.ms} ms</Text>}
                    {s.error && <Text size={FS.micro} c="rose">{s.error}</Text>}
                  </Group>
                </Box>
              </Group>
            </Box>
          )
        })}
      </Stack>
    </Box>
  )
}

function fmtOffset(ms: number): string {
  const s = Math.floor(ms / 1000)
  const m = Math.floor(s / 60)
  const h = Math.floor(m / 60)
  if (h > 0) return `${h}h${String(m % 60).padStart(2, '0')}m`
  if (m > 0) return `${m}m${String(s % 60).padStart(2, '0')}s`
  return `${s}s`
}
