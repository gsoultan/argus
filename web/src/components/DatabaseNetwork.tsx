import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import {
  ActionIcon, Alert, Badge, Box, Button, Checkbox, Code, Group, Loader, Radio, ScrollArea, SimpleGrid, Stack, Table, Text, TextInput,
} from '@mantine/core'
import { IconAlertTriangle, IconRefresh } from '@tabler/icons-react'
import { FS, SP } from '~/theme'
import { AdminRefused, NeedsDBA, PanelFrame, type AdminFailure, type AdminPanelProps } from '~/components/DatabaseAdmin'
import { ClientTest, Copy, PlanSteps, SourcePicker, knownServers, plural } from '~/components/network/ReachParts'
import { ADDRESS_STATE, REACH, listenFor, privateNetworks, type Reach } from '~/lib/redisManage'
import { pgNowText, pgReachPlan, type PGNetworkData, type PGNetworkResult } from '~/lib/pgNetwork'
import { mysqlNowText, mysqlReachPlan, type MySQLNetworkData } from '~/lib/mysqlNetwork'
import { mongoNowText, mongoReachPlan, type MongoNetworkData } from '~/lib/mongoNetwork'

type NetData = PGNetworkData | MySQLNetworkData | MongoNetworkData
export type NetworkEngine = 'postgres' | 'mysql' | 'mongodb'
import { assetsQuery } from '~/lib/queries'
import { isConfigured } from '~/lib/live'

/*
Which machines can connect to this Postgres or MySQL: this server only,
selected machines, the private network, or anywhere. Postgres decides with
listen_addresses (which addresses it accepts connections on; a change takes
a restart) and pg_hba.conf (who may sign in, and how); the server's firewall
decides who reaches the port at all. The agent writes all three, and only
its own pg_hba.conf lines and firewall rules. The plan beside the choice is
what the agent will run; the agent refuses on its own what the plan calls
blocked. Opening to anywhere always waits for a second person, needs the
server's name typed, and no pg_hba.conf line that asks for no password.

MySQL and MariaDB decide with bind_address, read only when mysqld starts,
and each user's host part, which Argus shows and leaves alone; opening to
anywhere is refused while a user may sign in from elsewhere with no password.

MongoDB decides with net.bindIp in mongod's configuration file, also read
only at start; Argus adds only a MongoDB whose access control is on, so
every machine still signs in as a user.
*/

export function NetworkPanel({ admin, level, narrow, onBack, onAskForAccess, assetId, engine = 'postgres' }:
  Pick<AdminPanelProps, 'admin' | 'level' | 'narrow' | 'onBack' | 'onAskForAccess' | 'assetId'> & { engine?: NetworkEngine }) {
  const [n, setN] = useState<NetData | null>(null)
  const [failure, setFailure] = useState<AdminFailure | null>(null)
  const [busy, setBusy] = useState(false)
  const [reach, setReach] = useState<Reach>('local')
  const [sources, setSources] = useState<string[]>([])
  const [confirmName, setConfirmName] = useState('')
  const [understood, setUnderstood] = useState(false)
  const [restart, setRestart] = useState(true)
  const [note, setNote] = useState<{ ok: boolean; text: string; ran?: string[] } | null>(null)
  const alive = useRef(true)
  useEffect(() => { alive.current = true; return () => { alive.current = false } }, [])
  const { data: assets } = useQuery({ ...assetsQuery(), enabled: isConfigured() })
  const servers = useMemo(() => knownServers(assets, assetId ?? ''), [assets, assetId])

  const reset = useCallback((x: NetData) => {
    setReach(x.reach)
    setSources(x.reach === 'selected' ? x.firewallSources ?? [] : [])
    setConfirmName('')
    setUnderstood(false)
  }, [])
  const got = useCallback((x: NetData) => { setN(x); reset(x) }, [reset])
  const refresh = useCallback(async () => {
    setBusy(true)
    const r = await admin<NetData>({ kind: 'network' })
    if (!alive.current) return
    if (r.error) setFailure(r.error)
    else if (r.data) { setFailure(null); got(r.data) }
    setBusy(false)
  }, [admin, got])
  useEffect(() => { if (level === 'dba') void refresh() }, [refresh, level])

  const blurb = engine === 'mysql'
    ? 'Which machines can connect: the addresses MySQL listens on, and the server’s firewall.'
    : engine === 'mongodb'
      ? 'Which machines can connect: the addresses MongoDB listens on, and the server’s firewall.'
      : 'Which machines can connect: the addresses Postgres listens on, pg_hba.conf, and the server’s firewall.'
  if (level !== 'dba') {
    return (
      <PanelFrame title="Network" narrow={narrow} onBack={onBack} blurb={blurb}>
        <NeedsDBA level={level} what="Changing which machines can connect" onAskForAccess={onAskForAccess} />
      </PanelFrame>
    )
  }
  const frame = (body: ReactNode) => (
    <PanelFrame title="Network" narrow={narrow} onBack={onBack} blurb={blurb}
      right={<ActionIcon variant="subtle" onClick={() => void refresh()} loading={busy} aria-label="Refresh"><IconRefresh size={16} /></ActionIcon>}>
      <ScrollArea style={{ flex: 1 }} type="auto">{body}</ScrollArea>
    </PanelFrame>
  )
  if (failure && !n) return frame(<Box p="md"><AdminRefused error={failure} /></Box>)
  if (!n) return frame(<Loader size={16} m="md" />)

  const host = n.host || 'this server'
  const nets = privateNetworks(n)
  const sent = reach === 'network' ? nets : reach === 'selected' ? sources : []
  const listen = listenFor(n, reach, sent)
  const my = engine === 'mysql'
  const mongo = engine === 'mongodb'
  const name = my ? ((n as MySQLNetworkData).mariadb ? 'MariaDB' : 'MySQL') : mongo ? 'MongoDB' : 'Postgres'
  const plan = my ? mysqlReachPlan(n as MySQLNetworkData, reach, sources, restart)
    : mongo ? mongoReachPlan(n as MongoNetworkData, reach, sources, restart)
    : pgReachPlan(n as PGNetworkData, reach, sources, restart)
  const same = reach === n.reach && !n.pendingRestart &&
    (reach !== 'selected' || [...sources].sort().join() === [...(n.firewallSources ?? [])].sort().join())
  const ready = !same && !plan.blocked &&
    (reach !== 'any' || confirmName.trim().toLowerCase() === host.toLowerCase()) &&
    (reach !== 'network' || understood)
  const now = my ? mysqlNowText(n as MySQLNetworkData) : mongo ? mongoNowText(n as MongoNetworkData) : pgNowText(n as PGNetworkData)
  const tls = mongo ? (n as MongoNetworkData).tls : (n as PGNetworkData | MySQLNetworkData).ssl
  const publicIPs = n.addresses.filter((a) => !a.private).map((a) => a.ip)

  const apply = async () => {
    setBusy(true)
    const r = await admin<PGNetworkResult>({ kind: 'network.set', reach, addresses: reach === 'selected' || reach === 'network' ? listen : [],
      sources: sent, restart, confirm: reach === 'any' ? confirmName.trim() : undefined })
    setBusy(false)
    if (r.error || !r.data) {
      setNote({ ok: false, text: r.error?.error ?? 'failed' })
      return
    }
    got(r.data.network)
    const after = r.data.restarted ? ` ${name} was restarted and listens anew.`
      : r.data.restartError ? ` ${name} was not restarted: ${r.data.restartError}.`
      : r.data.network.pendingRestart ? ` It takes effect when ${name} is restarted.` : ''
    setNote({ ok: !r.data.restartError, ran: r.data.firewallRan, text: `Now: ${REACH[reach].label.toLowerCase()}.${after}` })
  }

  const option = (r: Reach, desc: string, extra?: ReactNode) => (
    <Box key={r}>
      <Radio.Card value={r} className="argus-pick" data-checked={reach === r || undefined}
        style={reach === r && r === 'any' ? { borderColor: 'var(--mantine-color-rose-4)' } : undefined}>
        <Group gap="sm" wrap="nowrap" align="flex-start" p={SP.cozy}>
          <Radio.Indicator mt={2} />
          <Box style={{ flex: 1, minWidth: 0 }}>
            <Group gap={SP.snug}>
              <Text size="sm" fw={600}>{r === 'network' && nets.length ? `The private network ${nets.join(', ')}` : REACH[r].label}</Text>
              {n.reach === r && <Badge size="xs" variant="light" color="slate">now</Badge>}
              {r === 'any' && <Badge size="xs" variant="light" color="rose">risky</Badge>}
            </Group>
            <Text size={FS.meta} c="dimmed" lh={1.4}>{desc}</Text>
          </Box>
        </Group>
      </Radio.Card>
      {reach === r && extra && <Box pl={34} pr={SP.cozy} pt={SP.snug}>{extra}</Box>}
    </Box>
  )

  return frame(
    <Stack gap={SP.cozy} p="md">
      <Alert color={now.tone} icon={n.reach === 'local' || n.reach === 'any' ? <IconAlertTriangle size={16} /> : undefined}>
        <Text size={FS.meta}>{now.text}</Text>
      </Alert>
      {note && (
        <Alert color={note.ok ? 'teal' : 'rose'} withCloseButton onClose={() => setNote(null)}>
          <Text size={FS.meta}>{note.text}</Text>
          {note.ran && note.ran.length > 0 && <Code block mt={4} style={{ fontSize: 11 }}>{note.ran.join('\n')}</Code>}
        </Alert>
      )}
      <Text size={FS.micro} c="dimmed">
        {my ? `${name} ${(n as MySQLNetworkData).version} · port ${n.port}`
          : mongo ? `MongoDB ${(n as MongoNetworkData).version} · port ${n.port}${(n as MongoNetworkData).configFile ? ` · ${(n as MongoNetworkData).configFile}` : ''}`
          : `Port ${n.port} · ${(n as PGNetworkData).hbaFile} · passwords ${(n as PGNetworkData).method}`} · TLS {tls ? 'on' : 'off'} · firewall: {n.firewall ? `${n.firewall} (${n.firewallActive ? 'on' : 'off'})` : n.firewallError ? 'could not be read' : 'none'}
        {n.remote > 0 && ` · ${n.remote} ${plural(n.remote, 'connection', 'connections')} from other machines now`}
      </Text>

      <SimpleGrid cols={{ base: 1, md: 2 }} spacing="md">
        <Radio.Group value={reach} onChange={(v) => { setReach(v as Reach); setConfirmName(''); setUnderstood(false) }}
          label={<Text size="sm" fw={600} mb={6}>Which machines can connect to this {name}</Text>}>
          <Stack gap={8}>
            {option('local', `Programs on ${host} only; every other machine is refused.`)}
            {option('selected', `Only the machines you list, through ${my || mongo ? '' : 'pg_hba.conf and '}the firewall on ${host}.`,
              <SourcePicker n={n} sources={sources} setSources={setSources} servers={servers} />)}
            {option('network', `Any machine on the same private network. Each still needs ${my ? 'a user made for its host' : mongo ? 'a MongoDB user' : 'a role'} and password.`,
              <Checkbox size="xs" checked={understood} onChange={(e) => setUnderstood(e.currentTarget.checked)}
                label={`I understand every machine on ${nets.join(', ') || 'these networks'} can reach ${name}`} />)}
            {option('any', 'Every network this server is on, the internet included. Needs a second person’s approval.',
              <Stack gap={4}>
                {!tls && <Text size={FS.micro} c="rose">TLS is off: queries and data cross the network unencrypted. Prefer selected machines or a VPN.</Text>}
                {publicIPs.length > 0 && <Text size={FS.micro} c="rose">{publicIPs.join(', ')} {plural(publicIPs.length, 'is a public address', 'are public addresses')}: bots try port {n.port} within minutes.</Text>}
                <TextInput size="xs" label={<>Type <Code>{host}</Code> to open it to anywhere</>} value={confirmName}
                  onChange={(e) => setConfirmName(e.currentTarget.value)} disabled={!!plan.blocked} aria-label="Type the server's name" />
              </Stack>)}
          </Stack>
        </Radio.Group>

        <Box>
          <Text size="sm" fw={600} mb={6}>What Argus will change on {host}</Text>
          <PlanSteps steps={plan.steps} />
          {n.service && (
            <Checkbox mt={6} size="xs" checked={restart} onChange={(e) => setRestart(e.currentTarget.checked)}
              label={`Restart ${name} when ${my ? 'bind_address' : mongo ? 'bindIp' : 'listen_addresses'} changes (${n.clients} open ${plural(n.clients, 'connection', 'connections')} end)`} />
          )}
          {plan.blocked && !same && <Text size={FS.micro} c="rose" mt={4}>Not yet: {plan.blocked}.</Text>}
          {!same && (
            <Text size={FS.micro} c="dimmed" mt={4}>
              {reach === 'any' ? 'Opening to anywhere always waits for someone else to approve exactly this, on every database.' : 'Where this database’s changes wait for approval, this does too.'} It is recorded with the session.
            </Text>
          )}
          <Group justify="flex-end" mt="sm" gap={6}>
            {!same && <Button size="xs" variant="default" onClick={() => reset(n)}>Discard</Button>}
            <Button size="xs" color={reach === 'any' ? 'rose' : undefined} disabled={!ready} loading={busy} onClick={() => void apply()}>
              {same ? 'Nothing to change' : reach === 'any' ? 'Ask for approval' : 'Apply'}
            </Button>
          </Group>
        </Box>
      </SimpleGrid>

      <Table verticalSpacing={4} fz="xs" withTableBorder mt="xs">
        <Table.Thead>
          <Table.Tr><Table.Th>Address on {host}</Table.Th><Table.Th>From a client there</Table.Th><Table.Th w={40} /></Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {n.addresses.map((a) => (
            <Table.Tr key={a.ip}>
              <Table.Td>
                <Text size="xs" ff="monospace">{a.ip}:{n.port}</Text>
                <Text size={FS.micro} c="dimmed">{a.iface} · {a.prefix}{a.private ? '' : ' · public'}</Text>
              </Table.Td>
              <Table.Td>
                <Badge size="xs" variant="light" color={a.state === 'signin' ? 'teal' : 'slate'}>
                  {a.state === 'signin' ? `${name} answers` : ADDRESS_STATE.closed.label}
                </Badge>
              </Table.Td>
              <Table.Td>{a.state === 'signin' && <Copy value={`${a.ip}:${n.port}`} label="Copy host and port" />}</Table.Td>
            </Table.Tr>
          ))}
          {n.addresses.length === 0 && <Table.Tr><Table.Td colSpan={3}><Text size={FS.meta} c="dimmed">This server has no address besides the loopback.</Text></Table.Td></Table.Tr>}
        </Table.Tbody>
      </Table>
      <Text size={FS.micro} c="dimmed">Checked from {host} itself, which its own firewall does not stop{my || mongo ? '' : ', and before pg_hba.conf is asked'}. Test from the client’s side below.</Text>
      {!my && !mongo && ((n as PGNetworkData).others?.length ?? 0) > 0 && (
        <Box>
          <Text size={FS.meta} fw={600}>The server’s own pg_hba.conf lines for other machines</Text>
          <Text size={FS.micro} c="dimmed">These come before Argus’s and still decide; Argus never changes them.</Text>
          <Code block mt={4} style={{ fontSize: 11 }}>{(n as PGNetworkData).others!.join('\n')}</Code>
        </Box>
      )}
      {n.reach !== 'local' && (
        <ClientTest n={n} engine={engine} sources={n.reach === 'selected' ? n.firewallSources ?? [] : []} servers={servers}
          command={(ip) => my ? `mysql -h ${ip} -P ${n.port} -u <user> -p -e 'select 1'`
            : mongo ? `mongosh "mongodb://<user>@${ip}:${n.port}/admin" --eval 'db.runCommand({ ping: 1 })'`
            : `psql "host=${ip} port=${n.port} dbname=postgres user=<role>" -c 'select 1'`} />
      )}
    </Stack>,
  )
}
