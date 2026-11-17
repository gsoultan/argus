import {
  Anchor, Box, Button, Group, Progress, Skeleton, Stack, Text, ThemeIcon,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import {
  IconAlertTriangle, IconCheck, IconExternalLink, IconQuestionMark, IconX,
} from '@tabler/icons-react'
import { SectionCard } from '~/components/page'
import { ErrorState } from '~/components/ErrorState'
import { absTime, relTime } from '~/components/primitives'
import {
  CONFIRM_ITEMS, RUNBOOK_URL, confirmTone, readiness, readinessChecks, readinessKey, useConfirmReadiness,
  type ReadinessView, type Tone,
} from '~/lib/controlReadiness'
import { notifyOk, run } from '~/lib/notify'
import { FS } from '~/theme'

/**
 * Settings › Control plane › If Argus goes down.
 *
 * What should be ready before this control plane stops. The first card is
 * what Argus can see for itself; the second is what only a person can say,
 * confirmed here, audited, and asked again after 90 days. Nothing on the page
 * is a setting: each line links to where it is changed.
 */
export function ControlReadinessSection({ onOpen }: { onOpen?: (section: string) => void }) {
  const { data, error, isPending, refetch } = useQuery({
    queryKey: readinessKey, queryFn: readiness, refetchInterval: 30_000, retry: false,
  })
  if (isPending) return <Skeleton h={520} />
  if (!data) {
    return <ErrorState title="argus-control did not say how ready it is" error={error} reset={() => void refetch()} />
  }
  return <ControlReadinessPanel v={data} onOpen={onOpen} />
}

/** How many lines need a person: the count beside the menu entry. */
export function readinessNeeds(v: ReadinessView, now = Date.now()): number {
  return readinessChecks(v, now).filter((l) => l.tone !== 'ok').length +
    CONFIRM_ITEMS.filter((i) => confirmTone(v.confirmations[i.id]) !== 'ok').length
}

export function ControlReadinessPanel({ v, onOpen, now = Date.now() }: {
  v: ReadinessView; onOpen?: (section: string) => void; now?: number
}) {
  const checks = readinessChecks(v, now)
  const tones = [...checks.map((c) => c.tone), ...CONFIRM_ITEMS.map((i) => confirmTone(v.confirmations[i.id]))]
  const total = tones.length
  const ready = tones.filter((t) => t === 'ok').length
  const ask = tones.filter((t) => t === 'ask').length
  const need = total - ready - ask
  const links: Record<string, { label: string; section: string } | undefined> = {
    outside: { label: 'Alerts', section: 'alerts' },
    backups: { label: 'Backups', section: 'control-backups' },
    ssh: { label: 'Logins', section: 'access' },
    recovery: { label: 'Account security', section: 'security' },
  }

  return (
    <Stack gap="sm" data-testid="control-readiness">
      <SectionCard title="If Argus goes down"
        description={<>What still works on your servers when this control plane stops, and what to have ready beforehand.
          Argus checks what it can see. The rest you confirm, and it asks again every {v.confirmDays} days.</>}
        action={
          <Button size="xs" variant="default" component="a" href={RUNBOOK_URL} target="_blank" rel="noreferrer"
            rightSection={<IconExternalLink size={13} />} data-testid="runbook-link">
            Open the runbook
          </Button>
        }>
        <Stack gap={6}>
          <Progress.Root size="md" aria-label={`${ready} of ${total} ready`}>
            <Progress.Section value={(ready / total) * 100} color="teal" />
            <Progress.Section value={(need / total) * 100} color="amber" />
            <Progress.Section value={(ask / total) * 100} color="gray.3" />
          </Progress.Root>
          <Text size={FS.meta} c="dimmed" data-testid="readiness-score">
            <Text span fw={600} c="var(--mantine-color-text)">{ready} of {total} ready</Text>
            {need > 0 && <> · {need} need{need === 1 ? 's' : ''} you</>}
            {ask > 0 && <> · {ask} to confirm</>}
          </Text>
          <Text size={FS.meta} c="dimmed">
            The runbook is kept outside Argus, so it can be read while Argus is down. Keep a printed copy too.
          </Text>
          {v.lastOutage && (
            <Text size={FS.meta} c="dimmed" data-testid="last-outage">
              Last outage: {absTime(v.lastOutage.startedAt)} to {absTime(v.lastOutage.endedAt)}, found
              by {v.lastOutage.foundBy}.
            </Text>
          )}
        </Stack>
      </SectionCard>

      <SectionCard title="Argus checks these">
        <Stack gap={0}>
          {checks.map((c, i) => {
            const link = links[c.id]
            return (
              <Line key={c.id} first={i === 0} tone={c.tone} title={c.title} testId={`check-${c.id}`}
                action={link && onOpen ? (
                  <Anchor size="sm" component="button" onClick={() => onOpen(link.section)}>{link.label} ›</Anchor>
                ) : undefined}>
                {c.detail}
              </Line>
            )
          })}
        </Stack>
      </SectionCard>

      <SectionCard title="You confirm these"
        description="Argus cannot see these. Confirming one records your name and the time in the audit log.">
        <Stack gap={0}>
          {CONFIRM_ITEMS.map((item, i) => (
            <ConfirmLine key={item.id} first={i === 0} v={v} id={item.id} title={item.title} detail={item.detail} />
          ))}
        </Stack>
      </SectionCard>
    </Stack>
  )
}

function ConfirmLine({ v, id, title, detail, first }: {
  v: ReadinessView; id: (typeof CONFIRM_ITEMS)[number]['id']; title: string; detail: string; first: boolean
}) {
  const confirm = useConfirmReadiness()
  const c = v.confirmations[id]
  const tone = confirmTone(c)
  return (
    <Line first={first} tone={tone} title={title} testId={`confirm-${id}`}
      note={c ? (
        c.lapsed
          ? <>Confirmed by {c.by} {relTime(c.at)}. That was more than {v.confirmDays} days ago: check it again.</>
          : <>Confirmed by {c.by} on {absTime(c.at)}. Asks again on {absTime(c.until)}.</>
      ) : undefined}
      action={
        <Button size="xs" variant={tone === 'ok' ? 'default' : 'filled'} loading={confirm.isPending}
          data-testid={`confirm-${id}-button`}
          onClick={() => void run(async () => {
            await confirm.mutateAsync(id)
            notifyOk('Confirmed', 'Recorded in the audit log with your name.')
          }, { failure: 'Could not confirm' })}>
          {c ? 'Confirm again' : 'Confirm'}
        </Button>
      }>
      {detail}
    </Line>
  )
}

const TONE = {
  ok: { color: 'teal', Icon: IconCheck },
  warn: { color: 'amber', Icon: IconAlertTriangle },
  bad: { color: 'rose', Icon: IconX },
  ask: { color: 'gray', Icon: IconQuestionMark },
} as const

function Line({ tone, title, children, note, action, first, testId }: {
  tone: Tone; title: string; children?: React.ReactNode; note?: React.ReactNode
  action?: React.ReactNode; first?: boolean; testId?: string
}) {
  const { color, Icon } = TONE[tone]
  return (
    <Group gap="sm" align="flex-start" wrap="nowrap" py="sm" data-testid={testId} data-tone={tone}
      style={first ? undefined : { borderTop: '1px solid var(--mantine-color-default-border)' }}>
      <ThemeIcon size={24} radius="xl" color={color} variant="light" aria-label={tone}><Icon size={14} /></ThemeIcon>
      <Box style={{ flex: 1, minWidth: 0 }}>
        <Text size={FS.body} fw={600}>{title}</Text>
        {children && <Text size={FS.meta} c="dimmed" lh={1.5} style={{ wordBreak: 'break-word' }}>{children}</Text>}
        {note && <Text size={FS.micro} c="dimmed" mt={4}>{note}</Text>}
      </Box>
      {action && <Box style={{ flexShrink: 0 }}>{action}</Box>}
    </Group>
  )
}


/** The menu's count: lines that need a person, once the page has been read. */
export function useReadinessNeeds(enabled: boolean): number {
  const { data } = useQuery({ queryKey: readinessKey, queryFn: readiness, enabled, refetchInterval: 5 * 60_000, retry: false })
  return data ? readinessNeeds(data) : 0
}
