import { useState } from 'react'
import {
  ActionIcon, Alert, Button, Group, NumberInput, Select, Skeleton, Stack, Switch, TagsInput, Text, TextInput,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconArrowDown, IconArrowUp, IconPackage, IconPlus, IconTrash } from '@tabler/icons-react'
import { SectionCard } from '~/components/page'
import { hhmm, MODE_LABEL } from '~/components/server/UpdatesTab'
import { isConfigured, type PackageMode, type PackagePolicy, type PackageReboot, type PackageRule } from '~/lib/live'
import { run } from '~/lib/notify'
import { packagePolicyQuery, useSavePackagePolicy } from '~/lib/packageQueries'
import { FS, SP } from '~/theme'

const DAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat']

const MODE_HELP: Record<PackageMode, string> = {
  off: "Argus doesn't look for updates.",
  notify: 'Look every 6 hours and show what is waiting; nothing is installed unless someone presses Update.',
  security: 'Install security updates in the window; everything else waits for someone to press Update.',
  all: 'Install every update in the window.',
}

const START_TIMES = Array.from({ length: 48 }, (_, i) => ({ value: String(i * 30), label: hhmm(i * 30) }))

/**
 * Settings › Updates: what Argus does about operating system updates, per
 * server tag. A server takes the first rule naming one of its tags, else
 * the one for every other server. Times are each server's own clock.
 */
export function PackageUpdateSettings({ canEdit }: { canEdit: boolean }) {
  const { data: saved, isLoading } = useQuery(packagePolicyQuery())
  const [draft, setDraft] = useState<PackagePolicy | null>(null)
  const save = useSavePackagePolicy()

  if (!isConfigured()) {
    return (
      <Alert color="slate">
        <Text size={FS.body}>Operating system updates are set on a control plane. Connect this console to Argus to set them.</Text>
      </Alert>
    )
  }
  if (isLoading || !saved) return <Skeleton h={320} />

  const p = draft ?? saved
  const changed = draft !== null && JSON.stringify(draft) !== JSON.stringify(saved)
  // The rule for every other server is the one saved with no tag; known by
  // its id, so a tag rule cleared while editing is not taken for it.
  const defId = saved.rules.find((r) => r.tag === '')?.id
  const def = p.rules.find((r) => r.id === defId)
  const tagged = p.rules.filter((r) => r !== def)
  const set = (next: PackagePolicy) => setDraft(next)
  const setRule = (rule: PackageRule, patch: Partial<PackageRule>) =>
    set({ ...p, rules: p.rules.map((r) => (r === rule ? { ...r, ...patch } : r)) })
  const move = (rule: PackageRule, by: number) => {
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
      position: tagged.length, tag: '', mode: 'security', windowDays: [0], windowStartMinute: 120, windowMinutes: 120,
      reboot: 'never', needsApproval: false, id: `new-${Date.now()}`,
    } as PackageRule, ...(def ? [def] : [])],
  })

  const commit = () => void run(
    async () => {
      // The draft's new rules carry a temporary id and an empty tag until
      // named; the server says what is missing.
      const body: PackagePolicy = {
        holds: p.holds,
        rules: [...tagged, ...(def ? [def] : [])].map((r, i) => ({ ...r, id: undefined, position: i })),
      }
      await save.mutateAsync(body)
      setDraft(null)
    },
    { failure: 'Updates not saved', success: ['Updates saved', 'Servers follow the new rules from their next check-in.'] },
  )

  return (
    <Stack gap="sm">
      {canEdit && changed && (
        <Alert color="amber" p="xs">
          <Group justify="space-between" gap="sm">
            <Text size={FS.body}>Unsaved changes to how servers are updated.</Text>
            <Group gap={SP.cozy}>
              <Button size="xs" variant="subtle" color="slate" onClick={() => setDraft(null)}>Discard</Button>
              <Button size="xs" loading={save.isPending} onClick={commit}>Save</Button>
            </Group>
          </Group>
        </Alert>
      )}

      {def && (
        <SectionCard title="Every other server" icon={IconPackage}
          description="Every Linux server no tag rule below names. New servers get this.">
          <RuleEditor rule={def} canEdit={canEdit} onChange={(patch) => setRule(def, patch)} />
        </SectionCard>
      )}

      <SectionCard title="By tag" description="The first rule naming one of a server's tags wins. Times are each server's own clock."
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

      <SectionCard title="Never update automatically"
        description="Packages whose upgrade restarts a database or every container. They still show as waiting, and are installed only when picked by hand on a server.">
        <TagsInput aria-label="Held packages" value={p.holds} disabled={!canEdit} clearable
          placeholder={p.holds.length === 0 ? "postgresql*, docker-ce*" : undefined} splitChars={[',', ' ']}
          onChange={(holds) => set({ ...p, holds })} />
      </SectionCard>

      <SectionCard title="Tell me">
        <Text size={FS.body} c="dimmed" lh={1.5}>
          New security updates, a run that failed or was interrupted, a server waiting for a reboot, and a run waiting
          for approval: in the bell under Servers and Approvals, like other server alerts. Every run is in Activity.
        </Text>
      </SectionCard>
    </Stack>
  )
}

function RuleEditor({ rule: r, canEdit, onChange }: {
  rule: PackageRule
  canEdit: boolean
  onChange: (patch: Partial<PackageRule>) => void
}) {
  const auto = r.mode === 'security' || r.mode === 'all'
  return (
    <Stack gap="xs">
      <Group gap="sm" align="flex-end" wrap="wrap">
        <Select label="Automatic updates" allowDeselect={false} disabled={!canEdit} value={r.mode} w={220}
          data={(Object.keys(MODE_LABEL) as PackageMode[]).map((m) => ({ value: m, label: MODE_LABEL[m] }))}
          onChange={(m) => m && onChange({ mode: m as PackageMode, ...(m !== 'off' && m !== 'notify' && r.windowDays.length === 0 ? { windowDays: [0] } : {}) })} />
        <Text size={FS.meta} c="dimmed" pb={8} style={{ flex: '1 1 260px' }}>{MODE_HELP[r.mode]}</Text>
      </Group>
      {auto && (
        <Group gap="sm" align="flex-end" wrap="wrap">
          <Stack gap={SP.hair}>
            <Text size={FS.meta} fw={500}>Window days</Text>
            <Group gap={SP.tight} role="group" aria-label="Window days">
              {DAYS.map((d, i) => {
                const on = r.windowDays.includes(i)
                return (
                  <Button key={d} size="compact-xs" variant={on ? 'filled' : 'default'} disabled={!canEdit} aria-pressed={on}
                    onClick={() => onChange({
                      windowDays: on ? r.windowDays.filter((x) => x !== i) : [...r.windowDays, i].sort((a, b) => a - b),
                    })}>
                    {d}
                  </Button>
                )
              })}
            </Group>
          </Stack>
          <Select label="Starts" w={100} allowDeselect={false} disabled={!canEdit} data={START_TIMES}
            value={String(r.windowStartMinute - (r.windowStartMinute % 30))}
            onChange={(v) => v && onChange({ windowStartMinute: Number(v) })} />
          <NumberInput label="Lasts (hours)" w={120} min={0.5} max={24} step={0.5} disabled={!canEdit}
            value={r.windowMinutes / 60} onChange={(v) => onChange({ windowMinutes: Math.round(Number(v || 0.5) * 60) })} />
          <Select label="Reboot" w={200} allowDeselect={false} disabled={!canEdit} value={r.reboot}
            data={[{ value: 'never', label: 'Never; tell me' }, { value: 'if_needed', label: 'If needed, in the window' }]}
            onChange={(v) => v && onChange({ reboot: v as PackageReboot })} />
        </Group>
      )}
      {r.mode !== 'off' && (
        <Switch size="sm" disabled={!canEdit} checked={r.needsApproval}
          onChange={(e) => onChange({ needsApproval: e.currentTarget.checked })}
          label="Runs asked for by hand need a second admin"
          description="Automatic runs in the window do not: the rule is their approval." />
      )}
    </Stack>
  )
}
