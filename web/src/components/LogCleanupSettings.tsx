import { useState } from 'react'
import {
  ActionIcon, Alert, Badge, Button, Group, NumberInput, Radio, Select, Skeleton, Stack, Switch, TagsInput, Text, TextInput,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconArrowDown, IconArrowUp, IconFileText, IconLock, IconPlus, IconTrash } from '@tabler/icons-react'
import { SectionCard } from '~/components/page'
import { hhmm } from '~/components/server/UpdatesTab'
import { isConfigured, type LogCleanupPolicy, type LogCleanupRule, type LogMode } from '~/lib/live'
import { logPolicyQuery, useSaveLogPolicy } from '~/lib/logQueries'
import { run } from '~/lib/notify'
import { FS, SP } from '~/theme'

export const LOG_MODE_LABEL: Record<LogMode, string> = {
  off: 'Off',
  disk: 'When a disk fills',
  daily: 'Every day in a window',
}

function modeHelp(r: LogCleanupRule): string {
  switch (r.mode) {
    case 'off':
      return 'Argus shows what can be freed and alerts when a disk fills. Nothing is cleaned unless someone presses Clean.'
    case 'disk':
      return `Clean what this rule allows when a disk passes ${r.diskPercent}%, at most once a day, then alert if it is still over.`
    case 'daily':
      return "Clean what this rule allows once a day in the window, by the server's own clock."
  }
}

/** What Argus never cleans, whatever is set here: built into the agent. */
const BUILT_IN = ['auth.log / secure', 'wtmp, btmp, lastlog', 'sudo logs', '/var/log/audit/', 'Windows Security log', 'Argus']

const START_TIMES = Array.from({ length: 48 }, (_, i) => ({ value: String(i * 30), label: hhmm(i * 30) }))

/**
 * Settings › Log cleanup: what a cleanup keeps, per server tag, and whether
 * Argus cleans by itself. A server takes the first rule naming one of its
 * tags, else the one for every other server. Off by default: a new install
 * only shows what can be freed and alerts when a disk fills.
 */
export function LogCleanupSettings({ canEdit }: { canEdit: boolean }) {
  const { data: saved, isLoading } = useQuery(logPolicyQuery())
  const [draft, setDraft] = useState<LogCleanupPolicy | null>(null)
  const save = useSaveLogPolicy()

  if (!isConfigured()) {
    return (
      <Alert color="slate">
        <Text size={FS.body}>Log cleanup is set on a control plane. Connect this console to Argus to set it.</Text>
      </Alert>
    )
  }
  if (isLoading || !saved) return <Skeleton h={320} />

  const p = draft ?? saved
  const changed = draft !== null && JSON.stringify(draft) !== JSON.stringify(saved)
  const defId = saved.rules.find((r) => r.tag === '')?.id
  const def = p.rules.find((r) => r.id === defId)
  const tagged = p.rules.filter((r) => r !== def)
  const set = (next: LogCleanupPolicy) => setDraft(next)
  const setRule = (rule: LogCleanupRule, patch: Partial<LogCleanupRule>) =>
    set({ ...p, rules: p.rules.map((r) => (r === rule ? { ...r, ...patch } : r)) })
  const move = (rule: LogCleanupRule, by: number) => {
    const list = [...tagged]
    const i = list.indexOf(rule)
    const j = i + by
    if (j < 0 || j >= list.length) return
    ;[list[i], list[j]] = [list[j]!, list[i]!]
    set({ ...p, rules: [...list, ...(def ? [def] : [])] })
  }
  const add = () => set({
    ...p,
    rules: [...tagged, {
      ...(def ?? saved.rules[0]!), tag: '', mode: 'disk', needsApproval: false, id: `new-${Date.now()}`,
    }, ...(def ? [def] : [])],
  })

  const commit = () => void run(
    async () => {
      const body: LogCleanupPolicy = {
        paths: p.paths,
        protect: p.protect,
        rules: [...tagged, ...(def ? [def] : [])].map((r, i) => ({ ...r, id: undefined, position: i })),
      }
      await save.mutateAsync(body)
      setDraft(null)
    },
    { failure: 'Log cleanup not saved', success: ['Log cleanup saved', 'Servers follow the new rules from their next check-in.'] },
  )

  return (
    <Stack gap="sm">
      {canEdit && changed && (
        <Alert color="amber" p="xs">
          <Group justify="space-between" gap="sm">
            <Text size={FS.body}>Unsaved changes to how logs are cleaned.</Text>
            <Group gap={SP.cozy}>
              <Button size="xs" variant="subtle" color="slate" onClick={() => setDraft(null)}>Discard</Button>
              <Button size="xs" loading={save.isPending} onClick={commit}>Save</Button>
            </Group>
          </Group>
        </Alert>
      )}

      {def && (
        <SectionCard title="Every other server" icon={IconFileText}
          description="Every server no tag rule below names. New servers get this. It also sets what a Clean pressed by hand keeps.">
          <RuleEditor rule={def} canEdit={canEdit} onChange={(patch) => setRule(def, patch)} />
        </SectionCard>
      )}

      <SectionCard title="By tag" description="The first rule naming one of a server's tags wins."
        action={canEdit ? <Button size="compact-sm" variant="default" leftSection={<IconPlus size={14} />} onClick={add}>Add rule</Button> : undefined}>
        {tagged.length === 0 ? (
          <Text size={FS.body} c="dimmed">No tag rules. Every server follows the rule above.</Text>
        ) : (
          <Stack gap="lg">
            {tagged.map((r, i) => (
              <Stack key={r.id ?? r.tag} gap="xs">
                <Group gap="sm" align="flex-end" wrap="wrap">
                  <TextInput label="Servers tagged" placeholder="production" value={r.tag} disabled={!canEdit}
                    onChange={(e) => setRule(r, { tag: e.currentTarget.value.trim() })} w={200} />
                  {canEdit && (
                    <Group gap={SP.tight}>
                      <ActionIcon variant="subtle" color="slate" aria-label="Move up" disabled={i === 0} onClick={() => move(r, -1)}>
                        <IconArrowUp size={14} />
                      </ActionIcon>
                      <ActionIcon variant="subtle" color="slate" aria-label="Move down" disabled={i === tagged.length - 1} onClick={() => move(r, 1)}>
                        <IconArrowDown size={14} />
                      </ActionIcon>
                      <ActionIcon variant="subtle" color="rose" aria-label={`Remove the rule for ${r.tag || 'this tag'}`}
                        onClick={() => set({ ...p, rules: p.rules.filter((x) => x !== r) })}>
                        <IconTrash size={14} />
                      </ActionIcon>
                    </Group>
                  )}
                </Group>
                <RuleEditor rule={r} canEdit={canEdit} onChange={(patch) => setRule(r, patch)} />
              </Stack>
            ))}
          </Stack>
        )}
      </SectionCard>

      <SectionCard title="Also clean"
        description="Application logs outside the usual places, as absolute paths with * in them. Only files matching these are ever touched there, under the rotated-log age above.">
        <TagsInput aria-label="Also clean" value={p.paths} disabled={!canEdit} clearable splitChars={[',', ' ']}
          placeholder={p.paths.length === 0 ? '/srv/*/logs/*.log, D:\\apps\\shop\\logs\\*.log' : undefined}
          onChange={(paths) => set({ ...p, paths })} />
      </SectionCard>

      <SectionCard title="Never clean" icon={IconLock} iconColor="slate"
        description="The locked ones are built into the agent and cannot be removed here. Add your own, such as a backup tool's logs.">
        <Stack gap="xs">
          <Group gap={SP.tight} wrap="wrap">
            {BUILT_IN.map((b) => (
              <Badge key={b} size="md" variant="light" color="slate" leftSection={<IconLock size={10} />} tt="none" ff="monospace">
                {b}
              </Badge>
            ))}
          </Group>
          <TagsInput aria-label="Never clean" value={p.protect} disabled={!canEdit} clearable splitChars={[',', ' ']}
            placeholder={p.protect.length === 0 ? '/var/log/pgbackrest/*' : undefined}
            onChange={(protect) => set({ ...p, protect })} />
        </Stack>
      </SectionCard>

      <SectionCard title="Tell me">
        <Text size={FS.body} c="dimmed" lh={1.5}>
          A disk passing 90%, deleted files still holding a gigabyte or more, a cleanup that failed or was interrupted,
          and one waiting for approval: in the bell under Servers and Approvals. Every cleanup is in Activity, with the
          list of what it removed.
        </Text>
      </SectionCard>
    </Stack>
  )
}

function RuleEditor({ rule: r, canEdit, onChange }: {
  rule: LogCleanupRule
  canEdit: boolean
  onChange: (patch: Partial<LogCleanupRule>) => void
}) {
  return (
    <Stack gap="sm">
      <Radio.Group label="Clean automatically" value={r.mode} onChange={(m) => onChange({ mode: m as LogMode })}>
        <Stack gap={SP.cozy} mt={SP.snug}>
          {(Object.keys(LOG_MODE_LABEL) as LogMode[]).map((m) => (
            <Radio key={m} value={m} disabled={!canEdit} label={m === 'disk' ? `When a disk passes ${r.diskPercent}%` : LOG_MODE_LABEL[m]}
              description={m === r.mode ? modeHelp(r) : undefined} />
          ))}
        </Stack>
      </Radio.Group>
      {r.mode === 'disk' && (
        <NumberInput label="Disk full at" w={140} min={50} max={99} suffix="%" disabled={!canEdit}
          value={r.diskPercent} onChange={(v) => onChange({ diskPercent: Number(v) || 85 })} />
      )}
      {r.mode === 'daily' && (
        <Group gap="sm" align="flex-end" wrap="wrap">
          <Select label="Starts" w={100} allowDeselect={false} disabled={!canEdit} data={START_TIMES}
            value={String(r.windowStartMinute - (r.windowStartMinute % 30))}
            onChange={(v) => v && onChange({ windowStartMinute: Number(v) })} />
          <NumberInput label="Lasts (hours)" w={120} min={0.5} max={24} step={0.5} disabled={!canEdit}
            value={r.windowMinutes / 60} onChange={(v) => onChange({ windowMinutes: Math.round(Number(v || 0.5) * 60) })} />
        </Group>
      )}
      <Group gap="sm" align="flex-end" wrap="wrap">
        <NumberInput label="Rotated logs: keep (days)" w={190} min={0} max={3650} disabled={!canEdit}
          value={r.rotatedKeepDays} onChange={(v) => onChange({ rotatedKeepDays: Number(v) || 0 })} />
        <NumberInput label="Journal: keep (days)" w={170} min={7} max={3650} disabled={!canEdit}
          value={r.journalKeepDays} onChange={(v) => onChange({ journalKeepDays: Number(v) || 7 })}
          description="at least a week" />
        <NumberInput label="Journal: at most (MB)" w={170} min={256} max={1048576} step={256} disabled={!canEdit}
          value={r.journalMaxMb} onChange={(v) => onChange({ journalMaxMb: Number(v) || 256 })} />
      </Group>
      <Group gap="sm" align="flex-end" wrap="wrap">
        <NumberInput label="Live logs: empty automatically above (MB)" w={280} min={0} max={1048576} step={100} disabled={!canEdit}
          value={r.activeOverMb} onChange={(v) => onChange({ activeOverMb: Number(v) || 0 })}
          description={r.activeOverMb === 0 ? 'never; only when picked by hand' : undefined} />
        <NumberInput label="and keep the last (MB)" w={180} min={0} max={256} disabled={!canEdit}
          value={r.activeKeepMb} onChange={(v) => onChange({ activeKeepMb: Number(v) || 0 })} />
      </Group>
      <Switch size="sm" disabled={!canEdit} checked={r.needsApproval}
        onChange={(e) => onChange({ needsApproval: e.currentTarget.checked })}
        label="Cleanups asked for by hand need a second admin"
        description="Automatic cleanups do not: the rule is their approval." />
    </Stack>
  )
}

