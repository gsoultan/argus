import { useState } from 'react'
import { Button, Group, Skeleton, Stack, Switch, Text, TextInput } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconLock } from '@tabler/icons-react'
import { SectionCard } from '~/components/page'
import { approvalLocksQuery, ENV_LABEL, useSaveApprovalLocks, type ApprovalLock } from '~/lib/approvalPolicy'
import { run } from '~/lib/notify'
import { FS, SP } from '~/theme'

/**
 * Settings › Organizations, for platform owners: approval policy cells no
 * organization may loosen. Nothing is locked out of the box. An organization
 * sees a locked cell with a lock and the reason given here.
 */
export function ApprovalLocksCard() {
  const { data, isLoading } = useQuery(approvalLocksQuery(true))
  const save = useSaveApprovalLocks()
  const [draft, setDraft] = useState<ApprovalLock[] | null>(null)
  if (isLoading || !data) return <Skeleton h={200} />
  const locks = draft ?? data.locks
  const has = (kind: string, env: string) => locks.find((l) => l.kind === kind && l.env === env)
  const toggle = (kind: string, env: ApprovalLock['env'], on: boolean) =>
    setDraft(on ? [...locks, { kind, env, reason: '' }] : locks.filter((l) => !(l.kind === kind && l.env === env)))
  const setReason = (kind: string, env: string, reason: string) =>
    setDraft(locks.map((l) => (l.kind === kind && l.env === env ? { ...l, reason } : l)))
  const dirty = draft !== null && JSON.stringify(draft) !== JSON.stringify(data.locks)

  return (
    <SectionCard title="What no organization may switch off" icon={IconLock}
      description="These always need a second person, whatever an organization's approval policy says. Each organization sees them locked, with your reason. Nothing is locked by default.">
      <Stack gap="xs">
        {data.kinds.map((k) => k.envs.filter((e) => (k.today[e] ?? 'never') !== 'never').map((env) => {
          const l = has(k.kind, env)
          return (
            <Group key={`${k.kind}/${env}`} gap="sm" wrap="wrap" align="center">
              <Switch checked={Boolean(l)} onChange={(e) => toggle(k.kind, env, e.currentTarget.checked)}
                label={`${k.title} · ${ENV_LABEL[env]}`} style={{ minWidth: 320 }} />
              {l && (
                <TextInput size="xs" placeholder="Reason shown to organizations" value={l.reason} style={{ flex: 1, minWidth: 220 }}
                  onChange={(e) => setReason(k.kind, env, e.currentTarget.value)} />
              )}
            </Group>
          )
        }))}
        {dirty && (
          <Group justify="flex-end" gap={SP.cozy}>
            <Text size={FS.meta} c="dimmed">Applies to every organization at once.</Text>
            <Button size="xs" variant="subtle" color="slate" onClick={() => setDraft(null)}>Discard</Button>
            <Button size="xs" loading={save.isPending}
              onClick={() => void run(async () => { await save.mutateAsync(locks); setDraft(null) },
                { failure: 'Locks not saved', success: ['Locks saved', 'Every organization follows them now.'] })}>
              Save
            </Button>
          </Group>
        )}
      </Stack>
    </SectionCard>
  )
}
