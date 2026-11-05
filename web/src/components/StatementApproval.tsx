import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react'
import { Alert, Button, Code, Group, Loader, Modal, ScrollArea, Stack, Text, Textarea, ThemeIcon } from '@mantine/core'
import { IconCheck, IconClockHour4, IconShieldCheck } from '@tabler/icons-react'
import { FS } from '~/theme'
import { useQueryClient } from '@tanstack/react-query'
import { askStatements, cancelRequest, changeApproval, statementAsk } from '~/lib/live'
import { notifyOk, notifyWarn } from '~/lib/notify'
import { combineChecks, deciderWords, impactWords, shortName, type ChangeImpact, type CheckLike } from '~/lib/changeApproval'
import { EnvBadge, ImpactTiles, People } from '~/components/ChangeImpact'
import type { ChangeApproval, DBLabel, StatementItem } from '~/types/domain'

/*
Approval for each statement on a database whose changes need it (its Safety
switch, on by default for production).

The control plane stops a statement that changes such a database
(code AR007) unless it comes with an approved ask for exactly it: same text,
same values, from the same person, within the hour, once. So the console sends
the statement as it would anyway; when the answer is AR007 it asks here, waits
for someone else to approve it in Approvals, and sends it again with the
approval's id. Nothing is worked out twice: the control plane is what says a
statement needs it.

Before asking, each statement is run once and rolled back (a check, with a
preview of the rows it changes), so the ask carries what it would change: the
approver sees the counts and the first rows, and once approved the statement
stops if it would change more rows than that (code AR008).
*/

export const NEEDS_APPROVAL = 'AR007'
export const OVER_CAP = 'AR008'
/** Checked before asking, one by one; an ask of more is sent with what its caller says. */
const MAX_CHECKED = 50
const MIN_REASON = 20
const POLL_MS = 3000

type Phase =
  | { kind: 'form'; error?: string }
  | { kind: 'sending' }
  | { kind: 'waiting'; id: string; cancelling?: boolean; error?: string }
  | { kind: 'denied'; by?: string | null; note?: string | null }
  | { kind: 'cancelled' }
  | { kind: 'expired' }

interface Open {
  items: StatementItem[]
  summary?: Record<string, unknown>
  /** Why the control plane stopped it, as it said. */
  why?: string
  resolve: (id: string | null) => void
}

/** ask(items) resolves with an approved request's id, or null if none came. */
export type AskApproval = (items: StatementItem[], opts?: { summary?: Record<string, unknown>; why?: string }) => Promise<string | null>

/** Runs one statement and rolls it back, with a preview; null when it cannot be checked. */
export type CheckStatement = (sql: string, params?: (string | null)[]) => Promise<CheckLike | null>

export interface ApprovalOptions {
  label?: DBLabel
  /** The person asking, so the prompt never names them among who can decide. */
  me?: string
  check?: CheckStatement
}

export function useStatementApproval(databaseId: string, databaseName: string, opts: ApprovalOptions = {}): { ask: AskApproval; dialog: ReactNode } {
  const [open, setOpen] = useState<Open | null>(null)
  const [phase, setPhase] = useState<Phase>({ kind: 'form' })
  const [reason, setReason] = useState('')
  const [approval, setApproval] = useState<ChangeApproval | null>(null)
  // undefined: counting; null: nothing could be counted.
  const [counted, setCounted] = useState<{ impact: ChangeImpact; rows: (number | undefined)[] } | null | undefined>(null)
  const openRef = useRef<Open | null>(null)
  openRef.current = open
  const checkRef = useRef(opts.check)
  checkRef.current = opts.check
  const qc = useQueryClient()

  const ask: AskApproval = useCallback((items, o) => new Promise<string | null>((resolve) => {
    openRef.current?.resolve(null)
    setPhase({ kind: 'form' })
    setOpen({ items, summary: o?.summary, why: o?.why, resolve })
    // A big import is asked for with what it says it is, not checked
    // statement by statement.
    const check = items.length <= MAX_CHECKED ? checkRef.current : undefined
    setCounted(check ? undefined : null)
    if (check) {
      void (async () => {
        const checks: (CheckLike | null)[] = []
        for (const it of items) {
          try { checks.push(await check(it.sql, it.params)) } catch { checks.push(null) }
        }
        if (openRef.current?.items !== items) return
        const impact = combineChecks(checks)
        setCounted(impact.checked ? { impact, rows: items.map((_, i) => checks[i]?.rows) } : null)
      })()
    }
  }), [])

  // Who can decide, for the prompt and the wait.
  useEffect(() => {
    if (!open) return
    let stop = false
    changeApproval(databaseId).then((a) => { if (!stop) setApproval(a) }, () => { if (!stop) setApproval(null) })
    return () => { stop = true }
  }, [open, databaseId])

  const close = useCallback((id: string | null) => {
    openRef.current?.resolve(id)
    setOpen(null)
  }, [])

  // Waiting: read how the ask stands until someone decides it.
  useEffect(() => {
    if (phase.kind !== 'waiting') return
    let stop = false
    const tick = async () => {
      try {
        const a = await statementAsk(databaseId, phase.id)
        if (stop) return
        if (a.state === 'approved') {
          notifyOk('Approved', `${a.decidedBy ?? 'Someone'} approved it; running it now.`)
          close(phase.id)
          return
        }
        if (a.state === 'denied') { setPhase({ kind: 'denied', by: a.decidedBy, note: a.note }); return }
        if (a.state === 'expired') { setPhase({ kind: 'expired' }); return }
        if (a.state === 'cancelled') { setPhase({ kind: 'cancelled' }); return }
      } catch {
        // A blip; the next tick tries again.
      }
      if (!stop) timer = setTimeout(tick, POLL_MS)
    }
    let timer = setTimeout(tick, POLL_MS)
    return () => { stop = true; clearTimeout(timer) }
  }, [phase.kind, phase.kind === 'waiting' ? phase.id : '', databaseId, close])

  // Closing the wait takes the ask back. Nothing else can use an approval
  // given later: the statement runs from this dialog, so an ask left behind
  // would sit in every approver's queue and bell, waiting for a decision
  // that changes nothing.
  const cancelAsk = useCallback(async (id: string) => {
    setPhase({ kind: 'waiting', id, cancelling: true })
    try {
      await cancelRequest(id)
      notifyOk('Ask cancelled', 'Nothing ran. The approvers no longer see it as waiting.')
      close(null)
    } catch (e) {
      const msg = e instanceof Error ? e.message : 'the ask was not cancelled'
      if (/already approved/.test(msg)) {
        // Approved while this was being cancelled. It is not run: the person
        // asked for it not to be.
        notifyWarn('Approved before it was cancelled', `${msg}. Nothing ran; ask again when you want to run it.`)
        close(null)
        return
      }
      if (/already denied|cancelled/.test(msg)) { close(null); return }
      setPhase({ kind: 'waiting', id, error: `Not cancelled: ${msg}` })
    } finally {
      void qc.invalidateQueries({ queryKey: ['requests'] })
    }
  }, [close, qc])

  const dismiss = () => {
    if (phase.kind === 'waiting') {
      if (!phase.cancelling) void cancelAsk(phase.id)
      return
    }
    close(null)
  }

  const send = async () => {
    if (!open) return
    setPhase({ kind: 'sending' })
    const c = counted
    const items = c ? open.items.map((it, i) => (c.rows[i] === undefined ? it : { ...it, rows: c.rows[i] })) : open.items
    const summary = c ? { ...open.summary, ...c.impact } : open.summary
    try {
      const a = await askStatements(databaseId, { items, summary, justification: reason.trim() })
      setPhase({ kind: 'waiting', id: a.requestId })
    } catch (e) {
      setPhase({ kind: 'form', error: e instanceof Error ? e.message : 'the ask was not sent' })
    }
  }

  const n = open?.items.length ?? 0
  const impact = counted ? counted.impact : null
  const rowsWord = impact ? impactWords(impact) : ''
  const others = (approval?.approvers === 'editors' ? approval.editors : [])
    .filter((e) => e.toLowerCase() !== opts.me?.toLowerCase())
  const deciders = deciderWords(databaseName, approval, opts.me)
  const dialog = (
    <Modal opened={open !== null} onClose={dismiss} centered size="lg"
      title={<Group gap="xs"><IconShieldCheck size={18} /><Text fw={650}>Ask for approval</Text>{opts.label && <EnvBadge label={opts.label} />}</Group>}>
      {open && (
        <Stack gap="sm">
          <Text size={FS.body}>
            Changes on {databaseName} need a second person's approval. {n === 1 ? 'This statement runs' : `These ${n} statements run`} once
            {' '}{deciders} approves {n === 1 ? 'it' : 'them'}, never you. That holds for Owner and DBA too.
          </Text>
          <ScrollArea.Autosize mah={220} type="auto">
            <Stack gap={6}>
              {open.items.map((it, i) => (
                <Code key={i} block style={{ whiteSpace: 'pre-wrap', fontSize: 12 }}>
                  {it.sql}{it.params?.length ? `\n-- values: ${it.params.map((p) => (p === null ? 'NULL' : JSON.stringify(p))).join(', ')}` : ''}
                </Code>
              ))}
            </Stack>
          </ScrollArea.Autosize>
          {counted === undefined && (
            <Group gap="xs"><Loader size="xs" /><Text size={FS.meta} c="dimmed">Counting what it changes: running it once and rolling it back…</Text></Group>
          )}
          {impact && (
            <Stack gap={4}>
              <Text size={FS.micro} c="dimmed" tt="uppercase" fw={600}>Impact, from a dry run now</Text>
              <ImpactTiles impact={impact} />
            </Stack>
          )}
          {(phase.kind === 'form' || phase.kind === 'sending') && (
            <>
              <Textarea label="Why" description={`The approver reads this. At least ${MIN_REASON} characters.`} autosize minRows={2}
                value={reason} onChange={(e) => setReason(e.currentTarget.value)} data-autofocus
                placeholder="Order 7 was imported twice; removing the copy." />
              {phase.kind === 'form' && phase.error && <Alert color="rose" variant="light">{phase.error}</Alert>}
              <Text size={FS.micro} c="dimmed">
                Who can decide: {approval?.approvers === 'editors'
                  ? <>admins, approvers{others.length ? <>, and {others.length === 1 ? 'one other editor' : `${others.length} other editors`} ({others.map(shortName).join(', ')})</> : ''}</>
                  : 'admins and approvers'}. They see it in the bell and on Approvals.
                {' '}Once approved, {n === 1 ? 'it runs' : 'each runs'} once, exactly as shown, within the hour
                {rowsWord ? `, and stops if it would change more than ${rowsWord.split(' · ')[0]}` : ''}. A change to the text or a value needs a new ask.
              </Text>
              <Group justify="flex-end">
                <Button variant="default" onClick={() => close(null)}>Cancel</Button>
                <Button onClick={() => void send()} loading={phase.kind === 'sending'}
                  disabled={reason.trim().length < MIN_REASON || counted === undefined}>
                  Ask for approval
                </Button>
              </Group>
            </>
          )}
          {phase.kind === 'waiting' && (
            <>
              <Stack gap="xs" pl="xs" style={{ borderLeft: '2px solid var(--mantine-color-default-border)' }}>
                <Group gap="sm" wrap="nowrap" align="flex-start">
                  <ThemeIcon size={26} radius="xl" variant="light" color="teal"><IconCheck size={14} /></ThemeIcon>
                  <Text size={FS.body} fw={600}>You asked{rowsWord ? ` · ${rowsWord}` : ''}</Text>
                </Group>
                <Group gap="sm" wrap="nowrap" align="flex-start">
                  <ThemeIcon size={26} radius="xl" variant="light" color="amber"><Loader size={12} color="amber" /></ThemeIcon>
                  <Stack gap={4}>
                    <Text size={FS.body} fw={600}>Waiting for one of them</Text>
                    {others.length > 0 && <People emails={others} />}
                    <Text size={FS.meta} c="dimmed">
                      {others.length ? `${others.map(shortName).join(', ')} (editors of ${databaseName}), and the admins and approvers. Not you.` : 'An admin or an approver. Not you.'}
                      {' '}It runs as soon as they approve, while this stays open.
                    </Text>
                  </Stack>
                </Group>
                <Group gap="sm" wrap="nowrap" align="flex-start">
                  <ThemeIcon size={26} radius="xl" variant="light" color="slate"><IconClockHour4 size={14} /></ThemeIcon>
                  <Text size={FS.body} c="dimmed">
                    Then it runs once{rowsWord ? `, and stops if it would change more than ${rowsWord.split(' · ')[0]}` : ''}.
                  </Text>
                </Group>
              </Stack>
              {phase.error && <Alert color="rose" variant="light">{phase.error}</Alert>}
              <Text size={FS.micro} c="dimmed">Waiting for someone to approve it in Approvals. Closing this cancels the ask, so nobody is left deciding it.</Text>
              <Group justify="flex-end">
                <Button variant="default" color="rose" loading={phase.cancelling} onClick={() => void cancelAsk(phase.id)}>Cancel the ask</Button>
              </Group>
            </>
          )}
          {phase.kind === 'denied' && (
            <>
              <Alert color="rose" variant="light" title="Denied">
                {phase.by ?? 'The approver'} denied it{phase.note ? `: ${phase.note}` : '.'} Nothing ran.
              </Alert>
              <Group justify="flex-end"><Button variant="default" onClick={() => close(null)}>Close</Button></Group>
            </>
          )}
          {phase.kind === 'cancelled' && (
            <>
              <Alert color="slate" variant="light" title="Cancelled">The ask was cancelled, from another tab or window. Nothing ran.</Alert>
              <Group justify="flex-end">
                <Button variant="default" onClick={() => close(null)}>Close</Button>
                <Button onClick={() => setPhase({ kind: 'form' })}>Ask again</Button>
              </Group>
            </>
          )}
          {phase.kind === 'expired' && (
            <>
              <Alert color="amber" variant="light">The approval expired before it was used. Nothing ran; ask again.</Alert>
              <Group justify="flex-end">
                <Button variant="default" onClick={() => close(null)}>Close</Button>
                <Button onClick={() => setPhase({ kind: 'form' })}>Ask again</Button>
              </Group>
            </>
          )}
        </Stack>
      )}
    </Modal>
  )
  return { ask, dialog }
}
