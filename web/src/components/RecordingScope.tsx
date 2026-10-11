import { Alert, Radio, Stack, Text } from '@mantine/core'
import { IconAlertTriangle, IconVideo } from '@tabler/icons-react'
import { SectionCard } from '~/components/page'
import { Field } from '~/components/primitives'
import { run } from '~/lib/notify'
import { useSetRecordingScope } from '~/lib/queries'
import { FS } from '~/theme'
import type { Asset, RecordingScope } from '~/types/domain'

const OPTIONS: { value: RecordingScope; label: string; description: string }[] = [
  {
    value: 'every_login',
    label: 'Record logins through Argus and direct logins',
    description: 'Someone who logs in with their own key, skipping Argus, is recorded too.',
  },
  {
    value: 'argus_only',
    label: 'Record logins through Argus only',
    description:
      'Someone who skips Argus gets in unrecorded. The audit log still says who, from where and when.',
  },
]

/**
 * What the agent's last check-in says about this setting.
 *
 * The control shows the saved value; this says whether the host obeys it yet.
 * Saved and enforced are different facts, and the gap between them is where an
 * operator would otherwise assume a change took effect.
 */
export function scopeConfirmation(asset: Asset): { applied: boolean; text: string } {
  const set = asset.recordingScope ?? 'every_login'
  const agent = asset.agentRecordingScope ?? null
  if (agent === null) {
    return {
      applied: false,
      text:
        'Not confirmed by the agent yet. A current agent confirms at its next check-in, ' +
        'within a minute; an older agent never confirms and records every login.',
    }
  }
  if (agent !== set) {
    return {
      applied: false,
      text: `The agent is still applying "${label(agent)}". It picks up the change at its next check-in.`,
    }
  }
  return { applied: true, text: 'Applied by the agent at its last check-in.' }
}

function label(s: RecordingScope): string {
  return OPTIONS.find((o) => o.value === s)?.label ?? s
}

export function RecordingScopeCard({ asset, canEdit }: { asset: Asset; canEdit: boolean }) {
  const set = useSetRecordingScope()
  const value = asset.recordingScope ?? 'every_login'
  const confirm = scopeConfirmation(asset)

  // Controlled by the server's value, not local state: a refused change leaves
  // the control where the server says it is, rather than where it was clicked.
  const onChange = (next: string) => {
    if (next === value) return
    const scope = next as RecordingScope
    void run(() => set.mutateAsync({ id: asset.id, scope }), {
      failure: 'Recording setting not changed',
      success:
        scope === 'argus_only'
          ? [
              'Recording logins through Argus only',
              `Direct logins to ${asset.hostname} will be let through unrecorded and reported as bypasses once the agent checks in.`,
            ]
          : [
              'Recording logins through Argus and direct logins',
              `Direct logins to ${asset.hostname} will be recorded again once the agent checks in.`,
            ],
    })
  }

  return (
    <SectionCard
      title="Which logins are recorded"
      icon={IconVideo}
      description="Logins through Argus are always recorded. This decides what happens when someone logs in to the server directly, skipping Argus."
    >
      <Stack gap="sm">
        <Radio.Group
          aria-label="Which logins the agent records"
          value={value}
          onChange={onChange}
        >
          <Stack gap="xs">
            {OPTIONS.map((o) => (
              <Radio
                key={o.value}
                value={o.value}
                label={o.label}
                description={o.description}
                disabled={!canEdit || set.isPending}
              />
            ))}
          </Stack>
        </Radio.Group>
        {value === 'argus_only' ? (
          <Alert color="amber" variant="light" icon={<IconAlertTriangle size={16} />} p="xs">
            <Text size={FS.micro} lh={1.5}>
              A direct login is let through with no replay. Each one is still written to the
              audit log as a bypass, with who, from where and when.
            </Text>
          </Alert>
        ) : (
          <Text size={FS.micro} c="dimmed" lh={1.5}>
            Direct logins are recorded, and flagged in the audit log as skipping Argus.
          </Text>
        )}
        <Field label="On the server">
          <Text size="xs" c={confirm.applied ? undefined : 'amber'}>
            {confirm.text}
          </Text>
        </Field>
        {!canEdit && (
          <Text size={FS.micro} c="dimmed">
            Only an administrator or owner can change this.
          </Text>
        )}
      </Stack>
    </SectionCard>
  )
}
