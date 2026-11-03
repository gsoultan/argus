import { useState } from 'react'
import { ActionIcon, Alert, Badge, Box, Code, CopyButton, Group, ScrollArea, Stack, Text, Tooltip } from '@mantine/core'
import { IconCheck, IconCopy } from '@tabler/icons-react'
import { FS } from '~/theme'
import type { Plan, PlanNode } from '~/lib/explainPlan'

/*
The Plan view under the editor: what EXPLAIN said, problems first, then each
step with its share of the time. Raw is EXPLAIN's own text, for a ticket.
*/

export interface PlanRun {
  mode: 'plan' | 'analyze' | 'buffers'
  plan: Plan | null
  error?: string
  /** ANALYZE ran inside a transaction that was rolled back. */
  rolledBack?: boolean
  running: boolean
}

function ms(n: number | undefined): string {
  if (n === undefined) return ''
  if (n >= 1000) return `${(n / 1000).toFixed(2)} s`
  if (n >= 10) return `${Math.round(n).toLocaleString()} ms`
  return `${n.toFixed(2)} ms`
}

function num(n: number): string {
  return n >= 1e6 ? `${(n / 1e6).toFixed(1)}M` : n.toLocaleString()
}

export function planSummary(run: PlanRun): string {
  const p = run.plan
  if (!p) return ''
  const parts: string[] = []
  if (p.analyzed && p.executionMs !== undefined) parts.push(`Total ${ms(p.executionMs)}`)
  else if (p.root) parts.push(`Estimated cost ${num(Math.round(p.root.cost))}`)
  if (p.planningMs !== undefined) parts.push(`planning ${ms(p.planningMs)}`)
  if (run.rolledBack) parts.push('rolled back')
  return parts.join(' · ')
}

export function ExplainView({ run, raw, narrow }: { run: PlanRun; raw: boolean; narrow: boolean }) {
  const [open, setOpen] = useState<PlanNode | null>(null)
  if (run.error) {
    return (
      <Box p="md">
        <Alert color="rose" variant="light" title="No plan">{run.error}</Alert>
      </Box>
    )
  }
  const p = run.plan
  if (!p) return <Text p="md" size={FS.meta} c="dimmed">{run.running ? 'Asking Postgres for the plan…' : 'Use Explain to see how Postgres runs this statement.'}</Text>
  if (raw) {
    return (
      <ScrollArea style={{ flex: 1 }} p="md">
        <Group justify="flex-end" mb={4}>
          <CopyButton value={p.text}>
            {({ copied, copy }) => (
              <Tooltip label={copied ? 'Copied' : 'Copy the plan'}>
                <ActionIcon variant="subtle" onClick={copy} aria-label="Copy the plan">{copied ? <IconCheck size={14} /> : <IconCopy size={14} />}</ActionIcon>
              </Tooltip>
            )}
          </CopyButton>
        </Group>
        <Code block style={{ fontSize: 12, whiteSpace: 'pre' }}>{p.text}</Code>
      </ScrollArea>
    )
  }
  const total = p.executionMs ?? p.root?.totalMs ?? 0
  const maxCost = p.root?.cost || 1
  return (
    <ScrollArea style={{ flex: 1 }} type="auto">
      {p.findings.length > 0 && (
        <Stack gap={8} p={narrow ? 'sm' : 'md'} pb={4}>
          {p.findings.map((f, i) => (
            <Box key={i} p="sm" style={{ borderRadius: 8, background: `var(--mantine-color-${f.severity === 'high' ? 'rose' : 'amber'}-light)` }}>
              <Text size={FS.meta} fw={650} c={f.severity === 'high' ? 'rose' : 'amber'}>{f.title}</Text>
              <Text size={FS.meta} mt={2}>{f.detail}</Text>
              {f.fix && (
                <Group gap={6} mt={4} wrap="nowrap">
                  <Code style={{ fontSize: 11 }}>{f.fix}</Code>
                  <CopyButton value={f.fix}>
                    {({ copied, copy }) => (
                      <Text component="button" type="button" size={FS.micro} c="var(--color-brand)" onClick={copy}
                        style={{ background: 'none', border: 0, cursor: 'pointer', padding: 0 }}>{copied ? 'Copied' : 'Copy'}</Text>
                    )}
                  </CopyButton>
                </Group>
              )}
            </Box>
          ))}
        </Stack>
      )}
      {p.findings.length === 0 && p.nodes.length > 0 && (
        <Text size={FS.meta} c="dimmed" px="md" pt="sm">Nothing in this plan stands out.{p.analyzed ? '' : ' Explain analyze runs it and shows where the time goes.'}</Text>
      )}
      {!narrow && (
        <Group gap="md" px="md" py={6} mt="xs" wrap="nowrap" style={{ background: 'var(--color-raised)', borderBottom: '1px solid var(--color-line)' }}>
          <Text size={FS.micro} c="dimmed" style={{ flex: 1 }}>Step</Text>
          <Text size={FS.micro} c="dimmed" w={160}>{p.analyzed ? 'Share of time' : 'Share of cost'}</Text>
          <Text size={FS.micro} c="dimmed" w={150} ta="right">{p.analyzed ? 'rows actual / est.' : 'rows est.'}</Text>
          {p.analyzed && <Text size={FS.micro} c="dimmed" w={80} ta="right">time</Text>}
        </Group>
      )}
      {p.nodes.map((n, i) => {
        const share = p.analyzed ? (total > 0 ? (n.selfMs ?? 0) / total : 0) : n.cost / maxCost
        const hot = p.analyzed && share >= 0.5
        const flag = p.findings.find((f) => f.node === n)
        const bar = (
          <Box w={narrow ? '100%' : 160} h={8} style={{ borderRadius: 4, background: 'var(--color-raised)', overflow: 'hidden', flexShrink: 0 }}>
            <Box h="100%" w={`${Math.max(2, Math.min(100, share * 100))}%`}
              style={{ background: hot ? 'var(--mantine-color-rose-6)' : 'var(--color-brand)', opacity: n.neverExecuted ? 0.3 : 1 }} />
          </Box>
        )
        const rows = p.analyzed
          ? `${n.neverExecuted ? 'never ran' : num((n.rows ?? 0) * (n.loops || 1))} / ${num(n.estRows * Math.max(1, n.loops ?? 1))}`
          : num(n.estRows)
        return (
          <Box key={i} px={narrow ? 'sm' : 'md'} py={narrow ? 8 : 6} onClick={() => setOpen(open === n ? null : n)}
            style={{ borderBottom: '1px solid var(--color-line)', cursor: 'pointer' }}>
            <Group gap={narrow ? 6 : 'md'} wrap={narrow ? 'wrap' : 'nowrap'}>
              <Group gap={6} wrap="nowrap" style={{ flex: 1, minWidth: 0, paddingLeft: n.depth * (narrow ? 10 : 20) }}>
                {n.depth > 0 && <Text size={FS.meta} c="dimmed">└</Text>}
                <Text size={FS.meta} fw={600} style={{ whiteSpace: 'nowrap' }}>{n.type}</Text>
                {n.target && <Text size={FS.micro} c="dimmed" ff="monospace" style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{n.target}</Text>}
                {flag && <Badge size="xs" radius="sm" variant="light" color={flag.severity === 'high' ? 'rose' : 'amber'} style={{ flexShrink: 0 }}>
                  {flag.severity === 'high' ? 'slowest' : 'look'}</Badge>}
              </Group>
              {narrow ? (
                <Text size={FS.micro} ff="monospace" c="dimmed">{p.analyzed ? ms(n.selfMs) : ''}</Text>
              ) : (
                <>
                  {bar}
                  <Text size={FS.micro} ff="monospace" c="dimmed" w={150} ta="right">{rows}</Text>
                  {p.analyzed && <Text size={FS.micro} ff="monospace" w={80} ta="right">{ms(n.selfMs)}</Text>}
                </>
              )}
              {narrow && bar}
            </Group>
            {open === n && (
              <Box mt={6} pl={n.depth * (narrow ? 10 : 20) + 14}>
                {narrow && <Text size={FS.micro} c="dimmed" ff="monospace">rows {rows}</Text>}
                <Text size={FS.micro} c="dimmed" ff="monospace">cost {n.cost.toFixed(2)}{n.loops && n.loops > 1 ? ` · ${n.loops.toLocaleString()} loops` : ''}</Text>
                {n.details.map((d, j) => <Text key={j} size={FS.micro} ff="monospace" style={{ wordBreak: 'break-word' }}>{d}</Text>)}
              </Box>
            )}
          </Box>
        )
      })}
    </ScrollArea>
  )
}
