import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react'
import { Alert, Button, Code, Group, Loader, Modal, ScrollArea, Stack, Text, Textarea } from '@mantine/core'
import { IconShieldCheck } from '@tabler/icons-react'
import { FS } from '~/theme'
import { askStatements, statementAsk } from '~/lib/live'
import { notifyOk } from '~/lib/notify'
import type { StatementItem } from '~/types/domain'

/*
Approval for each statement on a production database.

The control plane stops a statement that changes a production database
(code AR007) unless it comes with an approved ask for exactly it: same text,
same values, from the same person, within the hour, once. So the console sends
the statement as it would anyway; when the answer is AR007 it asks here, waits
for someone else to approve it in Approvals, and sends it again with the
approval's id. Nothing is worked out twice: the control plane is what says a
statement needs it.
*/

export const NEEDS_APPROVAL = 'AR007'
const MIN_REASON = 20
const POLL_MS = 3000

type Phase =
  | { kind: 'form'; error?: string }
  | { kind: 'sending' }
  | { kind: 'waiting'; id: string }
  | { kind: 'denied'; by?: string | null; note?: string | null }
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

export function useStatementApproval(databaseId: string, databaseName: string): { ask: AskApproval; dialog: ReactNode } {
  const [open, setOpen] = useState<Open | null>(null)
  const [phase, setPhase] = useState<Phase>({ kind: 'form' })
  const [reason, setReason] = useState('')
  const openRef = useRef<Open | null>(null)
  openRef.current = open

  const ask: AskApproval = useCallback((items, opts) => new Promise<string | null>((resolve) => {
    openRef.current?.resolve(null)
    setPhase({ kind: 'form' })
    setOpen({ items, summary: opts?.summary, why: opts?.why, resolve })
  }), [])

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
      } catch {
        // A blip; the next tick tries again.
      }
      if (!stop) timer = setTimeout(tick, POLL_MS)
    }
    let timer = setTimeout(tick, POLL_MS)
    return () => { stop = true; clearTimeout(timer) }
  }, [phase, databaseId, close])

  const send = async () => {
    if (!open) return
    setPhase({ kind: 'sending' })
    try {
      const a = await askStatements(databaseId, { items: open.items, summary: open.summary, justification: reason.trim() })
      setPhase({ kind: 'waiting', id: a.requestId })
    } catch (e) {
      setPhase({ kind: 'form', error: e instanceof Error ? e.message : 'the ask was not sent' })
    }
  }

  const n = open?.items.length ?? 0
  const dialog = (
    <Modal opened={open !== null} onClose={() => close(null)} centered size="lg"
      title={<Group gap="xs"><IconShieldCheck size={18} /><Text fw={650}>Ask for approval</Text></Group>}>
      {open && (
        <Stack gap="sm">
          <Text size={FS.body}>
            {databaseName} is production. {n === 1 ? 'This statement changes it, so it runs' : `These ${n} statements change it, so they run`} once
            someone else approves {n === 1 ? 'it' : 'them'}: an admin or an approver, never you. That holds for Owner and DBA too.
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
          {(phase.kind === 'form' || phase.kind === 'sending') && (
            <>
              <Textarea label="Why" description={`The approver reads this. At least ${MIN_REASON} characters.`} autosize minRows={2}
                value={reason} onChange={(e) => setReason(e.currentTarget.value)} data-autofocus
                placeholder="Order 7 was imported twice; removing the copy." />
              {phase.kind === 'form' && phase.error && <Alert color="rose" variant="light">{phase.error}</Alert>}
              <Text size={FS.micro} c="dimmed">
                Once approved, {n === 1 ? 'it runs' : 'each runs'} once, exactly as shown, within the hour. A change to the text or a value needs a new ask.
              </Text>
              <Group justify="flex-end">
                <Button variant="default" onClick={() => close(null)}>Cancel</Button>
                <Button onClick={() => void send()} loading={phase.kind === 'sending'} disabled={reason.trim().length < MIN_REASON}>
                  Ask for approval
                </Button>
              </Group>
            </>
          )}
          {phase.kind === 'waiting' && (
            <>
              <Group gap="sm" wrap="nowrap">
                <Loader size="sm" />
                <Text size={FS.body}>Waiting for someone to approve it in Approvals. It runs as soon as they do; you can keep this open or come back.</Text>
              </Group>
              <Group justify="flex-end">
                <Button variant="default" onClick={() => close(null)}>Stop waiting</Button>
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
