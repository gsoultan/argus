import { ActionIcon, Box, Button, Card, Group, NumberInput, Select, Stack, Text } from '@mantine/core'
import { IconPlus, IconX } from '@tabler/icons-react'
import { SIGNALS, ruleProblem, type ScaleRule } from '~/lib/cloudScale'
import { FS, SP } from '~/theme'

/** A list of rules, joined by "or" (adding) or "and" (taking away). */
export function RuleList({ title, join, rules, onChange, fresh }: {
  title: string; join: 'or' | 'and'; rules: ScaleRule[]; onChange: (r: ScaleRule[]) => void; fresh: ScaleRule
}) {
  const set = (i: number, r: Partial<ScaleRule>) => onChange(rules.map((x, j) => (j === i ? { ...x, ...r } : x)))
  return (
    <Box>
      <Text fw={600} size={FS.body} mb={SP.cozy}>{title}</Text>
      <Card withBorder padding="sm">
        <Stack gap={SP.cozy}>
          {rules.length === 0 && <Text size={FS.meta} c="dimmed">Never, by itself.</Text>}
          {rules.map((r, i) => (
            <Box key={i}>
              {i > 0 && <Text size={FS.micro} c="dimmed" mb={4}>{join}</Text>}
              <Group gap="xs" wrap="wrap" align="center">
                <Select aria-label="Over" w={150} data={[{ value: 'average', label: 'Group average' }, { value: 'any', label: "Any member's" }]}
                  value={r.over} onChange={(v) => v && set(i, { over: v as ScaleRule['over'] })} allowDeselect={false} />
                <Select aria-label="Signal" w={170} data={SIGNALS.map((s) => ({ value: s.value, label: s.label }))} value={r.signal}
                  onChange={(v) => v && set(i, { signal: v as ScaleRule['signal'] })} allowDeselect={false} />
                <Select aria-label="Above or below" w={100} data={['above', 'below']} value={r.op} onChange={(v) => v && set(i, { op: v as ScaleRule['op'] })} allowDeselect={false} />
                <NumberInput aria-label="Value" w={90} min={0} value={r.value} onChange={(v) => set(i, { value: Number(v) || 0 })}
                  suffix={SIGNALS.find((s) => s.value === r.signal)?.unit ? ' %' : undefined} decimalScale={2} />
                <Text size={FS.meta}>for</Text>
                <NumberInput aria-label="Minutes" w={90} min={5} max={120} step={5} value={r.minutes} onChange={(v) => set(i, { minutes: Number(v) || 0 })} suffix=" min" />
                <ActionIcon variant="subtle" color="slate" aria-label="Remove this rule" onClick={() => onChange(rules.filter((_, j) => j !== i))}><IconX size={14} /></ActionIcon>
              </Group>
              {ruleProblem(r) && <Text size={FS.micro} c="rose" mt={4}>{ruleProblem(r)}</Text>}
            </Box>
          ))}
          {rules.length < 4 && (
            <Button size="xs" variant="subtle" leftSection={<IconPlus size={14} />} w="fit-content" onClick={() => onChange([...rules, fresh])}>
              Add a signal: processor, memory, swap, load or steal
            </Button>
          )}
        </Stack>
      </Card>
    </Box>
  )
}
