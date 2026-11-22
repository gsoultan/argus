import { Alert, Badge, Button, Checkbox, CopyButton, Group, Modal, PasswordInput, Radio, SegmentedControl, Select, Stack, Text, TextInput, Tooltip } from '@mantine/core'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { IconCheck, IconCloudLock, IconCopy, IconKey, IconPencil, IconPlus, IconTrash, IconX } from '@tabler/icons-react'
import { useMemo, useState } from 'react'
import { AloneOr, ApproveOwn } from '~/components/ApproveOwn'
import { AnchorLink } from '~/components/links'
import { EmptyState, SectionCard } from '~/components/page'
import { relTime } from '~/components/primitives'
import { isConfigured } from '~/lib/live'
import { run } from '~/lib/notify'
import {
  approveIPsec, askIPsecChange, askIPsecLink, askIPsecRemove, declineIPsec, IPSEC_LINK, ipsecLinksQuery, PROVIDER, showIPsecKey,
  WG_MEMBER_STATE, type IPsecDraft, type IPsecLink, type IPsecProposals,
} from '~/lib/networks'
import { assetsQuery } from '~/lib/queries'
import { FS, SP } from '~/theme'

/*
 * IPsec links from an Argus server to a cloud VPN gateway or an office
 * firewall, set up with strongSwan through its agent
 * (internal/control/ipsec_links.go). Cloud gateways speak IPsec only, which is
 * why these are not WireGuard.
 */

const CIPHERS: Record<IPsecProposals, string> = {
  strong: 'AES-256-GCM, SHA-384, ECP-384. AWS and Google Cloud take these; on Azure, set a custom IPsec policy with GCMAES256.',
  compatible: "AES-256, SHA-256, MODP-2048. For an Azure gateway on its default policy, or a firewall that can't do better.",
}

const ranges = (s: string) => s.split(/[\s,]+/).map((x) => x.trim()).filter(Boolean)

export function IPsecLinksCard({ canManage, me }: { canManage: boolean; me?: string }) {
  const { data, isLoading } = useQuery({ ...ipsecLinksQuery(), enabled: isConfigured() })
  const [editing, setEditing] = useState<IPsecLink | 'new' | null>(null)
  const links = data ?? []
  return (
    <SectionCard title="Links to clouds and offices" icon={IconCloudLock} iconColor="sky"
      description="IPsec links from one of your servers to an AWS, Azure or Google Cloud VPN gateway, or to an office firewall. Argus sets up strongSwan on the server; every change waits for another admin."
      action={canManage && <Button size="xs" variant="default" leftSection={<IconPlus size={14} />} onClick={() => setEditing('new')}>New link</Button>}>
      {isLoading ? null : links.length === 0 ? (
        <EmptyState icon={IconCloudLock} title="No IPsec links yet."
          description="Link an office or data centre server to a cloud network: make the VPN connection in the cloud, then give Argus its tunnel address and key." />
      ) : (
        <Stack gap="lg">{links.map((l) => <Link key={l.id} l={l} canManage={canManage} me={me} onEdit={() => setEditing(l)} />)}</Stack>
      )}
      {editing && <LinkEditor l={editing === 'new' ? null : editing} onClose={() => setEditing(null)} />}
    </SectionCard>
  )
}

function Link({ l, canManage, me, onEdit }: { l: IPsecLink; canManage: boolean; me?: string; onEdit: () => void }) {
  const qc = useQueryClient()
  const done = () => qc.invalidateQueries({ queryKey: ['networks'] })
  const approve = useMutation({ mutationFn: (c?: string) => approveIPsec(l.id, c), onSettled: done })
  const decline = useMutation({ mutationFn: () => declineIPsec(l.id), onSettled: done })
  const remove = useMutation({ mutationFn: () => askIPsecRemove(l.id), onSettled: done })
  const [key, setKey] = useState<string | null>(null)
  const reveal = useMutation({ mutationFn: () => showIPsecKey(l.id), onSuccess: (v) => setKey(v.psk) })
  const p = l.pending
  const mine = !!me && l.pendingBy?.toLowerCase() === me.toLowerCase()
  const link = l.link ? IPSEC_LINK[l.link] : undefined
  const server = l.server ? WG_MEMBER_STATE[l.server] : undefined
  const what = !p ? '' : p.remove ? 'remove it' : l.state === 'waiting' ? 'set it up' : `change it${p.newKey ? ', with a new key' : ''}`
  return (
    <Stack gap={SP.cozy} data-testid="ipsec-link">
      <Group justify="space-between" wrap="wrap" gap="xs">
        <Group gap="xs">
          <Text size={FS.body} fw={600}>{l.name}</Text>
          {l.state === 'waiting' && <Badge color="amber" variant="light">Waiting for approval</Badge>}
          {link && <Badge color={link.color} variant={link.color === 'rose' ? 'filled' : 'light'}>{link.label}</Badge>}
          {server && l.server !== 'applied' && (
            <Tooltip multiline w={300} label={l.serverMessage || server.hint}><Badge color={server.color} variant="light">{server.label}</Badge></Tooltip>
          )}
        </Group>
        {canManage && l.state === 'active' && (
          <Group gap="xs">
            <Button size="xs" variant="subtle" leftSection={<IconKey size={14} />} loading={reveal.isPending}
              onClick={() => void run(() => reveal.mutateAsync(), { failure: 'Key not shown' })}>Show key</Button>
            {!p && <Button size="xs" variant="subtle" leftSection={<IconPencil size={14} />} onClick={onEdit}>Change</Button>}
            {!p && <Button size="xs" variant="subtle" color="rose" leftSection={<IconTrash size={14} />}
              onClick={() => void run(() => remove.mutateAsync(), { failure: 'Not asked', success: ['Asked', 'It waits for another admin to approve.'] })}>Remove</Button>}
          </Group>
        )}
      </Group>
      <Text size={FS.body}>
        <AnchorLink to="/assets/$assetId" params={{ assetId: l.assetId }} size={FS.body}>{l.hostname}</AnchorLink>
        <Text span ff="monospace" size={FS.body}> ⇄ {l.peer}</Text>
        <Text span c="dimmed" size={FS.meta}> · {l.proposals === 'strong' ? 'Strong ciphers' : 'Compatible ciphers'} · IKEv2</Text>
      </Text>
      <Text size={FS.meta} ff="monospace" c="dimmed">{l.localRanges.join(', ')} ⇄ {l.remoteRanges.join(', ')}</Text>
      {l.cloud && (
        <Text size={FS.meta} c={l.cloud.up ? 'dimmed' : 'rose'} data-testid="ipsec-cloud">
          The other end is {l.cloud.name} in {PROVIDER[l.cloud.provider] ?? l.cloud.provider} {l.cloud.accountName} ({l.cloud.region}): the cloud says this tunnel is {l.cloud.up ? 'up' : `down${l.cloud.why ? ` (${l.cloud.why})` : ''}`}.
        </Text>
      )}
      {l.server && (l.server === 'failed' || l.server === 'rolled-back') && l.serverMessage && <Text size={FS.meta} c="rose">{l.serverMessage}</Text>}
      {p && (
        <Alert color="amber" p="xs" data-testid="ipsec-pending">
          <Group justify="space-between" wrap="wrap" gap="xs">
            <Text size={FS.meta}>
              {l.pendingBy} asks to {what}{l.pendingAt ? `, ${relTime(l.pendingAt)}` : ''}. Nothing changes on the server until another admin approves.
              {!p.remove && <><br />{p.hostname} ⇄ {p.peer} · {p.localRanges.join(', ')} ⇄ {p.remoteRanges.join(', ')} · {p.proposals}</>}
            </Text>
            {canManage && (
              <Group gap="xs">
                {mine && !l.approveOwn
                  ? <Tooltip label="Another admin approves it" withArrow><Button size="compact-xs" disabled leftSection={<IconCheck size={12} />}>Approve</Button></Tooltip>
                  : mine
                  ? <ApproveOwn what="this link change" loading={approve.isPending}
                    onApprove={(c) => run(() => approve.mutateAsync(c), { failure: 'Not approved', success: ['Approved', 'The server applies it on its next heartbeat.'] })} />
                  : <Button size="compact-xs" color="teal" leftSection={<IconCheck size={12} />} loading={approve.isPending}
                    onClick={() => void run(() => approve.mutateAsync(undefined), { failure: 'Not approved', success: ['Approved', 'The server applies it on its next heartbeat.'] })}>Approve</Button>}
                <Button size="compact-xs" variant="default" leftSection={<IconX size={12} />} loading={decline.isPending}
                  onClick={() => void run(() => decline.mutateAsync(), { failure: 'Not declined', success: [mine ? 'Taken back' : 'Declined', 'The link stays as it is.'] })}>{mine ? 'Take back' : 'Decline'}</Button>
              </Group>
            )}
          </Group>
        </Alert>
      )}
      {key && (
        <Modal opened onClose={() => setKey(null)} title={`Pre-shared key for ${l.name}`}>
          <Stack gap="sm">
            <Text size={FS.body}>Enter it as the tunnel's pre-shared key in the cloud or on the office firewall. Showing it was recorded in Activity.</Text>
            <Group gap="xs" wrap="nowrap">
              <Text ff="monospace" size={FS.body} style={{ wordBreak: 'break-all' }} data-testid="ipsec-key">{key}</Text>
              <CopyButton value={key}>{({ copied, copy }) => (
                <Button size="compact-xs" variant="default" leftSection={copied ? <IconCheck size={12} /> : <IconCopy size={12} />} onClick={copy}>{copied ? 'Copied' : 'Copy'}</Button>
              )}</CopyButton>
            </Group>
          </Stack>
        </Modal>
      )}
    </Stack>
  )
}

function LinkEditor({ l, onClose }: { l: IPsecLink | null; onClose: () => void }) {
  const qc = useQueryClient()
  const { data: assets } = useQuery(assetsQuery())
  const servers = useMemo(() => (assets ?? []).filter((a) => !a.protocol || a.protocol === 'ssh'), [assets])
  const [name, setName] = useState(l?.name ?? '')
  const [assetId, setAssetId] = useState<string | null>(l?.assetId ?? null)
  const [peer, setPeer] = useState(l?.peer ?? '')
  const [local, setLocal] = useState((l?.localRanges ?? []).join(', '))
  const [remote, setRemote] = useState((l?.remoteRanges ?? []).join(', '))
  const [proposals, setProposals] = useState<IPsecProposals>(l?.proposals ?? 'strong')
  const [keyFrom, setKeyFrom] = useState<'keep' | 'argus' | 'paste'>(l ? 'keep' : 'argus')
  const [psk, setPsk] = useState('')
  const [understood, setUnderstood] = useState(false)
  const save = useMutation({
    mutationFn: async (d: IPsecDraft & { psk?: string }) => { if (l) await askIPsecChange(l.id, d); else await askIPsecLink(d) },
    onSuccess: () => qc.invalidateQueries({ queryKey: ['networks'] }),
  })
  const submit = () =>
    void run(() => save.mutateAsync({
      name: name.trim(), assetId: assetId ?? '', peer: peer.trim(), localRanges: ranges(local), remoteRanges: ranges(remote), proposals,
      newKey: keyFrom !== 'keep', psk: keyFrom === 'paste' ? psk.trim() : undefined,
    }), { failure: 'Not asked', success: ['Asked', 'It waits for another admin to approve. Nothing changes on the server before then.'] }).then((ok) => { if (ok) onClose() })
  return (
    <Modal opened onClose={onClose} title={l ? `Change ${l.name}` : 'New IPsec link'} size="lg">
      <Stack gap="md">
        <Alert color="sky" p="xs">
          <Text size={FS.meta}>
            First make the VPN connection in the cloud, with this server's public address as the customer or peer gateway and static routing to the
            ranges on this side. In AWS that is a customer gateway and a Site-to-Site VPN connection; in Azure a local network gateway and a
            connection; in Google Cloud a peer VPN gateway and a tunnel. Then give its tunnel address below. A cloud VPN in a connected account is
            matched by that address, so its tunnel state shows here.
          </Text>
        </Alert>
        <TextInput label="Name" placeholder="Jakarta office to AWS" value={name} onChange={(e) => setName(e.currentTarget.value)} maxLength={80} />
        <Select label="This side's gateway" description="The Argus server strongSwan runs on" searchable value={assetId} onChange={setAssetId}
          data={servers.map((s) => ({ value: s.id, label: s.hostname }))} />
        <TextInput label="Other end" description="The cloud tunnel's outside address, or the office firewall's public address" placeholder="52.1.2.3"
          value={peer} onChange={(e) => setPeer(e.currentTarget.value)} styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)' } }} />
        <Group grow align="flex-start">
          <TextInput label="Ranges on this side" placeholder="192.168.10.0/24" value={local} onChange={(e) => setLocal(e.currentTarget.value)} />
          <TextInput label="Ranges on the other side" placeholder="10.30.0.0/16" value={remote} onChange={(e) => setRemote(e.currentTarget.value)} />
        </Group>
        <Stack gap={4}>
          <Text size={FS.body} fw={500}>Ciphers</Text>
          <SegmentedControl value={proposals} onChange={(v) => setProposals(v as IPsecProposals)} w={260}
            data={[{ value: 'strong', label: 'Strong' }, { value: 'compatible', label: 'Compatible' }]} />
          <Text size={FS.meta} c="dimmed">{CIPHERS[proposals]}</Text>
        </Stack>
        <Radio.Group label="Pre-shared key" value={keyFrom} onChange={(v) => setKeyFrom(v as typeof keyFrom)}>
          <Stack gap={6} mt={4}>
            {l && <Radio value="keep" label="Keep the current key" />}
            <Radio value="argus" label="Argus makes one; you copy it into the cloud with Show key" />
            <Radio value="paste" label="Use the key the cloud made" />
          </Stack>
        </Radio.Group>
        {keyFrom === 'paste' && (
          <PasswordInput label="Key" description="16 to 64 letters, digits, dots and underscores. Kept sealed; never shown in lists." value={psk}
            onChange={(e) => setPsk(e.currentTarget.value)} autoComplete="off" />
        )}
        <Checkbox checked={understood} onChange={(e) => setUnderstood(e.currentTarget.checked)}
          label="I understand the server applies this on a 2-minute trial and undoes it if it can no longer reach Argus" />
        <Text size={FS.meta} c="dimmed"><AloneOr kind="runs" what="this link">Another admin approves it before the server changes.</AloneOr></Text>
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button onClick={submit} loading={save.isPending}
            disabled={!name.trim() || !assetId || !peer.trim() || !local.trim() || !remote.trim() || (keyFrom === 'paste' && !psk.trim()) || !understood}>
            Ask for approval
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}
