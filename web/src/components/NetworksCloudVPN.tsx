import { Alert, Checkbox, Group, SegmentedControl, Select, Stack, Text, TextInput } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { useMemo, useState } from 'react'
import { ChangeModal, useSubmit } from '~/components/cloud/ChangeModal'
import { RoleNeedsUpdate } from '~/components/cloud/StorageActions'
import { cloudAccountsQuery, cloudItemsQuery, roleLacks } from '~/lib/cloud'
import { assetsQuery } from '~/lib/queries'
import { FS } from '~/theme'

/*
 * Making the AWS end of an IPsec link (internal/control/cloud_vpn_routes.go).
 * It is a cloud change with the environment's approval. Once AWS has made the
 * Site-to-Site VPN, Argus asks for the link from the server to its first
 * tunnel, with a key it made for both; that link waits for its own approval.
 */

const mono = { input: { fontFamily: 'var(--mantine-font-family-monospace)' } }

const isPrefix = (s: string) => /^(\d{1,3}\.){3}\d{1,3}\/\d{1,2}$/.test(s.trim())
const isPublic = (s: string) => {
  const m = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(s.trim())
  if (!m) return false
  const [a, b] = [Number(m[1]), Number(m[2])]
  if (m.slice(1).some((x) => Number(x) > 255)) return false
  return !(a === 10 || a === 127 || a === 0 || a >= 224 || (a === 172 && b >= 16 && b < 32) || (a === 192 && b === 168) ||
    (a === 169 && b === 254) || (a === 100 && b >= 64 && b < 128))
}

/** Whether any connected AWS account lets Argus make changes. */
export function useAWSManage() {
  const accounts = useQuery(cloudAccountsQuery())
  return (accounts.data?.accounts ?? []).some((a) => a.provider === 'aws' && a.access === 'manage')
}

export function MakeAWSVPN({ onClose }: { onClose: () => void }) {
  const accounts = useQuery(cloudAccountsQuery())
  const vpcs = useQuery(cloudItemsQuery('network'))
  const { data: assets } = useQuery(assetsQuery())
  const manage = (accounts.data?.accounts ?? []).filter((a) => a.provider === 'aws' && a.access === 'manage')
  const choices = useMemo(() => (vpcs.data ?? []).filter((v) => manage.some((a) => a.id === v.accountId)), [vpcs.data, manage])
  const servers = useMemo(() => (assets ?? []).filter((a) => !a.protocol || a.protocol === 'ssh'), [assets])
  const [vpcKey, setVpcKey] = useState<string | null>(null)
  const vpc = choices.find((v) => v.accountId + '/' + v.resource.region + '/' + v.resource.id === vpcKey)
  const account = manage.find((a) => a.id === vpc?.accountId)
  const [name, setName] = useState('')
  const [assetId, setAssetId] = useState<string | null>(null)
  const [address, setAddress] = useState('')
  const [serverRange, setServerRange] = useState('')
  const [cloudRange, setCloudRange] = useState('')
  const [ciphers, setCiphers] = useState<'strong' | 'compatible'>('strong')
  const [propagate, setPropagate] = useState(true)
  const [reason, setReason] = useState('')
  const { submit, busy } = useSubmit(onClose)
  const host = servers.find((s) => s.id === assetId)?.hostname
  const addressError = address.trim() && !isPublic(address) ? 'Its public IPv4 address, as AWS sees it' : undefined
  const rangeError = serverRange.trim() && !isPrefix(serverRange) ? 'One range, such as 192.168.10.0/24' : undefined
  const cloudError = cloudRange.trim() && !isPrefix(cloudRange) ? 'One range, such as 10.30.0.0/16' : undefined
  const ready = vpc && name.trim() && assetId && isPublic(address) && isPrefix(serverRange) && (!cloudRange.trim() || isPrefix(cloudRange))
  return (
    <ChangeModal title="Make the AWS end of a link" env={vpc?.environment ?? 'production'} level="change" reason={reason} setReason={setReason}
      onClose={onClose} busy={busy} submit="Make it" disabled={!ready || roleLacks(account, 'vpn-create')}
      onSubmit={() => void submit({
        accountId: vpc!.accountId, region: vpc!.resource.region, action: 'vpn-create', reason,
        spec: { name: name.trim(), network: vpc!.resource.id, assetId, serverAddress: address.trim(), serverRange: serverRange.trim(),
          cloudRange: cloudRange.trim() || undefined, ciphers, propagate },
      }, 'AWS VPN asked for')}>
      <RoleNeedsUpdate account={account} action="vpn-create" />
      <Text size={FS.meta} c="dimmed">
        For a VPC with no VPN yet. Argus makes the customer gateway, the VPC's virtual private gateway when it has none, and a
        Site-to-Site VPN connection with static routing. Then it asks for the IPsec link from your server to the first tunnel.
      </Text>
      <TextInput label="Name" description="For the connection in AWS and the link in Argus" placeholder="Jakarta office to AWS"
        value={name} onChange={(e) => setName(e.currentTarget.value)} maxLength={80} />
      <Select label="VPC" searchable value={vpcKey} onChange={(v) => { setVpcKey(v); setCloudRange('') }}
        nothingFoundMessage="No VPC read in an AWS account Argus may change"
        data={choices.map((v) => ({ value: v.accountId + '/' + v.resource.region + '/' + v.resource.id,
          label: `${v.resource.name} · ${v.resource.size ?? ''} · ${v.accountName}, ${v.resource.region}` }))} />
      <Group grow align="flex-start">
        <Select label="Your server" description="The Argus server strongSwan runs on" searchable value={assetId} onChange={setAssetId}
          data={servers.map((s) => ({ value: s.id, label: s.hostname }))} />
        <TextInput label="Its public address" description="As AWS sees it; behind NAT, the NAT's" placeholder="36.72.1.9" styles={mono}
          value={address} onChange={(e) => setAddress(e.currentTarget.value)} error={addressError} />
      </Group>
      <Group grow align="flex-start">
        <TextInput label="Range behind your server" placeholder="192.168.10.0/24" styles={mono} value={serverRange}
          onChange={(e) => setServerRange(e.currentTarget.value)} error={rangeError} />
        <TextInput label="Range in the VPC" description={vpc ? `Empty for all of ${vpc.resource.size}` : undefined}
          placeholder={vpc?.resource.size ?? '10.30.0.0/16'} styles={mono} value={cloudRange}
          onChange={(e) => setCloudRange(e.currentTarget.value)} error={cloudError} />
      </Group>
      <Stack gap={4}>
        <Text size={FS.body} fw={500}>Ciphers</Text>
        <SegmentedControl value={ciphers} onChange={(v) => setCiphers(v as 'strong' | 'compatible')}
          data={[{ value: 'strong', label: 'Strong' }, { value: 'compatible', label: 'Compatible' }]} style={{ alignSelf: 'flex-start' }} />
      </Stack>
      <Checkbox checked={propagate} onChange={(e) => setPropagate(e.currentTarget.checked)}
        label={`Let the VPC's route tables learn ${serverRange.trim() || 'your range'} through the gateway`}
        description="Without it, replies from the VPC don't find their way back until you add the route yourself." />
      <Alert color="azure" variant="light" p="sm" data-testid="vpn-cost">
        <Text size={FS.meta}>
          AWS bills from about US$0.05 an hour per connection in most regions (about $36 a month), plus data sent out of AWS. The pre-shared
          key is made by Argus, set on both tunnels, and kept sealed; it is never in the change. Argus uses the first tunnel, so the second stays
          down by design. {host ? `Nothing changes on ${host} until another admin approves the link.` : ''}
        </Text>
      </Alert>
    </ChangeModal>
  )
}
