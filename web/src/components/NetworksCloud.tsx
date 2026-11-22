import { Alert, Anchor, Badge, Button, Group, List, Modal, PasswordInput, Stack, Table, Text, TextInput, Tooltip } from '@mantine/core'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { IconBrandCloudflare, IconCloud, IconPlugConnected, IconRefresh, IconTrash } from '@tabler/icons-react'
import { useState } from 'react'
import { AnchorLink } from '~/components/links'
import { DataTable, EmptyState, SectionCard } from '~/components/page'
import { relTime } from '~/components/primitives'
import { run } from '~/lib/notify'
import {
  addCloudflare, CF_LIST, CF_STATUS, CLOUD_VPN_STATE, cloudflareQuery, PROVIDER, removeCloudflare, syncCloudflare,
  type CFList, type NetworkCFAccount, type NetworkCloudVPN,
} from '~/lib/networks'
import { FS, SP } from '~/theme'

/*
 * The parts of Networks read from outside the servers: site-to-site VPNs in the
 * connected cloud accounts, and connected Cloudflare accounts with each
 * tunnel's connectors matched to the servers that run them.
 */

export function CloudVPNsCard({ vpns, loading }: { vpns: NetworkCloudVPN[]; loading: boolean }) {
  return (
    <SectionCard title="Cloud VPNs" icon={IconCloud} iconColor="sky" flush
      description="Site-to-site VPNs in your connected AWS, Azure and Google Cloud accounts, including ones Argus did not make. Read with each cloud sync.">
      <DataTable columns={['Connection', 'State', 'Tunnels', 'Other end', 'Type']} minWidth={820}
        loading={loading} isEmpty={vpns.length === 0}
        empty={<EmptyState icon={IconCloud} title="No cloud VPNs found."
          description={<>VPN connections in your connected cloud accounts show here with each tunnel's state. <AnchorLink to="/cloud" size={FS.body}>Connect a cloud account</AnchorLink> if yours is not there.</>} />}>
        {vpns.map((v) => {
          const st = CLOUD_VPN_STATE[v.state] ?? { label: v.state || 'Not known', color: 'slate' }
          return (
            <Table.Tr key={v.accountId + v.region + v.id} data-testid="cloud-vpn">
              <Table.Td>
                <Text size={FS.body} fw={500}>{v.name}</Text>
                <Text size={FS.meta} c="dimmed">{PROVIDER[v.provider] ?? v.provider} · {v.accountName} · {v.region}</Text>
              </Table.Td>
              <Table.Td><Badge color={st.color} variant={st.color === 'rose' ? 'filled' : 'light'}>{st.label}</Badge></Table.Td>
              <Table.Td>
                {v.tunnelsTotal === 0 ? <Text size={FS.body} c="dimmed">—</Text> : (
                  <Tooltip multiline w={320} label={v.tunnels.map((t) => `${t.Address}: ${t.Up ? 'up' : `down${t.Why ? ` (${t.Why})` : ''}`}`).join('\n')}>
                    <Text size={FS.body} c={v.tunnelsUp < v.tunnelsTotal ? 'rose' : undefined}>{v.tunnelsUp} of {v.tunnelsTotal} up</Text>
                  </Tooltip>
                )}
              </Table.Td>
              <Table.Td>
                <Text size={FS.body} ff="monospace">{v.remoteAddress || '—'}</Text>
                {v.remoteNetworks.length > 0 && <Text size={FS.meta} c="dimmed" ff="monospace">{v.remoteNetworks.slice(0, 3).join(', ')}{v.remoteNetworks.length > 3 ? ` +${v.remoteNetworks.length - 3}` : ''}</Text>}
              </Table.Td>
              <Table.Td><Text size={FS.body}>IPsec{v.type ? ` · ${v.type}` : ''}</Text><Text size={FS.meta} c="dimmed">{v.routing === 'bgp' ? 'BGP routing' : v.routing === 'static' ? 'Static routes' : ''}</Text></Table.Td>
            </Table.Tr>
          )
        })}
      </DataTable>
    </SectionCard>
  )
}

export function CloudflareCard({ accounts, loading, canManage }: { accounts: NetworkCFAccount[]; loading: boolean; canManage: boolean }) {
  const [adding, setAdding] = useState(false)
  return (
    <SectionCard title="Cloudflare account" icon={IconBrandCloudflare} iconColor="amber"
      description="Every tunnel and WARP Connector in your Cloudflare account, as Cloudflare sees it, matched to the servers that run them. Read every 5 minutes with a read-only token."
      action={canManage && <Button size="xs" variant="default" leftSection={<IconPlugConnected size={14} />} onClick={() => setAdding(true)}>Connect an account</Button>}>
      {loading ? null : accounts.length === 0 ? (
        <EmptyState icon={IconBrandCloudflare} title="No Cloudflare account connected."
          description="Your servers' cloudflared already shows above. Connecting the account adds what no server can see: a tunnel running on a machine without an agent, one that stopped weeks ago, one with a single connector." />
      ) : (
        <Stack gap="lg">{accounts.map((a) => <CFAccount key={a.id} a={a} canManage={canManage} />)}</Stack>
      )}
      {adding && <ConnectCloudflare onClose={() => setAdding(false)} />}
    </SectionCard>
  )
}

function CFAccount({ a, canManage }: { a: NetworkCFAccount; canManage: boolean }) {
  const qc = useQueryClient()
  const [removing, setRemoving] = useState(false)
  const sync = useMutation({ mutationFn: () => syncCloudflare(a.id), onSettled: () => qc.invalidateQueries({ queryKey: ['networks'] }) })
  const missing = Object.keys(a.missing ?? {}) as CFList[]
  return (
    <Stack gap={SP.cozy} data-testid="cf-account">
      <Group justify="space-between" wrap="wrap" gap="xs">
        <Group gap="xs">
          <Text size={FS.body} fw={600}>{a.name}</Text>
          <Text size={FS.meta} c="dimmed" ff="monospace">{a.accountId}</Text>
          <Text size={FS.meta} c={a.stale ? 'amber' : 'dimmed'}>{a.syncedAt ? `Read ${relTime(a.syncedAt)}` : 'Not read yet'}</Text>
        </Group>
        {canManage && (
          <Group gap="xs">
            <Button size="xs" variant="subtle" leftSection={<IconRefresh size={14} />} loading={sync.isPending}
              onClick={() => void run(() => sync.mutateAsync(), { failure: 'Could not read the account', success: ['Read', `${a.name} is up to date.`] })}>Read now</Button>
            <Button size="xs" variant="subtle" color="rose" leftSection={<IconTrash size={14} />} onClick={() => setRemoving(true)}>Disconnect</Button>
          </Group>
        )}
      </Group>
      {a.lastError && <Alert color="amber" p="xs"><Text size={FS.meta}>The last read failed: {a.lastError}. Shown as of the last read that worked.</Text></Alert>}
      <DataTable columns={['Tunnel', 'Cloudflare says', 'Connectors', 'Private networks']} minWidth={820} isEmpty={a.tunnels.length === 0}
        empty={<EmptyState icon={IconBrandCloudflare} title="No tunnels in this account." />}>
        {a.tunnels.map((t) => {
          const st = CF_STATUS[t.status] ?? { label: t.status, color: 'slate', hint: '' }
          return (
            <Table.Tr key={t.id} data-testid="cf-account-tunnel">
              <Table.Td>
                <Text size={FS.body} fw={500}>{t.name}</Text>
                <Text size={FS.meta} c="dimmed">{t.kind === 'warp-connector' ? 'WARP Connector' : 'Cloudflare Tunnel'}{t.status !== 'healthy' && t.connsInactiveAt ? ` · last connected ${relTime(t.connsInactiveAt)}` : ''}</Text>
              </Table.Td>
              <Table.Td><Tooltip label={st.hint} multiline w={260}><Badge color={st.color} variant={st.color === 'rose' ? 'filled' : 'light'}>{st.label}</Badge></Tooltip></Table.Td>
              <Table.Td>
                {t.connectors.length === 0 ? <Text size={FS.body} c="dimmed">None running</Text> : (
                  <Stack gap={2}>
                    {t.connectors.map((c) => (
                      <Group key={c.clientId} gap={6} wrap="nowrap">
                        {c.assetId && c.hostname
                          ? <AnchorLink to="/assets/$assetId" params={{ assetId: c.assetId }} size={FS.body}>{c.hostname}</AnchorLink>
                          : <Tooltip label="No Argus server reports this connector." withArrow><Badge color="amber" variant="light">Unknown machine {c.originIp ?? ''}</Badge></Tooltip>}
                        <Text size={FS.meta} c="dimmed">{c.connections} conn · {c.locations.join(', ')}</Text>
                        {c.version && <Text size={FS.meta} c={c.old ? 'amber' : 'dimmed'}>{c.version}{c.old ? ' (old)' : ''}</Text>}
                      </Group>
                    ))}
                  </Stack>
                )}
              </Table.Td>
              <Table.Td><Text size={FS.meta} ff="monospace">{t.routes.join(', ') || '—'}</Text></Table.Td>
            </Table.Tr>
          )
        })}
      </DataTable>
      <Text size={FS.meta} c="dimmed">
        {a.read.includes('devices') ? `${a.devicesTotal} ${a.devicesTotal === 1 ? 'device' : 'devices'} enrolled in WARP.` : ''}
        {a.read.includes('magicWan') && a.magicWan.length > 0 ? ` Magic WAN: ${a.magicWan.map((m) => `${m.name} (${m.kind.toUpperCase()} to ${m.customerEndpoint})`).join(', ')}.` : ''}
        {missing.length > 0 ? ` The token cannot read ${missing.map((m) => CF_LIST[m]).join(', ')}, so those are not shown.` : ''}
      </Text>
      {removing && <DisconnectCloudflare a={a} onClose={() => setRemoving(false)} />}
    </Stack>
  )
}

function ConnectCloudflare({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient()
  const { data: v } = useQuery(cloudflareQuery())
  const [name, setName] = useState('')
  const [accountId, setAccountId] = useState('')
  const [token, setToken] = useState('')
  const add = useMutation({ mutationFn: addCloudflare, onSuccess: () => qc.invalidateQueries({ queryKey: ['networks'] }) })
  const save = () =>
    void run(() => add.mutateAsync({ name: name.trim(), accountId: accountId.trim().toLowerCase(), token: token.trim() }), {
      failure: 'Account not connected', success: ['Connected', `${name.trim()} is read every 5 minutes.`],
    }).then((ok) => { if (ok) onClose() })
  return (
    <Modal opened onClose={onClose} title="Connect a Cloudflare account" size="lg">
      <Stack gap="md">
        <Text size={FS.body}>Argus only reads. It never creates, changes or deletes anything in Cloudflare.</Text>
        <List size="sm" spacing={4}>
          <List.Item>In Cloudflare, open <Text span fw={600}>Manage account › Account API tokens › Create token › Custom token</Text>.</List.Item>
          <List.Item>Give it <Text span fw={600}>Cloudflare Tunnel: Read</Text>. Add <Text span fw={600}>Zero Trust: Read</Text> for routes and WARP devices, and <Text span fw={600}>Magic WAN: Read</Text> if you use Magic WAN. Nothing with Edit.</List.Item>
          <List.Item>The account id is in the address bar after <Text span ff="monospace">dash.cloudflare.com/</Text>.</List.Item>
        </List>
        <TextInput label="Name" placeholder="Main account" value={name} onChange={(e) => setName(e.currentTarget.value)} maxLength={80} />
        <TextInput label="Account id" placeholder="32 characters" value={accountId} onChange={(e) => setAccountId(e.currentTarget.value)}
          styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)' } }} />
        <PasswordInput label="API token" description="Argus reads the account with it before saving. It is kept sealed and never shown again."
          value={token} onChange={(e) => setToken(e.currentTarget.value)} autoComplete="off" />
        {v && !v.canSave && <Alert color="amber"><Text size={FS.meta}>{v.cannotSave}</Text></Alert>}
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button onClick={save} loading={add.isPending} disabled={!name.trim() || !accountId.trim() || !token.trim() || (v ? !v.canSave : false)}>Connect</Button>
        </Group>
        <Text size={FS.meta} c="dimmed">Not sure which permissions you gave it? Argus says which lists the token cannot read. <Anchor size={FS.meta} href="https://developers.cloudflare.com/fundamentals/api/get-started/account-owned-tokens/" target="_blank" rel="noreferrer">Cloudflare's guide</Anchor></Text>
      </Stack>
    </Modal>
  )
}

function DisconnectCloudflare({ a, onClose }: { a: NetworkCFAccount; onClose: () => void }) {
  const qc = useQueryClient()
  const remove = useMutation({ mutationFn: () => removeCloudflare(a.id), onSuccess: () => qc.invalidateQueries({ queryKey: ['networks'] }) })
  return (
    <Modal opened onClose={onClose} title={`Disconnect ${a.name}?`}>
      <Stack gap="md">
        <Text size={FS.body}>Argus stops reading it and deletes the token it kept. Nothing in Cloudflare changes. Revoke the token in Cloudflare as well.</Text>
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button color="rose" loading={remove.isPending}
            onClick={() => void run(() => remove.mutateAsync(), { failure: 'Not disconnected', success: ['Disconnected', `${a.name} is no longer read.`] }).then((ok) => { if (ok) onClose() })}>
            Disconnect
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}
