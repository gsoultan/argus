import {
  Alert, Badge, Box, Button, Checkbox, Group, Modal, NumberInput, Progress, Select, SimpleGrid, Stack, Table, Text, TextInput, Tooltip,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconCoin, IconPencil, IconPlus, IconRefresh, IconTarget, IconTrash, IconZzz } from '@tabler/icons-react'
import { useState } from 'react'
import { DataTable, EmptyState, SectionCard } from '~/components/page'
import { Stat, relTime } from '~/components/primitives'
import type { CloudAccount, CloudItem, CloudKind } from '~/lib/cloud'
import {
  budgetState, change, daysLeft, cloudCostsQuery, cloudIdleQuery, dollars, parseThresholds, totals, useDeleteBudget, useReadCosts, useSaveBudget,
  type CloudBudget, type CloudCostRead, type CloudIdleItem, type DayCost,
} from '~/lib/cloudCosts'
import { notifyError, notifyOk } from '~/lib/notify'
import { meQuery } from '~/lib/queries'
import { canSee } from '~/lib/roles'
import { FS, INK } from '~/theme'
import { EnvBadge, ResourceDrawer } from './common'

/*
 * What the connected accounts cost, what in them bills for nothing, and
 * monthly budgets that alert. Spend is AWS's own bill, read from Cost
 * Explorer every few hours; idle resources come from the inventory, priced
 * at list price.
 */

const KIND_WORD: Partial<Record<CloudKind, string>> = {
  volume: 'Disk', address: 'Elastic IP', instance: 'Instance', snapshot: 'Snapshot', 'load-balancer': 'Load balancer',
}

export function CostsPanel({ accounts, items, names }: {
  accounts: CloudAccount[]
  /** The inventory, so an idle resource opens in its usual panel. */
  items: Partial<Record<CloudKind, CloudItem[] | undefined>>
  names: Map<string, string>
}) {
  const { data: me } = useQuery(meQuery())
  const admin = canSee(me?.role, 'admin')
  const q = useQuery(cloudCostsQuery())
  const idle = useQuery(cloudIdleQuery())
  const [open, setOpen] = useState<CloudItem | null>(null)
  const reads = q.data?.accounts ?? []
  const t = totals(reads)
  const vs = change(t.monthToDate, t.lastMonthToDate)
  const never = reads.length > 0 && reads.every((r) => !r.costs)
  const openIdle = (x: CloudIdleItem) => setOpen(items[x.kind]?.find((it) => it.accountId === x.accountId && it.resource.id === x.id) ?? null)

  return (
    <Stack gap="lg">
      {q.error && <Alert color="rose" variant="light">{(q.error as Error).message}</Alert>}
      <SimpleGrid cols={{ base: 1, xs: 2, md: 4 }} spacing="md">
        <Stat label="Spent this month" value={q.data ? (t.read ? dollars(t.monthToDate) : '—') : undefined} icon={IconCoin}
          sub={t.read ? (vs === null ? 'Nothing last month to compare with' : `${vs > 0 ? '+' : ''}${vs}% on last month to this day`) : 'Not read yet'} />
        <Stat label="Forecast for the month" value={q.data ? (t.forecast !== null ? dollars(t.forecast) : '—') : undefined}
          sub={t.forecast === null ? 'AWS gives one after a few weeks of data'
            : t.forecastRest == null || !t.month ? 'AWS\'s own forecast'
              : `${dollars(Math.max(0, t.forecast - t.forecastRest))} to yesterday + ${dollars(t.forecastRest)} AWS predicts for ${daysLeft(t.month)} days left`} />
        <Stat label="Last month" value={q.data ? (t.read ? dollars(t.lastMonth) : '—') : undefined} />
        <Stat label="Idle, a month" value={idle.data ? dollars(idle.data.monthly) : undefined} icon={IconZzz}
          tone={idle.data && idle.data.monthly > 0 ? 'warn' : 'ok'} color="amber"
          sub={idle.data ? `${idle.data.items.length} resource${idle.data.items.length === 1 ? '' : 's'} billing for nothing` : undefined} />
      </SimpleGrid>

      {never && (
        <Alert color="sky" variant="light">
          Argus reads each account's bill from AWS Cost Explorer every {q.data?.intervalHours ?? 8} hours. The first read is on its way.
        </Alert>
      )}

      {t.days.length > 0 && (
        <SectionCard title="Spend by day" description="This month in blue, last month in grey, every connected account together.">
          <DayBars days={t.days} month={t.month!} />
        </SectionCard>
      )}

      <SectionCard title="By account" flush
        description={`Read from AWS Cost Explorer every ${q.data?.intervalHours ?? 8} hours, as unblended cost. The forecast is AWS's own forecast for the whole month, the figure the Billing and Cost Management home page shows as Total forecasted cost for current month. AWS charges $0.01 a request and a read is two, about $1.80 a month an account.`}>
        <DataTable minWidth={760} loading={q.isPending} isEmpty={reads.length === 0}
          empty={<EmptyState icon={IconCoin} title="No accounts" description="Connect an AWS account to see what it costs." />}
          columns={['Account', 'This month', 'Last month, same day', 'Forecast', 'Last month', 'Read', '']}>
          {reads.map((r) => <AccountRow key={r.accountId} r={r} admin={admin} />)}
        </DataTable>
      </SectionCard>

      <Budgets budgets={q.data?.budgets} accounts={accounts} admin={admin} loading={q.isPending} />

      <SectionCard title="Idle resources" icon={IconZzz} iconColor="amber" flush
        description="Worked out from the inventory, at us-east-1 list prices. Open one to delete or release it through the usual change path.">
        <DataTable minWidth={760} loading={idle.isPending} isEmpty={(idle.data?.items.length ?? 0) === 0}
          empty={<EmptyState icon={IconZzz} title="Nothing idle" description="Unattached disks and addresses, instances stopped for two weeks, old snapshots of deleted disks and load balancers with nothing behind them show here." />}
          columns={['Resource', 'Account', 'Why', 'A month', '']}>
          {idle.data?.items.map((x) => (
            <Table.Tr key={x.accountId + x.id}>
              <Table.Td>
                <Text size={FS.meta} fw={500}>{x.name}</Text>
                <Text size={FS.micro} c="dimmed">{KIND_WORD[x.kind] ?? x.kind} · {x.region}{x.name !== x.id && !x.id.startsWith('arn:') ? ` · ${x.id}` : ''}</Text>
              </Table.Td>
              <Table.Td><Group gap={6} wrap="nowrap"><Text size={FS.meta}>{x.accountName}</Text><EnvBadge env={x.environment} /></Group></Table.Td>
              <Table.Td><Text size={FS.meta}>{x.why}</Text></Table.Td>
              <Table.Td><Text size={FS.meta} fw={500}>{x.atMost ? 'up to ' : ''}{dollars(x.monthly)}</Text></Table.Td>
              <Table.Td ta="right">
                <Button size="compact-xs" variant="default" onClick={() => openIdle(x)}
                  disabled={!items[x.kind]?.some((it) => it.resource.id === x.id)}>Open</Button>
              </Table.Td>
            </Table.Tr>
          ))}
        </DataTable>
      </SectionCard>

      {t.services.length > 0 && (
        <SectionCard title="Top services" flush description="Every connected account together.">
          <DataTable minWidth={520} columns={['Service', 'This month', 'Last month']}>
            {t.services.slice(0, 10).map((s) => (
              <Table.Tr key={s.service}>
                <Table.Td><Text size={FS.meta}>{s.service}</Text></Table.Td>
                <Table.Td><Text size={FS.meta} fw={500}>{dollars(s.thisMonth)}</Text></Table.Td>
                <Table.Td><Text size={FS.meta} c="dimmed">{dollars(s.lastMonth)}</Text></Table.Td>
              </Table.Tr>
            ))}
          </DataTable>
        </SectionCard>
      )}

      <ResourceDrawer it={open} onClose={() => setOpen(null)} names={names} />
    </Stack>
  )
}

function AccountRow({ r, admin }: { r: CloudCostRead; admin: boolean }) {
  const read = useReadCosts()
  const c = r.costs
  const vs = c ? change(c.monthToDate, c.lastMonthToDate) : null
  const go = async () => {
    try {
      const out = await read.mutateAsync(r.accountId)
      if (out.error) notifyError('AWS did not answer', new Error(out.error))
      else notifyOk('Costs read', `${r.accountName}: ${dollars(out.costs?.monthToDate ?? 0)} so far this month.`)
    } catch (err) {
      notifyError('Could not read costs', err)
    }
  }
  return (
    <Table.Tr>
      <Table.Td>
        <Group gap={6} wrap="nowrap"><Text size={FS.meta} fw={500}>{r.accountName}</Text><EnvBadge env={r.environment} /></Group>
        <Text size={FS.micro} c="dimmed">{r.cloudAccountId}</Text>
        {r.error && <Text size={FS.micro} c={INK.denied} maw={340}>{r.error}</Text>}
      </Table.Td>
      <Table.Td><Text size={FS.meta} fw={500}>{c ? dollars(c.monthToDate) : '—'}</Text></Table.Td>
      <Table.Td>
        <Text size={FS.meta} c="dimmed">{c ? dollars(c.lastMonthToDate) : '—'}</Text>
        {vs !== null && <Text size={FS.micro} c={vs > 10 ? INK.pending : 'dimmed'}>{vs > 0 ? '+' : ''}{vs}%</Text>}
      </Table.Td>
      <Table.Td><Text size={FS.meta}>{c?.forecast != null ? dollars(c.forecast) : '—'}</Text></Table.Td>
      <Table.Td><Text size={FS.meta} c="dimmed">{c ? dollars(c.lastMonth) : '—'}</Text></Table.Td>
      <Table.Td><Text size={FS.meta} c="dimmed">{r.readAt ? relTime(r.readAt) : 'Not yet'}</Text></Table.Td>
      <Table.Td ta="right">
        {admin && (
          <Tooltip label="Read now. AWS charges $0.01 a request." withArrow>
            <Button size="compact-xs" variant="default" leftSection={<IconRefresh size={12} />} loading={read.isPending} onClick={go}>Read</Button>
          </Tooltip>
        )}
      </Table.Td>
    </Table.Tr>
  )
}

/** Daily spend for last month and this, as bars; drawn in SVG so the page needs no chart library. */
function DayBars({ days, month }: { days: DayCost[]; month: string }) {
  const max = Math.max(...days.map((d) => d.amount), 0.01)
  const W = 8, GAP = 2, H = 120
  const width = days.length * (W + GAP)
  const first = days.findIndex((d) => d.date.startsWith(month))
  return (
    <Box>
      <svg viewBox={`0 0 ${width} ${H + 16}`} width="100%" height={H + 16} preserveAspectRatio="none" role="img"
        aria-label={`Daily spend from ${days[0]!.date} to ${days.at(-1)!.date}; the most in a day was ${dollars(max)}`}>
        {days.map((d, i) => {
          const h = Math.max((d.amount / max) * H, d.amount > 0 ? 1 : 0)
          const cur = d.date.startsWith(month)
          return (
            <rect key={d.date} x={i * (W + GAP)} y={H - h} width={W} height={h} rx={1.5}
              fill={cur ? 'var(--mantine-color-azure-6)' : 'var(--mantine-color-slate-3)'}>
              <title>{`${d.date}: ${dollars(d.amount)}`}</title>
            </rect>
          )
        })}
        {first > 0 && <line x1={first * (W + GAP) - GAP / 2} x2={first * (W + GAP) - GAP / 2} y1={0} y2={H} stroke="var(--mantine-color-slate-4)" strokeDasharray="3 3" />}
      </svg>
      <Group justify="space-between">
        <Text size={FS.micro} c="dimmed">{days[0]!.date}</Text>
        <Text size={FS.micro} c="dimmed">Most in a day {dollars(max)}</Text>
        <Text size={FS.micro} c="dimmed">{days.at(-1)!.date}</Text>
      </Group>
    </Box>
  )
}

function Budgets({ budgets, accounts, admin, loading }: { budgets?: CloudBudget[]; accounts: CloudAccount[]; admin: boolean; loading: boolean }) {
  const [edit, setEdit] = useState<CloudBudget | 'new' | null>(null)
  const del = useDeleteBudget()
  const remove = async (b: CloudBudget) => {
    try {
      await del.mutateAsync(b.id)
      notifyOk('Budget removed', b.name)
    } catch (err) {
      notifyError('Could not remove the budget', err)
    }
  }
  const list = budgets ?? []
  return (
    <SectionCard title="Budgets" icon={IconTarget} flush
      description="Monthly, for one account or all of them. Administrators and owners are alerted once a month for each threshold passed, and when AWS forecasts the month running over."
      action={admin ? <Button size="xs" leftSection={<IconPlus size={14} />} onClick={() => setEdit('new')}>New budget</Button> : undefined}>
      <DataTable minWidth={760} loading={loading} isEmpty={list.length === 0}
        empty={<EmptyState icon={IconTarget} title="No budgets" description="Set a monthly amount and Argus tells you when spend reaches 80% and 100% of it." />}
        columns={['Budget', 'Spent this month', 'Forecast', 'Alerts at', '']}>
        {list.map((b) => {
          const s = budgetState(b)
          const color = s.over ? 'rose' : s.passed > 0 || s.forecastOver ? 'amber' : 'teal'
          return (
            <Table.Tr key={b.id}>
              <Table.Td>
                <Text size={FS.meta} fw={500}>{b.name}</Text>
                <Text size={FS.micro} c="dimmed">{b.accountId ? b.accountName ?? 'One account' : 'Every account'} · {dollars(b.amount)} a month</Text>
              </Table.Td>
              <Table.Td miw={220}>
                <Group gap="xs" wrap="nowrap">
                  <Progress value={Math.min(s.pct, 100)} color={color} w={120} size="sm" aria-label={`${Math.round(s.pct)}% of ${b.name}`} />
                  <Text size={FS.meta}>{dollars(b.spent)}</Text>
                  <Text size={FS.micro} c="dimmed">{Math.round(s.pct)}%</Text>
                </Group>
                {b.unread > 0 && <Text size={FS.micro} c="dimmed">{b.unread} account{b.unread === 1 ? '' : 's'} not read this month yet</Text>}
              </Table.Td>
              <Table.Td>
                <Text size={FS.meta} c={s.forecastOver ? INK.pending : undefined}>{b.projected !== null ? dollars(b.projected) : '—'}</Text>
                {s.forecastOver && <Text size={FS.micro} c={INK.pending}>Over by {dollars(b.projected! - b.amount)}</Text>}
              </Table.Td>
              <Table.Td>
                <Group gap={4}>
                  {b.thresholds.map((p) => <Badge key={p} size="sm" variant={s.pct >= p ? 'filled' : 'light'} color={s.pct >= p ? color : 'slate'} radius="sm">{p}%</Badge>)}
                  {b.forecast && <Badge size="sm" variant="light" color="slate" radius="sm">Forecast</Badge>}
                </Group>
              </Table.Td>
              <Table.Td ta="right">
                {admin && (
                  <Group gap={4} justify="flex-end" wrap="nowrap">
                    <Button size="compact-xs" variant="default" leftSection={<IconPencil size={12} />} onClick={() => setEdit(b)}>Change</Button>
                    <Button size="compact-xs" variant="subtle" color="rose" aria-label={`Remove ${b.name}`} onClick={() => void remove(b)}><IconTrash size={13} /></Button>
                  </Group>
                )}
              </Table.Td>
            </Table.Tr>
          )
        })}
      </DataTable>
      {edit && <BudgetForm b={edit === 'new' ? null : edit} accounts={accounts} onClose={() => setEdit(null)} />}
    </SectionCard>
  )
}

function BudgetForm({ b, accounts, onClose }: { b: CloudBudget | null; accounts: CloudAccount[]; onClose: () => void }) {
  const [account, setAccount] = useState<string>(b?.accountId ?? '')
  const [name, setName] = useState(b?.name ?? '')
  const [amount, setAmount] = useState<number | string>(b?.amount ?? '')
  const [th, setTh] = useState((b?.thresholds ?? [80, 100]).join(', '))
  const [forecast, setForecast] = useState(b?.forecast ?? true)
  const save = useSaveBudget()
  const thresholds = parseThresholds(th)
  const amt = typeof amount === 'number' ? amount : Number(amount)
  const ok = name.trim() !== '' && amt > 0 && thresholds !== null
  const submit = async () => {
    try {
      await save.mutateAsync({ id: b?.id, accountId: account, name: name.trim(), amount: amt, thresholds: thresholds!, forecast })
      notifyOk(b ? 'Budget changed' : 'Budget set', `${name.trim()}: ${dollars(amt)} a month.`)
      onClose()
    } catch (err) {
      notifyError('Could not save the budget', err)
    }
  }
  return (
    <Modal opened onClose={onClose} title={<Text fw={700}>{b ? 'Change budget' : 'New budget'}</Text>} centered>
      <Stack gap="md">
        <Select label="For" value={account} onChange={(v) => setAccount(v ?? '')} allowDeselect={false}
          data={[{ value: '', label: 'Every account' }, ...accounts.map((a) => ({ value: a.id, label: `${a.name} · ${a.accountId}` }))]} />
        <TextInput label="Name" placeholder="Payments production" value={name} onChange={(e) => setName(e.currentTarget.value)} maxLength={80} data-autofocus />
        <NumberInput label="Amount a month" description="US dollars, as AWS bills them." prefix="$" thousandSeparator="," decimalScale={2}
          min={0} value={amount} onChange={setAmount} />
        <TextInput label="Alert at" description="Percentages of the amount, up to five." value={th} onChange={(e) => setTh(e.currentTarget.value)}
          error={thresholds === null ? 'Whole percentages from 1 to 200, such as 80, 100' : undefined} rightSection={<Text size={FS.meta} c="dimmed">%</Text>} />
        <Checkbox label="Also alert when AWS forecasts the month going over" checked={forecast} onChange={(e) => setForecast(e.currentTarget.checked)} />
        {b && <Text size={FS.meta} c="dimmed">Saving a change starts this month's alerts afresh.</Text>}
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button onClick={submit} loading={save.isPending} disabled={!ok}>{b ? 'Save' : 'Set budget'}</Button>
        </Group>
      </Stack>
    </Modal>
  )
}
