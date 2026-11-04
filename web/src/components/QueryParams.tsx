import { ActionIcon, Group, Text, TextInput, Tooltip } from '@mantine/core'
import { IconBackspace } from '@tabler/icons-react'
import { FS, SP } from '~/theme'

/*
The :name parameters of the statement in the editor, one field each. A
value goes with the statement as a bind parameter, never into its text. An
empty field is NULL; Postgres works out each value's type from where the
statement uses it.
*/
export function QueryParams({ names, values, onChange, narrow }: {
  names: string[]
  values: Record<string, string | null>
  onChange: (v: Record<string, string | null>) => void
  narrow?: boolean
}) {
  if (names.length === 0) return null
  return (
    <Group gap="xs" px={narrow ? 'sm' : 'md'} py={SP.cozy} wrap="wrap" aria-label="Parameters" role="group"
      style={{ borderTop: '1px solid var(--color-line)', background: 'var(--color-raised)' }}>
      <Text size={FS.micro} c="dimmed" fw={600}>Parameters</Text>
      {names.map((n) => {
        const v = values[n] ?? null
        return (
          <TextInput key={n} size="xs" w={narrow ? '100%' : 200}
            leftSection={<Text size={FS.micro} ff="var(--font-mono)" c="dimmed">:{n}</Text>}
            leftSectionWidth={Math.min(120, 18 + n.length * 7)}
            aria-label={`Value for :${n}`}
            placeholder="NULL"
            value={v ?? ''}
            onChange={(e) => { const x = e.currentTarget.value; onChange({ ...values, [n]: x === '' ? null : x }) }}
            rightSection={v !== null && (
              <Tooltip label="Make it NULL" withinPortal>
                <ActionIcon size="xs" variant="subtle" aria-label={`Make :${n} NULL`} onClick={() => onChange({ ...values, [n]: null })}>
                  <IconBackspace size={12} />
                </ActionIcon>
              </Tooltip>
            )}
            styles={{ input: { fontFamily: 'var(--font-mono)' } }}
          />
        )
      })}
    </Group>
  )
}
