import { Avatar, Badge, Box, Group, Paper, ScrollArea, SimpleGrid, Table, Text, Tooltip } from '@mantine/core'
import { IconShieldCheck, IconTrash } from '@tabler/icons-react'
import { FS } from '~/theme'
import {
  BIG_CHANGE, LABEL_NAME, impactTiles, impactWords, initials, shortName,
  type ChangeImpact, type ImpactPreview,
} from '~/lib/changeApproval'
import type { DBLabel } from '~/types/domain'

/*
The pieces every view of a change request shares: the environment badge, the
"changes need approval" badge, the impact as tiles, the first affected rows,
and the people who can decide.
*/

const LABEL_COLOR: Record<DBLabel, string> = { production: 'rose', staging: 'amber', development: 'slate' }

/** The environment a database is in. */
export function EnvBadge({ label, size = 'sm' }: { label: DBLabel; size?: 'xs' | 'sm' }) {
  return (
    <Badge variant="light" color={LABEL_COLOR[label] ?? 'slate'} radius="sm" size={size} tt="none">
      {LABEL_NAME[label] ?? label}
    </Badge>
  )
}

/** Shown wherever a database is named whose changes wait for approval. */
export function ApprovalBadge({ size = 'sm' }: { size?: 'xs' | 'sm' }) {
  return (
    <Tooltip label="A statement that changes it runs once someone else approves it. Reading never waits." withArrow>
      <Badge variant="light" color="amber" radius="sm" size={size} tt="none"
        leftSection={<IconShieldCheck size={12} aria-hidden />}>
        changes need approval
      </Badge>
    </Tooltip>
  )
}

const WHAT = { updated: 'updated', deleted: 'deleted', inserted: 'inserted' } as const
const TONE = { updated: 'var(--mantine-color-azure-6)', deleted: 'var(--mantine-color-rose-7)', inserted: 'var(--mantine-color-teal-7)' } as const

/** The rows a change touches, per kind, as tiles. */
export function ImpactTiles({ impact }: { impact: ChangeImpact }) {
  const tiles = impactTiles(impact)
  if (!tiles.length) {
    if (impact.rows === undefined) return null
    return <Text size={FS.body} fw={600}>{impactWords(impact)}</Text>
  }
  return (
    <SimpleGrid cols={{ base: 1, xs: Math.min(tiles.length, 3) }} spacing="xs">
      {tiles.map((t) => (
        <Paper key={t.what} withBorder radius="md" px="md" py="xs">
          <Text fw={700} size="xl" style={{ color: TONE[t.what], fontVariantNumeric: 'tabular-nums' }}>{t.n.toLocaleString()}</Text>
          <Text size={FS.meta} c="dimmed">{t.n === 1 ? 'row' : 'rows'} {WHAT[t.what]} in {t.tables.join(', ')}</Text>
        </Paper>
      ))}
    </SimpleGrid>
  )
}

/** "18 rows · 2 tables", in red when it is a big change. */
export function ImpactText({ impact, fallback = '' }: { impact: ChangeImpact; fallback?: string }) {
  const w = impactWords(impact)
  if (!w) return <Text size={FS.body} c="dimmed">{fallback || 'not counted'}</Text>
  const big = (impact.rows ?? 0) > BIG_CHANGE
  return <Text size={FS.body} fw={650} c={big ? 'rose.7' : undefined} style={{ fontVariantNumeric: 'tabular-nums' }}>{w}</Text>
}

/** The first rows each statement changes, from the dry run. */
export function PreviewRows({ preview }: { preview: ImpactPreview[] }) {
  if (!preview.length) return null
  return (
    <Box>
      {preview.map((p, n) => (
        <ScrollArea key={n} type="auto" mb="xs">
          <Table withTableBorder striped={false} verticalSpacing={4} fz={FS.meta} ff="monospace" miw={360}>
            <Table.Thead>
              <Table.Tr>
                <Table.Th ff="var(--mantine-font-family)">change</Table.Th>
                {p.columns.map((c, i) => <Table.Th key={i}>{c}</Table.Th>)}
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {p.rows.map((r, i) => (
                <Table.Tr key={i}>
                  <Table.Td>
                    <Badge size="xs" variant="light" radius="sm" tt="none" color={p.change === 'delete' ? 'rose' : p.change === 'insert' ? 'teal' : 'azure'}
                      leftSection={p.change === 'delete' ? <IconTrash size={10} aria-hidden /> : undefined}>
                      {p.change}
                    </Badge>
                  </Table.Td>
                  {r.map((v, j) => (
                    <Table.Td key={j} style={{ textDecoration: p.change === 'delete' ? 'line-through' : undefined, whiteSpace: 'nowrap' }}>
                      {v === null ? <Text span c="dimmed" size={FS.meta}>NULL</Text> : v}
                    </Table.Td>
                  ))}
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </ScrollArea>
      ))}
      <Text size={FS.micro} c="dimmed">
        Updated and inserted rows show their new values; deleted rows show what they held. Deleted rows go to the trash.
      </Text>
    </Box>
  )
}

/** People, as initials with their short names after. */
export function People({ emails, max = 5 }: { emails: string[]; max?: number }) {
  if (!emails.length) return null
  return (
    <Group gap={6} wrap="nowrap">
      <Avatar.Group spacing={6}>
        {emails.slice(0, max).map((e) => (
          <Tooltip key={e} label={e} withArrow><Avatar size={22} radius="xl" color="azure">{initials(e)}</Avatar></Tooltip>
        ))}
        {emails.length > max && <Avatar size={22} radius="xl">+{emails.length - max}</Avatar>}
      </Avatar.Group>
      <Text size={FS.meta} c="dimmed" lineClamp={1}>{emails.map(shortName).join(', ')}</Text>
    </Group>
  )
}
