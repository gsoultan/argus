import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import {
  Alert, Badge, Box, Button, Checkbox, Code, Group, Modal, PasswordInput, Radio, SimpleGrid, Stack, Table, Text, TextInput,
} from '@mantine/core'
import { IconAlertTriangle, IconCheck, IconNetwork, IconX } from '@tabler/icons-react'
import { FS, SP } from '~/theme'
import { SectionCard } from '~/components/page'
import type { StoreRequest, StoreSession } from '~/components/useStoreSession'
import {
  ADDRESS_STATE, REACH, connectCommand, listenFor, networkSummary, privateNetworks, reachPlan,
  type Reach, type RedisLoginResult, type RedisNetworkData, type RedisNetworkResult, type RedisSecureResult,
} from '~/lib/redisManage'
import { assetsQuery } from '~/lib/queries'
import { isConfigured } from '~/lib/live'
import { ClientTest, Copy, PlanSteps, SourcePicker, knownServers, plural } from '~/components/network/ReachParts'
import { dataOf, type ChangeAnswer } from '~/components/redis/useRedisChange'

/*
Which machines can connect to this Redis: this server only, selected machines, the
private network, or anywhere. Redis's bind only picks which of the server's
addresses it listens on and cannot name clients, so "selected machines" is
bind plus a firewall rule on the server, and the agent writes both, touching
only the rules Argus added. The plan beside the choice is what the agent will
run; the agent refuses on its own what the plan calls blocked.

Opening to anywhere is never one click: every user needs a password, the
server's name is typed, and a second person approves, on every database.
Where a machine that may connect is itself an Argus server, its agent can
connect for real: the one check that sees the firewall from the client's side.
*/


/** Who can connect now, in one line. */
function nowText(n: RedisNetworkData): { tone: string; text: string } {
  const host = n.host || 'this server'
  const src = n.firewallSources ?? []
  switch (n.reach) {
    case 'local': return { tone: 'amber', text: `Only programs on ${host} can connect. Redis listens on ${n.bind.join(' ')}, so other machines get “connection refused”.` }
    case 'selected': return { tone: 'teal', text: `${src.length} selected ${plural(src.length, 'machine', 'machines')} can connect: ${src.join(', ')}.` }
    case 'network': return { tone: 'azure', text: `Machines on the networks ${host} is on can connect with a user and password.` }
    case 'any': return { tone: 'rose', text: `Redis listens on every address of ${host}${n.firewallActive ? '' : ', and no firewall here narrows it'}.` }
  }
  return networkSummary(n)
}

export function RedisNetwork({ s, canChange, change, assetId, onNetwork }: {
  s: StoreSession
  canChange: boolean
  change: (req: StoreRequest) => Promise<ChangeAnswer>
  /** The server Redis runs on, so it is not offered as a client of itself. */
  assetId?: string
  onNetwork?: (n: RedisNetworkData) => void
}) {
  const [n, setN] = useState<RedisNetworkData | null>(null)
  const [err, setErr] = useState<string | null>(null)
  const [reach, setReach] = useState<Reach>('local')
  const [sources, setSources] = useState<string[]>([])
  const [confirmName, setConfirmName] = useState('')
  const [understood, setUnderstood] = useState(false)
  const [secure, setSecure] = useState(false)
  const [busy, setBusy] = useState(false)
  const [note, setNote] = useState<{ ok: boolean; text: string; ran?: string[] } | null>(null)
  const { data: assets } = useQuery({ ...assetsQuery(), enabled: isConfigured() })
  const servers = useMemo(() => knownServers(assets, assetId ?? ''), [assets, assetId])

  const reset = useCallback((x: RedisNetworkData) => {
    setReach(x.reach ?? 'local')
    setSources(x.reach === 'selected' ? x.firewallSources ?? [] : [])
    setConfirmName('')
    setUnderstood(false)
  }, [])
  const got = useCallback((x: RedisNetworkData) => {
    setN(x)
    reset(x)
    onNetwork?.(x)
  }, [onNetwork, reset])
  const load = useCallback(async () => {
    const a = await s.ask({ op: 'network' })
    const d = dataOf<RedisNetworkData>(a)
    if (!d) setErr(a.error ?? 'could not read the network settings')
    else { setErr(null); got(d) }
  }, [s, got])
  useEffect(() => { if (s.status === 'ready') void load() }, [s.status, load])

  if (!n) {
    return (
      <SectionCard title="Which machines can connect" icon={IconNetwork}>
        <Box px="md" pb="md">{err ? <Text size={FS.meta} c="rose">{err}</Text> : <Text size={FS.meta} c="dimmed">Checking each address and the firewall…</Text>}</Box>
      </SectionCard>
    )
  }

  const host = n.host || 'this server'
  const nets = privateNetworks(n)
  const sent = reach === 'network' ? nets : reach === 'selected' ? sources : []
  const plan = reachPlan(n, reach, sent, s.ready?.version)
  const listen = listenFor(n, reach, sent)
  const current = n.reach ?? 'local'
  const same = reach === current && (reach !== 'selected' || [...sources].sort().join() === [...(n.firewallSources ?? [])].sort().join())
  const waits = reach === 'any' || !!s.ready?.changesNeedApproval
  const ready = canChange && !same && !plan.blocked &&
    (reach !== 'any' || confirmName.trim().toLowerCase() === host.toLowerCase()) &&
    (reach !== 'network' || understood)
  const now = nowText(n)
  const publicIPs = n.addresses.filter((a) => !a.private).map((a) => a.ip)


  const apply = async () => {
    setBusy(true)
    const req: StoreRequest = { op: 'network_set', reach, addresses: listen, sources: sent }
    if (reach === 'any') req.confirm = confirmName.trim()
    const a = await change(req)
    setBusy(false)
    if (a.notApproved) { setNote({ ok: false, text: 'Nothing changed: it was not approved.' }); return }
    const r = dataOf<RedisNetworkResult>(a)
    if (!r) { setNote({ ok: false, text: a.error ?? 'failed' }); void load(); return }
    got(r.network)
    setNote({
      ok: r.rewritten,
      ran: r.firewallRan,
      text: `Now: ${REACH[reach].label.toLowerCase()}.${r.rewritten ? ' Saved in the config file.' : ` It is live, but writing the config file failed: ${r.rewriteError}`}`,
    })
  }
  const secureDefault = async () => {
    setBusy(true)
    const a = await change({ op: 'default_secure' })
    setBusy(false)
    setSecure(false)
    if (a.notApproved) { setNote({ ok: false, text: 'Nothing changed: it was not approved.' }); return }
    const r = dataOf<RedisSecureResult>(a)
    if (!r) { setNote({ ok: false, text: a.error ?? 'failed' }); return }
    got(r.network)
    setNote({ ok: true, text: `The default user now needs a password. It was made on the server and is in ${r.savedTo} (requirepass); it is not shown here.` })
  }

  // The extra controls sit under the card, not inside it: the card is a button.
  const option = (r: Reach, desc: string, extra?: ReactNode) => (
    <Box key={r}>
    <Radio.Card value={r} className="argus-pick" data-checked={reach === r || undefined} disabled={!canChange}
      style={reach === r && r === 'any' ? { borderColor: 'var(--mantine-color-rose-4)' } : undefined}>
      <Group gap="sm" wrap="nowrap" align="flex-start" p={SP.cozy}>
        <Radio.Indicator mt={2} />
        <Box style={{ flex: 1, minWidth: 0 }}>
          <Group gap={SP.snug}>
            <Text size="sm" fw={600}>{r === 'network' && nets.length ? `The private network ${nets.join(', ')}` : REACH[r].label}</Text>
            {current === r && <Badge size="xs" variant="light" color="slate">now</Badge>}
            {r === 'any' && <Badge size="xs" variant="light" color="rose">risky</Badge>}
          </Group>
          <Text size={FS.meta} c="dimmed" lh={1.4}>{desc}</Text>
        </Box>
      </Group>
    </Radio.Card>
    {reach === r && extra && <Box pl={34} pr={SP.cozy} pt={SP.snug}>{extra}</Box>}
    </Box>
  )

  const picker = <SourcePicker n={n} sources={sources} setSources={setSources} servers={servers} />

  const firewallNote = n.firewall
    ? `${n.firewall} (${n.firewallActive ? 'on' : 'off'})`
    : n.firewallError ? 'could not be read' : 'none'

  return (
    <SectionCard title="Which machines can connect" icon={IconNetwork}
      description={`Port ${n.port}${n.tlsPort ? ` (TLS ${n.tlsPort})` : ''} · protected mode ${n.protectedMode ? 'on' : 'off'} · firewall: ${firewallNote}`}>
      <Stack gap={SP.cozy} px="md" pb="md">
        <Alert color={now.tone} icon={current === 'local' || current === 'any' ? <IconAlertTriangle size={16} /> : undefined}>
          <Text size={FS.meta}>{now.text}</Text>
        </Alert>
        {note && (
          <Alert color={note.ok ? 'teal' : 'rose'} withCloseButton onClose={() => setNote(null)}>
            <Text size={FS.meta}>{note.text}</Text>
            {note.ran && note.ran.length > 0 && <Code block mt={4} style={{ fontSize: 11 }}>{note.ran.join('\n')}</Code>}
          </Alert>
        )}
        {n.defaultOpen && (
          <Alert color="amber" icon={<IconAlertTriangle size={16} />} title="The default user has no password">
            <Text size={FS.meta}>
              While it has none, anyone who reaches Redis is admin, and protected mode refuses every client that is not on this server.
              Give it a password before letting other machines connect.
              {n.defaultClients > 0 && ` ${n.defaultClients} ${plural(n.defaultClients, 'connection is', 'connections are')} signed in as default now and keep working until they reconnect, then need the password.`}
            </Text>
            {canChange && (
              n.agentConf
                ? <Button mt={6} size="compact-xs" variant="default" onClick={() => setSecure(true)}>Give the default user a password</Button>
                : <Text size={FS.micro} mt={4}>Argus can’t do this here: Redis rewrites {n.configFile || 'no config file'}, but the agent signs in with a different file. Set requirepass on the server.</Text>
            )}
          </Alert>
        )}

        <SimpleGrid cols={{ base: 1, md: 2 }} spacing="md">
          <Radio.Group value={reach} onChange={(v) => { setReach(v as Reach); setConfirmName(''); setUnderstood(false) }}
            label={<Text size="sm" fw={600} mb={6}>Which machines can connect to this Redis</Text>}>
            <Stack gap={8}>
              {option('local', `Programs on ${host} only; every other machine is refused.`)}
              {option('selected', `Only the machines you list, through the firewall on ${host}.`, picker)}
              {option('network', 'Any machine on the same private network. Each still needs a user and password.',
                <Checkbox size="xs" checked={understood} onChange={(e) => setUnderstood(e.currentTarget.checked)}
                  label={`I understand every machine on ${nets.join(', ') || 'these networks'} can reach Redis`} />)}
              {option('any', 'Every network this server is on, the internet included. Needs a password on every user and a second person’s approval.',
                <Stack gap={4}>
                  <Text size={FS.micro} c="rose">With no TLS, passwords and data cross the network unencrypted. Prefer selected machines or a VPN.</Text>
                  {publicIPs.length > 0 && <Text size={FS.micro} c="rose">{publicIPs.join(', ')} {plural(publicIPs.length, 'is a public address', 'are public addresses')}: bots scan port {n.port} within minutes.</Text>}
                  <TextInput size="xs" label={<>Type <Code>{host}</Code> to open it to anywhere</>} value={confirmName}
                    onChange={(e) => setConfirmName(e.currentTarget.value)} disabled={!!plan.blocked} aria-label="Type the server's name" />
                </Stack>)}
            </Stack>
          </Radio.Group>

          <Box>
            <Text size="sm" fw={600} mb={6}>What Argus will change on {host}</Text>
            <PlanSteps steps={plan.steps} />
            {plan.blocked && !same && <Text size={FS.micro} c="rose" mt={4}>Not yet: {plan.blocked}.</Text>}
            {waits && !same && (
              <Text size={FS.micro} c="dimmed" mt={4}>
                {reach === 'any' ? 'Opening to anywhere always waits for someone else to approve exactly this, on every database.' : 'This database’s changes wait for someone else to approve exactly this.'} It is recorded with the session.
              </Text>
            )}
            {canChange && (
              <Group justify="flex-end" mt="sm" gap={6}>
                {!same && <Button size="xs" variant="default" onClick={() => reset(n)}>Discard</Button>}
                <Button size="xs" color={reach === 'any' ? 'rose' : undefined} disabled={!ready} loading={busy} onClick={() => void apply()}>
                  {same ? 'Nothing to change' : waits ? 'Ask for approval' : 'Apply'}
                </Button>
              </Group>
            )}
          </Box>
        </SimpleGrid>

        <Table verticalSpacing={4} fz="xs" withTableBorder mt="xs">
          <Table.Thead>
            <Table.Tr><Table.Th>Address on {host}</Table.Th><Table.Th>From a client there</Table.Th><Table.Th w={40} /></Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {n.addresses.map((a) => {
              const st = ADDRESS_STATE[a.state]
              return (
                <Table.Tr key={a.ip}>
                  <Table.Td>
                    <Text size="xs" ff="monospace">{a.ip}:{n.port}</Text>
                    <Text size={FS.micro} c="dimmed">{a.iface} · {a.prefix}{a.private ? '' : ' · public'}</Text>
                  </Table.Td>
                  <Table.Td>
                    <Badge size="xs" variant="light" color={st.tone}>{st.label}</Badge>
                    {a.detail && a.state !== 'closed' && <Text size={FS.micro} c="dimmed">{a.detail}</Text>}
                  </Table.Td>
                  <Table.Td>{a.state === 'signin' && <Copy value={`${a.ip}:${n.port}`} label="Copy host and port" />}</Table.Td>
                </Table.Tr>
              )
            })}
            {n.addresses.length === 0 && <Table.Tr><Table.Td colSpan={3}><Text size={FS.meta} c="dimmed">This server has no address besides the loopback.</Text></Table.Td></Table.Tr>}
          </Table.Tbody>
        </Table>
        <Text size={FS.micro} c="dimmed">Checked from {host} itself, which its own firewall does not stop. Test from the client’s side below.</Text>

        {current !== 'local' && <ClientTest n={n} engine="redis" sources={current === 'selected' ? n.firewallSources ?? [] : []} servers={servers}
          command={(ip) => `redis-cli -h ${ip} -p ${n.port} --user <user> --askpass PING`} />}
      </Stack>

      <Modal opened={secure} onClose={() => setSecure(false)} title="Give the default user a password" size="md">
        <Stack gap="sm">
          <Text size={FS.meta}>The agent makes a long random password on the server and writes it to {n.configFile} as requirepass, where the agent reads its own sign-in. It never leaves the server and is not shown here.</Text>
          {n.defaultClients > 0 && <Alert color="amber">{n.defaultClients} {plural(n.defaultClients, 'connection is', 'connections are')} signed in as default with no password. Any application that does that must move to its own user, or use the new password from {n.configFile}, before it reconnects.</Alert>}
          <Code block>{'CONFIG SET requirepass <made on the server>\nCONFIG REWRITE'}</Code>
          <Group justify="flex-end">
            <Button variant="default" size="xs" onClick={() => setSecure(false)}>Cancel</Button>
            <Button size="xs" loading={busy} onClick={() => void secureDefault()}>Set a password</Button>
          </Group>
        </Stack>
      </Modal>
    </SectionCard>
  )
}

/** The result of signing in as a user on each address, with how to connect. */
export function LoginResult({ r, onClose }: { r: RedisLoginResult; onClose?: () => void }) {
  const lan = r.logins.filter((l) => !l.address.startsWith('127.0.0.1:'))
  const okLan = lan.filter((l) => l.ok)
  const local = r.logins.find((l) => l.address.startsWith('127.0.0.1:'))
  const color = okLan.length ? 'teal' : local?.ok ? 'amber' : 'rose'
  return (
    <Alert color={color} withCloseButton={!!onClose} onClose={onClose}
      title={okLan.length ? `${r.user} signs in from the LAN` : local?.ok ? `${r.user} signs in on this server only` : `${r.user} could not sign in`}>
      <Stack gap={4}>
        {r.logins.map((l) => (
          <Group key={l.address} gap={6} wrap="nowrap" align="flex-start">
            {l.ok ? <IconCheck size={14} color="var(--mantine-color-teal-6)" /> : <IconX size={14} color="var(--mantine-color-rose-6)" />}
            <Text size={FS.micro} ff="monospace" w={170}>{l.address}</Text>
            <Text size={FS.micro} style={{ flex: 1 }}>{l.ok ? 'signed in, PING answered' : l.detail}</Text>
          </Group>
        ))}
        {okLan.length === 0 && local?.ok && <Text size={FS.micro}>The user and password are right. To reach it from other machines, open Redis to the LAN below.</Text>}
        {okLan.map((l) => {
          const host = l.address.slice(0, l.address.lastIndexOf(':')).replace(/^\[|\]$/g, '')
          const cmd = connectCommand(host, r.port, r.user)
          return <Group key={l.address} gap={4} wrap="nowrap"><Code style={{ flex: 1, fontSize: 11.5 }}>{cmd}</Code><Copy value={cmd} label="Copy command" /></Group>
        })}
        {okLan.length > 0 && <Text size={FS.micro} c="dimmed">Checked from this server; if another machine still can’t connect, a firewall between them is blocking the port.</Text>}
      </Stack>
    </Alert>
  )
}

/** Ask for a user's password and check it signs in, on every address. */
export function TestSignIn({ s, user, onClose, onResult }: {
  s: StoreSession; user: string | null; onClose: () => void; onResult: (r: RedisLoginResult) => void
}) {
  const [pw, setPw] = useState('')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  useEffect(() => { setPw(''); setErr(null) }, [user])
  const go = async () => {
    if (!user) return
    setBusy(true)
    const a = await s.ask({ op: 'acl_login', user, password: pw })
    setBusy(false)
    setPw('')
    const r = dataOf<RedisLoginResult>(a)
    if (!r) { setErr(a.error ?? 'failed'); return }
    onResult(r)
    onClose()
  }
  return (
    <Modal opened={user !== null} onClose={onClose} title={`Test sign-in as ${user ?? ''}`} size="sm">
      <Stack gap="sm">
        <PasswordInput label="Password" value={pw} onChange={(e) => setPw(e.currentTarget.value)} autoFocus
          onKeyDown={(e) => { if (e.key === 'Enter' && pw) void go() }} />
        <Text size={FS.micro} c="dimmed">Sent to the agent once to try AUTH on each address Redis listens on, then dropped. The recording names the user, never the password.</Text>
        {err && <Text size={FS.meta} c="rose">{err}</Text>}
        <Group justify="flex-end">
          <Button variant="default" size="xs" onClick={onClose}>Cancel</Button>
          <Button size="xs" disabled={!pw} loading={busy} onClick={() => void go()}>Test</Button>
        </Group>
      </Stack>
    </Modal>
  )
}
