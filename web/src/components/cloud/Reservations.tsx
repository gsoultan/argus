import { Alert, Badge, Box, Group, Progress, SimpleGrid, Stack, Table, Text } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconCalendarDue, IconCoin, IconReceipt, IconShieldCheck } from '@tabler/icons-react'
import { DataTable, EmptyState, SectionCard } from '~/components/page'
import { Stat } from '~/components/primitives'
import { dollars } from '~/lib/cloudCosts'
import {
  HOURS_A_MONTH, cloudReservationsQuery, daysUntil, held, type Held, type HeldKind, type ReservationsView,
} from '~/lib/cloudReservations'
import { FS, INK } from '~/theme'
import type { CloudTab } from './tabs'

/*
 * What the accounts have paid for ahead, and how much of it is used:
 * Reserved Instances, Savings Plans and Capacity Reservations. Use and
 * coverage are Argus's estimate against the instances running at the last
 * read, at list price, and say so; a Capacity Reservation's use is AWS's own.
 */

const KIND: Record<HeldKind, { label: string; color: string }> = {
  ri: { label: 'Reserved Instance', color: 'azure' },
  sp: { label: 'Savings Plan', color: 'sky' },
  cr: { label: 'Capacity Reservation', color: 'slate' },
}

const day = (iso?: string) => (iso ? new Date(iso).toLocaleDateString('en-GB', { day: 'numeric', month: 'short', year: 'numeric' }) : '')

export function ReservationsPanel({ onTab }: { onTab?: (t: CloudTab) => void }) {
  const q = useQuery(cloudReservationsQuery())
  const v = q.data
  const rows = v ? held(v) : []
  const hourly = v?.coverage.reduce((s, c) => s + c.hourly, 0) ?? 0
  const byRi = v?.coverage.reduce((s, c) => s + c.byRi, 0) ?? 0
  const rest = v?.coverage.reduce((s, c) => s + c.rest, 0) ?? 0
  const commit = v?.savingsPlans.reduce((s, p) => s + p.use.commitment, 0) ?? 0
  const ending = rows.filter((r) => { const d = daysUntil(r.ends); return d !== null && d >= 0 && d <= 30 })
    .sort((a, b) => Date.parse(a.ends!) - Date.parse(b.ends!))
  const unusedCount = rows.filter((r) => r.unused > 0).length

  return (
    <Stack gap="lg" data-testid="reservations">
      {q.error && <Alert color="rose" variant="light">{(q.error as Error).message}</Alert>}
      {v && <ReadNotes v={v} />}
      <SimpleGrid cols={{ base: 1, xs: 2, md: 4 }} spacing="md">
        <Stat label="Covered by Reserved Instances" icon={IconShieldCheck} color="azure"
          value={v ? (hourly > 0 ? `${Math.round((byRi / hourly) * 100)}%` : '—') : undefined}
          sub={hourly > 0 ? 'of what runs now, at list price' : 'Nothing running to cover'} />
        <Stat label="Not reserved, a month" icon={IconCoin}
          value={v ? dollars(rest * HOURS_A_MONTH) : undefined}
          sub={commit > 0 ? `at list price; Savings Plans commit ${dollars(commit * HOURS_A_MONTH)} a month against it` : 'at list price, if it keeps running'} />
        <Stat label="Paid for, not used" icon={IconReceipt} color="rose" tone={v && v.unusedMonthly > 0 ? 'warn' : 'ok'}
          value={v ? dollars(v.unusedMonthly) : undefined}
          onClick={onTab && v && v.unusedMonthly > 0 ? () => onTab('risks') : undefined}
          sub={v ? (unusedCount ? `a month · ${unusedCount} reservation${unusedCount === 1 ? '' : 's'}` : 'Everything reserved is in use') : undefined} />
        <Stat label="Ending in 30 days" icon={IconCalendarDue} color="amber" tone={ending.length ? 'warn' : 'ok'}
          value={v ? String(ending.length) : undefined}
          sub={ending[0] ? `${ending[0].covers} · ${day(ending[0].ends)}` : 'Nothing ends this month'} />
      </SimpleGrid>

      <SectionCard title="Coverage" flush
        description="The running instances in each account and region, priced at list price, and how much a Reserved Instance covers. An estimate from the last read; AWS applies them by the second over the month.">
        <DataTable minWidth={640} loading={q.isPending} isEmpty={(v?.coverage.length ?? 0) === 0}
          empty={<EmptyState icon={IconShieldCheck} title="Nothing running" description="Coverage is worked out from running instances; none are running in the accounts read." />}
          columns={['Where', 'Reserved', 'Not reserved, a month']}>
          {v?.coverage.map((c) => {
            const pct = c.hourly > 0 ? Math.round((c.byRi / c.hourly) * 100) : 0
            return (
              <Table.Tr key={c.accountId + c.region}>
                <Table.Td>
                  <Text size={FS.body} fw={600}>{c.accountName}</Text>
                  <Text size={FS.micro} c="dimmed">{c.region} · {c.instances} running{c.priced < c.instances ? `, ${c.instances - c.priced} without a list price` : ''}</Text>
                </Table.Td>
                <Table.Td w="45%">
                  <Progress.Root size="md" aria-label={`${pct}% reserved in ${c.accountName} ${c.region}`}>
                    <Progress.Section value={pct} color="azure" />
                    <Progress.Section value={100 - pct} color="amber" />
                  </Progress.Root>
                  <Text size={FS.micro} c="dimmed" mt={4}>{pct}% reserved · {100 - pct}% Savings Plan or on demand</Text>
                </Table.Td>
                <Table.Td>
                  <Text size={FS.body} fw={600}>{dollars(c.rest * HOURS_A_MONTH)}</Text>
                  <Text size={FS.micro} c="dimmed">at list price</Text>
                </Table.Td>
              </Table.Tr>
            )
          })}
        </DataTable>
      </SectionCard>

      <SectionCard title="What you have" flush
        description="Read from AWS with each account's inventory. Use is matched against the instances running at the last read; a Capacity Reservation's use is AWS's own count.">
        <DataTable minWidth={860} loading={q.isPending} isEmpty={rows.length === 0}
          empty={<EmptyState icon={IconReceipt} title="No reservations" description="No Reserved Instances, Savings Plans or Capacity Reservations in the accounts read." />}
          columns={['Kind', 'Covers', 'Where', 'Term', 'Ends', 'Use']}>
          {rows.map((r) => <HeldRow key={r.key} r={r} />)}
        </DataTable>
      </SectionCard>
    </Stack>
  )
}

function HeldRow({ r }: { r: Held }) {
  const k = KIND[r.kind]
  const left = daysUntil(r.ends)
  const pct = r.use === null ? null : Math.round(r.use * 100)
  return (
    <Table.Tr data-testid={`held-${r.item.resource.id}`}>
      <Table.Td><Badge variant="light" color={k.color} radius="sm" tt="none">{k.label}</Badge></Table.Td>
      <Table.Td>
        <Text size={FS.body} fw={600}>{r.covers}</Text>
        <Text size={FS.micro} c="dimmed">{r.item.accountName}</Text>
      </Table.Td>
      <Table.Td><Text size={FS.meta}>{r.where}</Text></Table.Td>
      <Table.Td><Text size={FS.meta}>{r.term}</Text></Table.Td>
      <Table.Td>
        <Group gap={6} wrap="nowrap">
          <Text size={FS.meta}>{r.ends ? day(r.ends) : r.kind === 'cr' ? 'Until cancelled' : '—'}</Text>
          {left !== null && left >= 0 && left <= 30 && <Badge size="sm" variant="light" color="amber" radius="sm" tt="none">{left} day{left === 1 ? '' : 's'}</Badge>}
        </Group>
      </Table.Td>
      <Table.Td w={220}>
        {pct !== null && (
          <Box mb={4}>
            <Text size={FS.meta} fw={600}>{pct}% used</Text>
            <Progress value={pct} size="sm" mt={4} color={pct >= 80 ? 'teal' : pct > 0 ? 'amber' : 'rose'} aria-label={`${pct}% used`} />
          </Box>
        )}
        <Text size={FS.micro} c="dimmed" lineClamp={3}>{r.useNote}</Text>
        {r.unused > 0 && <Text size={FS.micro} c={INK.denied} mt={4}>{dollars(r.unused)} a month unused</Text>}
      </Table.Td>
    </Table.Tr>
  )
}

/** Says which accounts' reservations are not read, and why. */
function ReadNotes({ v }: { v: ReservationsView }) {
  const unread = v.accounts.filter((a) => !a.read)
  const needRole = v.accounts.filter((a) => a.savingsPlansNeedRole)
  const failed = v.accounts.filter((a) => a.read && (!a.reservedInstances || !a.capacityReservations || (!a.savingsPlans && !a.savingsPlansNeedRole)))
  if (!unread.length && !needRole.length && !failed.length) return null
  const names = (xs: { name: string }[]) => xs.map((a) => a.name).join(', ')
  return (
    <Stack gap="xs">
      {unread.length > 0 && (
        <Alert color="sky" variant="light" p="sm">
          <Text size={FS.meta}>Reservations in {names(unread)} are read with the next inventory read, within a few minutes.</Text>
        </Alert>
      )}
      {needRole.length > 0 && (
        <Alert color="amber" variant="light" p="sm" data-testid="reservations-need-role">
          <Text size={FS.meta}>
            Savings Plans in {names(needRole)} could not be read: Argus's read role there was made before it read them and lacks
            savingsplans:DescribeSavingsPlans. Deploy the current template over the same stack from the account's Overview card; nothing else changes.
          </Text>
        </Alert>
      )}
      {failed.length > 0 && (
        <Alert color="amber" variant="light" p="sm">
          <Text size={FS.meta}>Part of the reservations in {names(failed)} could not be read at the last read; what was read before is shown.</Text>
        </Alert>
      )}
    </Stack>
  )
}
