import { Alert, Group, NumberInput, SegmentedControl, Select, Stack, Switch, Text, TextInput } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { cloudAccountsQuery, cloudItemsQuery, cloudLaunchOptionsQuery, roleLacks } from '~/lib/cloud'
import type { ReservationItem } from '~/lib/cloudReservations'
import { FS } from '~/theme'
import { ChangeModal, useSubmit } from './ChangeModal'
import { RoleNeedsUpdate } from './StorageActions'

/*
 * Capacity Reservations from Cloud › Reservations: keep room for instances
 * of one type in one zone, or give it back. Both are Change level and go
 * through the usual change, with approval where the environment needs it.
 * AWS bills the room from the moment it has it, used or not; both dialogs
 * say so before anything is asked.
 */

const MAX_COUNT = 100

/** Days from now as an ISO time, for "ends after N days". */
export function endAfter(days: number, now = Date.now()): string {
  return new Date(now + days * 86_400_000).toISOString()
}

export function NewCapacity({ onClose }: { onClose: () => void }) {
  const accounts = useQuery(cloudAccountsQuery())
  const subnets = useQuery(cloudItemsQuery('subnet'))
  const manageable = (accounts.data?.accounts ?? []).filter((a) => a.access === 'manage')
  const [acct, setAcct] = useState<string | null>(manageable.length === 1 ? manageable[0]!.id : null)
  const account = manageable.find((a) => a.id === acct)
  const [reg, setReg] = useState<string | null>(account?.regions.length === 1 ? account.regions[0]! : null)
  const region = reg ?? (account?.regions.length === 1 ? account.regions[0]! : null)
  const opts = useQuery(cloudLaunchOptionsQuery(acct, region))
  const zones = [...new Set((subnets.data ?? []).filter((s) => s.accountId === acct && s.resource.region === region && s.resource.zone)
    .map((s) => s.resource.zone!))].sort()
  const [zone, setZone] = useState<string | null>(null)
  const [size, setSize] = useState<string | null>(null)
  const [count, setCount] = useState<number | string>(1)
  const [targeted, setTargeted] = useState(false)
  const [ends, setEnds] = useState<'never' | 'after'>('never')
  const [days, setDays] = useState<number | string>(30)
  const [name, setName] = useState('')
  const [reason, setReason] = useState('')
  const { submit, busy } = useSubmit(onClose)
  const n = Number(count)
  const d = Number(days)
  const okCount = Number.isInteger(n) && n >= 1 && n <= MAX_COUNT
  const okEnd = ends === 'never' || (Number.isInteger(d) && d >= 1 && d <= 1825)
  const pickedZone = zone && zones.includes(zone) ? zone : zones.length === 1 ? zones[0]! : null

  return (
    <ChangeModal title="Reserve capacity" env={account?.environment ?? 'production'} level="change" reason={reason} setReason={setReason}
      onClose={onClose} busy={busy} submit="Reserve it"
      disabled={!acct || !region || !pickedZone || !size || !okCount || !okEnd || name.length > 128 || roleLacks(account, 'capacity-create')}
      onSubmit={() => void submit({ accountId: acct!, region: region!, action: 'capacity-create', reason, spec: {
        zone: pickedZone, size, count: n, targeted, name: name.trim() || undefined, endAt: ends === 'after' ? endAfter(d) : undefined,
      } }, 'Capacity reserved')}>
      <RoleNeedsUpdate account={account} action="capacity-create" />
      <Alert color="amber" variant="light" p="sm">
        <Text size={FS.meta}>
          AWS bills the room from the moment it has it, at the on-demand rate, whether anything runs in it or not. An instance that
          lands in it is not billed twice. Cancel it here when it is no longer needed.
        </Text>
      </Alert>
      {manageable.length > 1 && (
        <Select label="Account" placeholder="Pick an account" value={acct} allowDeselect={false}
          onChange={(v) => { setAcct(v); setReg(null); setZone(null); setSize(null) }}
          data={manageable.map((a) => ({ value: a.id, label: a.name }))} />
      )}
      {account && account.regions.length > 1 && (
        <Select label="Region" value={region} onChange={(v) => { setReg(v); setZone(null) }} data={account.regions} allowDeselect={false} />
      )}
      <Group grow align="flex-start">
        <Select label="Zone" placeholder={zones.length ? 'Pick a zone' : 'No subnets read here yet'} value={pickedZone} onChange={setZone}
          data={zones} allowDeselect={false} />
        <Select label="Instance type" placeholder="Pick a type" searchable value={size} onChange={setSize} allowDeselect={false}
          data={opts.data?.sizes ?? []} />
        <NumberInput label="Instances" min={1} max={MAX_COUNT} value={count} onChange={setCount} allowDecimal={false}
          error={!okCount ? `1 to ${MAX_COUNT}` : undefined} />
      </Group>
      <Switch checked={targeted} onChange={(e) => setTargeted(e.currentTarget.checked)} label="Only for launches that name it"
        description={targeted
          ? 'Only a launch that picks this reservation lands in it; nothing running moves in by itself.'
          : 'Any running or new instance of this type in this zone fills it, which is how most teams use one.'} />
      <Stack gap={4}>
        <Text size={FS.meta} fw={600}>Ends</Text>
        <Group gap="sm" align="center">
          <SegmentedControl size="xs" value={ends} onChange={(v) => setEnds(v as 'never' | 'after')}
            data={[{ value: 'never', label: 'When cancelled' }, { value: 'after', label: 'After a number of days' }]} />
          {ends === 'after' && (
            <NumberInput aria-label="Days" w={120} min={1} max={1825} value={days} onChange={setDays} allowDecimal={false} suffix=" days"
              error={!okEnd ? '1 to 1825' : undefined} />
          )}
        </Group>
      </Stack>
      <TextInput label="Name (optional)" placeholder="checkout-burst" value={name} onChange={(e) => setName(e.currentTarget.value)} maxLength={128} />
      <Text size={FS.meta} c="dimmed">The preview after you ask shows the monthly cost of the room while it is empty, at list price.</Text>
    </ChangeModal>
  )
}

export function CancelCapacity({ it, onClose }: { it: ReservationItem; onClose: () => void }) {
  const accounts = useQuery(cloudAccountsQuery())
  const account = accounts.data?.accounts.find((a) => a.id === it.accountId)
  const [reason, setReason] = useState('')
  const { submit, busy } = useSubmit(onClose)
  const r = it.resource
  const total = Number(r.details?.total ?? 0)
  const used = total - Number(r.details?.available ?? 0)
  return (
    <ChangeModal title={`Cancel ${r.name}`} env={it.environment} level="change" reason={reason} setReason={setReason}
      onClose={onClose} busy={busy} submit="Cancel reservation" color="amber" disabled={roleLacks(account, 'capacity-cancel')}
      onSubmit={() => void submit({ accountId: it.accountId, region: r.region, action: 'capacity-cancel', resourceId: r.id, reason },
        'Reservation cancelled')}>
      <RoleNeedsUpdate account={account} action="capacity-cancel" />
      <Text size={FS.body}>
        Gives back the room for {total} × {r.size} in {r.zone}. AWS stops billing it within the hour.
      </Text>
      <Text size={FS.meta} c="dimmed">
        {used > 0
          ? `The ${used} instance${used === 1 ? '' : 's'} in it keep running at the usual rate; only the promise of room goes.`
          : 'Nothing runs in it, so nothing else changes.'}
      </Text>
    </ChangeModal>
  )
}
