import { useState } from 'react'
import { ActionIcon, Alert, Button, Group, NumberInput, Skeleton, Stack, Switch, Text, TextInput } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconArrowDown, IconArrowUp, IconDatabase, IconPlus, IconTrash } from '@tabler/icons-react'
import { SectionCard } from '~/components/page'
import type { DiskGrowPolicy, DiskGrowRule } from '~/lib/diskApi'
import { diskPolicyQuery, useSaveDiskPolicy } from '~/lib/diskQueries'
import { isConfigured } from '~/lib/live'
import { run } from '~/lib/notify'
import { FS, SP } from '~/theme'

/**
 * Settings › Disk growing: per server tag, whether a grow asked for by hand
 * waits for a second admin, and whether Argus grows a filesystem on its own
 * when it fills. A server takes the first rule naming one of its tags, else
 * the one for every other server. Out of the box nothing grows on its own and
 * production waits for a second admin.
 */
export function DiskGrowSettings({ canEdit }: { canEdit: boolean }) {
  const { data: saved, isLoading } = useQuery(diskPolicyQuery())
  const [draft, setDraft] = useState<DiskGrowPolicy | null>(null)
  const save = useSaveDiskPolicy()

  if (!isConfigured()) {
    return (
      <Alert color="slate">
        <Text size={FS.body}>Disk growing is set on a control plane. Connect this console to Argus to set it.</Text>
      </Alert>
    )
  }
  if (isLoading || !saved) return <Skeleton h={240} />

  const p = draft ?? saved
  const changed = draft !== null && JSON.stringify(draft) !== JSON.stringify(saved)
  const def = p.rules.find((r) => r.tag === '' && (saved.rules.find((s) => s.tag === '')?.id === r.id))
  const tagged = p.rules.filter((r) => r !== def)
  const setRule = (rule: DiskGrowRule, patch: Partial<DiskGrowRule>) =>
    setDraft({ rules: p.rules.map((r) => (r === rule ? { ...r, ...patch } : r)) })
  const move = (rule: DiskGrowRule, by: number) => {
    const list = [...tagged]
    const i = list.indexOf(rule)
    const j = i + by
    if (j < 0 || j >= list.length) return
    ;[list[i], list[j]] = [list[j]!, list[i]!]
    setDraft({ rules: [...list, ...(def ? [def] : [])] })
  }
  const add = () => setDraft({
    rules: [...tagged, { id: `new-${Date.now()}`, position: 0, tag: '', needsApproval: true, autoPercent: 0 },
      ...(def ? [def] : [])],
  })
  const commit = () => void run(
    async () => {
      await save.mutateAsync({
        rules: [...tagged, ...(def ? [def] : [])].map((r, i) => ({ ...r, id: undefined, position: i })),
      })
      setDraft(null)
    },
    { failure: 'Disk growing not saved', success: ['Disk growing saved', 'Servers follow the new rules from their next check-in.'] },
  )

  return (
    <Stack gap="sm">
      {canEdit && changed && (
        <Alert color="amber" p="xs">
          <Group justify="space-between" gap="sm">
            <Text size={FS.body}>Unsaved changes to how disks grow.</Text>
            <Group gap={SP.cozy}>
              <Button size="xs" variant="subtle" color="slate" onClick={() => setDraft(null)}>Discard</Button>
              <Button size="xs" loading={save.isPending} onClick={commit}>Save</Button>
            </Group>
          </Group>
        </Alert>
      )}

      {def && (
        <SectionCard title="Every other server" icon={IconDatabase}
          description="Every server no tag rule below names. New servers get this.">
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
                        onClick={() => setDraft({ rules: p.rules.filter((x) => x !== r) })}>
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

      <SectionCard title="Tell me">
        <Text size={FS.body} c="dimmed" lh={1.5}>
          A grow waiting for approval, and one that failed or was interrupted: in the bell under Servers and Approvals.
          Every grow is in Activity, with each command it ran and the partition table before.
        </Text>
      </SectionCard>
    </Stack>
  )
}

function RuleEditor({ rule: r, canEdit, onChange }: {
  rule: DiskGrowRule
  canEdit: boolean
  onChange: (patch: Partial<DiskGrowRule>) => void
}) {
  const auto = r.autoPercent > 0
  return (
    <Stack gap="sm">
      <Switch size="sm" disabled={!canEdit} checked={r.needsApproval}
        onChange={(e) => onChange({ needsApproval: e.currentTarget.checked })}
        label="A grow asked for by hand needs a second admin"
        description="Growing on its own does not: the rule is its approval." />
      <Switch size="sm" disabled={!canEdit} checked={auto}
        onChange={(e) => onChange({ autoPercent: e.currentTarget.checked ? 90 : 0 })}
        label={auto ? `Grow on its own when a filesystem passes ${r.autoPercent}%` : 'Grow on its own when a filesystem fills'}
        description="Into room the server already has, at most once a day. It never makes a disk or a cloud volume bigger." />
      {auto && (
        <NumberInput label="Filesystem full at" w={160} min={50} max={99} suffix="%" disabled={!canEdit}
          value={r.autoPercent} onChange={(v) => onChange({ autoPercent: Math.min(99, Math.max(50, Number(v) || 90)) })} />
      )}
    </Stack>
  )
}
