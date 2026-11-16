import {
  Alert, Box, Button, Group, Modal, NumberInput, SegmentedControl, Select, SimpleGrid, Stack, Text, TextInput, Textarea,
} from '@mantine/core'
import { useDebouncedValue } from '@mantine/hooks'
import { useQuery } from '@tanstack/react-query'
import { IconUserCheck } from '@tabler/icons-react'
import { useMemo, useState } from 'react'
import { cloudAccountsQuery, cloudLaunchOptionsQuery, roleLacks, useAskCloudChange } from '~/lib/cloud'
import { cloudQuoteQuery, familyOf, sameTotal, type OfferQuery, type Payment } from '~/lib/cloudReservations'
import { notifyError } from '~/lib/notify'
import { FS, INK } from '~/theme'
import { reportChange } from './ChangeModal'
import { RoleNeedsUpdate } from './StorageActions'

/*
 * Buying a Reserved Instance or a Savings Plan: a one- or three-year
 * commitment AWS does not refund. The dialog prices it with AWS as it is
 * filled in, says what it commits to and saves, and asks for the total to be
 * typed. It always waits for someone else, in every environment; the
 * control plane prices it again when asked and just before buying.
 */

const PAYMENTS: { value: Payment; label: string }[] = [
  { value: 'No Upfront', label: 'No upfront' }, { value: 'Partial Upfront', label: 'Partial' }, { value: 'All Upfront', label: 'All upfront' },
]

const usd = (n: number) => (n >= 100 ? `$${Math.round(n).toLocaleString('en-US')}` : `$${n.toFixed(2)}`)

export interface BuyPreset { accountId?: string; query?: Partial<OfferQuery> }

export function BuyReservation({ onClose, preset }: { onClose: () => void; preset?: BuyPreset }) {
  const accounts = useQuery(cloudAccountsQuery())
  const manageable = (accounts.data?.accounts ?? []).filter((a) => a.access === 'manage' && a.provider === 'aws')
  const p = preset?.query ?? {}
  const [acct, setAcct] = useState<string | null>(preset?.accountId ?? (manageable.length === 1 ? manageable[0]!.id : null))
  const account = manageable.find((a) => a.id === acct)
  const [reg, setReg] = useState<string | null>(p.region ?? null)
  const [kind, setKind] = useState<'ri' | 'sp'>(p.kind ?? 'ri')
  const [planType, setPlanType] = useState<'Compute' | 'EC2Instance'>(p.planType ?? 'Compute')
  // A Compute plan applies in every region; the change is still asked in one of the account's.
  const anyRegion = kind === 'sp' && planType === 'Compute'
  const region = reg && account?.regions.includes(reg) ? reg : account?.regions.length === 1 || anyRegion ? account?.regions[0] ?? null : null
  const opts = useQuery(cloudLaunchOptionsQuery(acct, region))
  const [size, setSize] = useState<string | null>(p.size ?? null)
  const [count, setCount] = useState<number | string>(p.count ?? 1)
  const [family, setFamily] = useState<string | null>(p.family ?? null)
  const [commitment, setCommitment] = useState<number | string>(p.commitment ?? 1)
  const [term, setTerm] = useState<'12' | '36'>(p.termMonths === 36 ? '36' : '12')
  const [payment, setPayment] = useState<Payment>(p.payment ?? 'No Upfront')
  const [typed, setTyped] = useState('')
  const [reason, setReason] = useState('')
  const ask = useAskCloudChange()

  const families = useMemo(() => [...new Set((opts.data?.sizes ?? []).map(familyOf))].sort(), [opts.data])
  const n = Number(count)
  const c = Number(commitment)
  const query: OfferQuery | null = useMemo(() => {
    if (!region) return null
    const base = { region, termMonths: Number(term) as 12 | 36, payment }
    if (kind === 'ri') {
      return size && Number.isInteger(n) && n >= 1 && n <= 100 ? { ...base, kind, size, count: n } : null
    }
    if (!(c >= 0.001 && c <= 1000)) return null
    if (planType === 'EC2Instance') return family ? { ...base, kind, planType, family, commitment: c } : null
    return { ...base, kind, planType, commitment: c }
  }, [region, term, payment, kind, size, n, planType, family, c])
  const [settled] = useDebouncedValue(query, 400)
  const quote = useQuery(cloudQuoteQuery(acct, settled))
  const q = settled === query ? quote.data : undefined
  const action = kind === 'ri' ? 'ri-buy' : 'sp-buy'
  const matches = q ? sameTotal(typed, q.offer.total) : false
  const ready = Boolean(q && matches && reason.trim().length >= 5 && !roleLacks(account, action))

  const buy = async () => {
    try {
      const out = await ask.mutateAsync({ accountId: acct!, region: region!, action, spec: { ...query }, confirm: typed.trim(), reason })
      reportChange(out, 'Bought')
      onClose()
    } catch (err) {
      void quote.refetch()
      notifyError('Could not ask to buy', err)
    }
  }

  const what = kind === 'ri'
    ? (size ? `${n} × ${size}, Linux, ${region ?? ''}, regional` : '')
    : planType === 'Compute' ? `$${c.toFixed(3)} an hour of compute, any region` : `$${c.toFixed(3)} an hour of ${family ?? ''} in ${region ?? ''}`

  return (
    <Modal opened onClose={onClose} centered size="lg" title={
      <Stack gap={2}>
        <Text fw={700}>{kind === 'ri' ? 'Buy a Reserved Instance' : 'Buy a Savings Plan'}</Text>
        <Text size={FS.meta} c="dimmed">A commitment for the whole term. AWS does not refund it, and Argus cannot undo it.</Text>
      </Stack>
    }>
      <Stack gap="md" data-testid="buy-reservation">
        <RoleNeedsUpdate account={account} action={action} />
        <SegmentedControl value={kind} onChange={(v) => { setKind(v as 'ri' | 'sp'); setTyped('') }} fullWidth
          data={[{ value: 'ri', label: 'Reserved Instance' }, { value: 'sp', label: 'Savings Plan' }]} />
        <Group grow align="flex-start">
          {manageable.length > 1 && (
            <Select label="Account" placeholder="Pick an account" value={acct} allowDeselect={false}
              onChange={(v) => { setAcct(v); setReg(null); setSize(null) }} data={manageable.map((a) => ({ value: a.id, label: a.name }))} />
          )}
          {account && account.regions.length > 1 && !anyRegion && (
            <Select label="Region" value={region} onChange={setReg} data={account.regions} allowDeselect={false} />
          )}
        </Group>
        {kind === 'ri' ? (
          <Group grow align="flex-start">
            <Select label="Instance type" placeholder="Pick a type" searchable value={size} onChange={setSize} allowDeselect={false}
              data={opts.data?.sizes ?? []} description="Covers any size of its family, for Linux." />
            <NumberInput label="Instances" min={1} max={100} value={count} onChange={setCount} allowDecimal={false} />
          </Group>
        ) : (
          <>
            <SegmentedControl size="xs" value={planType} onChange={(v) => setPlanType(v as 'Compute' | 'EC2Instance')}
              data={[{ value: 'Compute', label: 'Compute: any instance, Fargate and Lambda' }, { value: 'EC2Instance', label: 'EC2 Instance: one family, one region' }]} />
            <Group grow align="flex-start">
              {planType === 'EC2Instance' && (
                <Select label="Family" placeholder="Pick a family" searchable value={family} onChange={setFamily} data={families} allowDeselect={false} />
              )}
              <NumberInput label="Commitment, an hour" prefix="$" decimalScale={3} min={0.001} max={1000} step={0.1} value={commitment}
                onChange={setCommitment} description="Spent every hour of the term, used or not." />
            </Group>
          </>
        )}
        <Group grow align="flex-start">
          <Stack gap={4}>
            <Text size={FS.meta} fw={600}>Term</Text>
            <SegmentedControl size="xs" value={term} onChange={(v) => setTerm(v as '12' | '36')} data={[{ value: '12', label: '1 year' }, { value: '36', label: '3 years' }]} />
          </Stack>
          <Stack gap={4}>
            <Text size={FS.meta} fw={600}>Payment</Text>
            <SegmentedControl size="xs" value={payment} onChange={(v) => setPayment(v as Payment)} data={PAYMENTS} />
          </Stack>
        </Group>

        {quote.error && settled === query && <Alert color="rose" variant="light" p="sm"><Text size={FS.meta}>{(quote.error as Error).message}</Text></Alert>}
        {!q && query && !quote.error && <Text size={FS.meta} c="dimmed">Asking AWS for the price…</Text>}
        {q && (
          <>
            <Stack gap={4}>
              <Row k="Covers" v={what} />
              <Row k="Account" v={account ? `${account.name} (${account.accountId})` : ''} />
              <Row k="Term and payment" v={`${term === '12' ? '1 year' : '3 years'} · ${payment.toLowerCase()}`} />
              {(kind === 'ri' || planType === 'EC2Instance') && (
                <Row k="Would cover today" v={q.covers.length ? q.covers.join(', ') : 'Nothing running in its family there now'} />
              )}
            </Stack>
            <Box p="md" style={{ border: '1px solid var(--mantine-color-default-border)', borderRadius: 'var(--mantine-radius-md)' }} data-testid="buy-totals">
              <SimpleGrid cols={{ base: 1, xs: 3 }} spacing="md">
                <Stack gap={0}>
                  <Text size={FS.micro} c="dimmed">You commit to</Text>
                  <Text fw={700} size="xl">{q.totalText}</Text>
                  <Text size={FS.micro} c="dimmed">
                    {q.offer.upfront > 0 ? `${usd(q.offer.upfront)} now` : '$0 upfront'}{q.offer.monthly > 0 ? `, ${usd(q.offer.monthly)} a month for ${term} months` : ''}
                  </Text>
                </Stack>
                {q.onDemand > 0 ? (
                  <>
                    <Stack gap={0}>
                      <Text size={FS.micro} c="dimmed">On demand would be</Text>
                      <Text fw={700} size="xl">{usd(q.onDemand)}</Text>
                      <Text size={FS.micro} c="dimmed">at list price, every hour of the term</Text>
                    </Stack>
                    <Stack gap={0}>
                      <Text size={FS.micro} c="dimmed">Saves</Text>
                      <Text fw={700} size="xl" c={q.saves > 0 ? 'teal' : INK.denied}>{usd(Math.max(q.saves, 0))}</Text>
                      <Text size={FS.micro} c="dimmed">{q.paysOffMonth ? `pays off from month ${q.paysOffMonth}` : 'never pays off at list price'}</Text>
                    </Stack>
                  </>
                ) : (
                  <Text size={FS.micro} c="dimmed" style={{ gridColumn: 'span 2' }}>
                    What a Savings Plan saves depends on what it lands on; AWS applies it to the biggest discount first. It is spent whether or not anything runs.
                  </Text>
                )}
              </SimpleGrid>
            </Box>
          </>
        )}
        <Alert color="amber" variant="light" icon={<IconUserCheck size={16} />} p="sm">
          <Text size={FS.meta} fw={600}>Needs a second person</Text>
          <Text size={FS.meta}>
            Buying is always approved by someone else, in every environment, and never by the person who asked. Argus checks AWS's price
            again just before it buys; if it changed, nothing is bought and you are asked again.
          </Text>
        </Alert>
        <TextInput label="Type the total to confirm" placeholder={q?.totalText ?? ''} value={typed} onChange={(e) => setTyped(e.currentTarget.value)}
          styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)' } }} disabled={!q} error={typed && q && !matches ? `Type ${q.totalText}` : undefined} />
        <Textarea label="Why" description="The person who approves reads this." autosize minRows={2} value={reason} onChange={(e) => setReason(e.currentTarget.value)} />
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button loading={ask.isPending} disabled={!ready} onClick={() => void buy()}>Ask to buy</Button>
        </Group>
      </Stack>
    </Modal>
  )
}

function Row({ k, v }: { k: string; v: string }) {
  return (
    <Group gap="md" wrap="nowrap" align="flex-start">
      <Text size={FS.meta} c="dimmed" w={150} style={{ flexShrink: 0 }}>{k}</Text>
      <Text size={FS.meta}>{v}</Text>
    </Group>
  )
}
