import { useCallback, useEffect, useState } from 'react'
import {
  ActionIcon, Alert, Badge, Box, Button, Checkbox, Code, CopyButton, Group, Modal, PasswordInput, Stack, Table, Text, Tooltip,
} from '@mantine/core'
import { IconAlertTriangle, IconCheck, IconCopy, IconNetwork, IconX } from '@tabler/icons-react'
import { FS, SP } from '~/theme'
import { SectionCard } from '~/components/page'
import type { StoreRequest, StoreSession } from '~/components/useStoreSession'
import {
  ADDRESS_STATE, connectCommand, firewallCommand, networkSummary,
  type RedisLoginResult, type RedisNetworkData, type RedisNetworkResult, type RedisSecureResult,
} from '~/lib/redisManage'
import { dataOf, type ChangeAnswer } from '~/components/redis/useRedisChange'

/*
Whether applications elsewhere on the LAN can reach this Redis, and the two
things that stop them as Redis is installed: it listens on the loopback only,
and protected mode refuses everyone off this machine while the default user
has no password. Opening it is a choice made here on purpose: private
addresses only, the default user must have a password first, and it waits for
approval where this database's changes do. A firewall on the server is the
one thing this cannot see past, so its command is shown to copy.
*/

function Copy({ value, label }: { value: string; label: string }) {
  return (
    <CopyButton value={value}>
      {({ copied, copy }) => (
        <Tooltip label={copied ? 'Copied' : label}>
          <ActionIcon variant="subtle" size="sm" onClick={copy} aria-label={label}>{copied ? <IconCheck size={12} /> : <IconCopy size={12} />}</ActionIcon>
        </Tooltip>
      )}
    </CopyButton>
  )
}

export function RedisNetwork({ s, canChange, change, onNetwork }: {
  s: StoreSession
  canChange: boolean
  change: (req: StoreRequest) => Promise<ChangeAnswer>
  onNetwork?: (n: RedisNetworkData) => void
}) {
  const [n, setN] = useState<RedisNetworkData | null>(null)
  const [err, setErr] = useState<string | null>(null)
  const [want, setWant] = useState<string[]>([])
  const [confirm, setConfirm] = useState<'open' | 'secure' | null>(null)
  const [understood, setUnderstood] = useState(false)
  const [busy, setBusy] = useState(false)
  const [note, setNote] = useState<{ ok: boolean; text: string } | null>(null)

  const got = useCallback((x: RedisNetworkData) => {
    setN(x)
    setWant(x.addresses.filter((a) => a.state !== 'closed' && a.private).map((a) => a.ip))
    onNetwork?.(x)
  }, [onNetwork])
  const load = useCallback(async () => {
    const a = await s.ask({ op: 'network' })
    const d = dataOf<RedisNetworkData>(a)
    if (!d) setErr(a.error ?? 'could not read the network settings')
    else { setErr(null); got(d) }
  }, [s, got])
  useEffect(() => { if (s.status === 'ready') void load() }, [s.status, load])

  if (!n) {
    return (
      <SectionCard title="Reach from the network" icon={IconNetwork}>
        <Box px="md" pb="md">{err ? <Text size={FS.meta} c="rose">{err}</Text> : <Text size={FS.meta} c="dimmed">Checking each address…</Text>}</Box>
      </SectionCard>
    )
  }

  const listening = n.addresses.filter((a) => a.state !== 'closed').map((a) => a.ip)
  const dirty = [...want].sort().join() !== [...listening.filter((ip) => n.addresses.find((a) => a.ip === ip)?.private)].sort().join()
  const sum = networkSummary(n)

  const apply = async () => {
    setBusy(true)
    const a = await change({ op: 'network_set', addresses: want })
    setBusy(false)
    setConfirm(null)
    setUnderstood(false)
    if (a.notApproved) { setNote({ ok: false, text: 'Nothing changed: it was not approved.' }); return }
    const r = dataOf<RedisNetworkResult>(a)
    if (!r) { setNote({ ok: false, text: a.error ?? 'failed' }); return }
    got(r.network)
    setNote({ ok: r.rewritten, text: want.length
      ? `Redis now listens on ${want.join(', ')} as well as this server.${r.rewritten ? ' Saved in redis.conf.' : ` It is live, but writing redis.conf failed: ${r.rewriteError}`}`
      : `Redis listens on this server only again.${r.rewritten ? ' Saved in redis.conf.' : ` Writing redis.conf failed: ${r.rewriteError}`}` })
  }
  const secure = async () => {
    setBusy(true)
    const a = await change({ op: 'default_secure' })
    setBusy(false)
    setConfirm(null)
    if (a.notApproved) { setNote({ ok: false, text: 'Nothing changed: it was not approved.' }); return }
    const r = dataOf<RedisSecureResult>(a)
    if (!r) { setNote({ ok: false, text: a.error ?? 'failed' }); return }
    got(r.network)
    setNote({ ok: true, text: `The default user now needs a password. It was made on the server and is in ${r.savedTo} (requirepass); it is not shown here.` })
  }

  const fwLines = n.firewall ? [...new Set(n.addresses.filter((a) => a.state === 'signin').map((a) => a.prefix))]
    .map((p) => firewallCommand(n.firewall, p, n.port)).filter((x): x is string => !!x) : []

  return (
    <SectionCard title="Reach from the network" icon={IconNetwork}
      description={`Port ${n.port}${n.tlsPort ? ` (TLS ${n.tlsPort})` : ''} · bind ${n.bind.join(' ')} · protected mode ${n.protectedMode ? 'on' : 'off'}`}>
      <Stack gap={SP.cozy} px="md" pb="md">
        <Text size={FS.meta} c={sum.tone === 'teal' ? undefined : sum.tone}>{sum.text}</Text>
        {note && <Alert color={note.ok ? 'teal' : 'rose'} withCloseButton onClose={() => setNote(null)}>{note.text}</Alert>}

        {n.defaultOpen && (
          <Alert color="amber" icon={<IconAlertTriangle size={16} />} title="The default user has no password">
            <Text size={FS.meta}>
              While it has none, protected mode refuses every client that is not on this server, whatever user it signs in as,
              so your users cannot connect from the LAN. Give it a password first.
              {n.defaultClients > 0 && ` ${n.defaultClients} connection${n.defaultClients === 1 ? ' is' : 's are'} signed in as default now; ${n.defaultClients === 1 ? 'it keeps' : 'they keep'} working until ${n.defaultClients === 1 ? 'it reconnects' : 'they reconnect'}, then need${n.defaultClients === 1 ? 's' : ''} the password.`}
            </Text>
            {canChange && (
              n.agentConf
                ? <Button mt={6} size="compact-xs" variant="default" onClick={() => setConfirm('secure')}>Give the default user a password</Button>
                : <Text size={FS.micro} mt={4}>Argus can’t do this here: Redis rewrites {n.configFile || 'no config file'}, but the agent signs in with a different file. Set requirepass on the server.</Text>
            )}
          </Alert>
        )}

        <Table verticalSpacing={4} fz="xs" withTableBorder>
          <Table.Thead>
            <Table.Tr>{canChange && <Table.Th w={40}>Listen</Table.Th>}<Table.Th>Address</Table.Th><Table.Th>From a client there</Table.Th><Table.Th w={40} /></Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {n.addresses.map((a) => {
              const st = ADDRESS_STATE[a.state]
              return (
                <Table.Tr key={a.ip}>
                  {canChange && (
                    <Table.Td>
                      <Tooltip label={a.private ? 'Listen on this address' : 'Not a private address: Argus opens Redis to the LAN only'} disabled={a.private}>
                        <Checkbox size="xs" aria-label={`Listen on ${a.ip}`} disabled={!a.private} checked={want.includes(a.ip)}
                          onChange={(e) => { const on = e.currentTarget.checked; setWant((w) => on ? [...w, a.ip] : w.filter((x) => x !== a.ip)) }} />
                      </Tooltip>
                    </Table.Td>
                  )}
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
            {n.addresses.length === 0 && <Table.Tr><Table.Td colSpan={4}><Text size={FS.meta} c="dimmed">This server has no address besides the loopback.</Text></Table.Td></Table.Tr>}
          </Table.Tbody>
        </Table>

        {canChange && dirty && (
          <Group justify="flex-end" gap={6}>
            <Button size="xs" variant="default" onClick={() => got(n)}>Discard</Button>
            <Button size="xs" disabled={want.length > 0 && n.defaultOpen} onClick={() => setConfirm('open')}>
              {want.length ? 'Open to the LAN…' : 'Listen on this server only…'}
            </Button>
          </Group>
        )}
        {canChange && dirty && want.length > 0 && n.defaultOpen && <Text size={FS.micro} c="amber" ta="right">Give the default user a password first.</Text>}

        {listening.length > 0 && (
          <Stack gap={4}>
            <Text size={FS.micro} c="dimmed">
              Checked from this server. A firewall here can still block other machines{n.firewall ? `; ${n.firewall} is on, so allow the port:` : '; if one runs, allow TCP port ' + n.port + ' from your LAN.'}
            </Text>
            {fwLines.map((l) => (
              <Group key={l} gap={4} wrap="nowrap"><Code style={{ flex: 1, fontSize: 11.5 }}>{l}</Code><Copy value={l} label="Copy command" /></Group>
            ))}
          </Stack>
        )}
      </Stack>

      <Modal opened={confirm === 'open'} onClose={() => { setConfirm(null); setUnderstood(false) }} title={want.length ? 'Open Redis to the LAN' : 'Listen on this server only'} size="md">
        <Stack gap="sm">
          {want.length ? (
            <>
              <Text size={FS.meta}>Redis will also listen on {want.map((w) => `${w}:${n.port}`).join(', ')}. Anything on {[...new Set(n.addresses.filter((a) => want.includes(a.ip)).map((a) => a.prefix))].join(', ')} can then connect; it still needs a user and password to do anything.</Text>
              <Checkbox checked={understood} onChange={(e) => setUnderstood(e.currentTarget.checked)}
                label="I understand every machine on these networks can reach Redis" />
            </>
          ) : <Text size={FS.meta}>Clients on the LAN are cut off; programs on this server keep working.</Text>}
          <Code block>{`CONFIG SET bind "127.0.0.1 -::1${want.map((w) => ` -${w}`).join('')}"\nCONFIG REWRITE`}</Code>
          {s.ready?.changesNeedApproval && <Text size={FS.micro} c="dimmed">This database's changes wait for someone else to approve exactly this.</Text>}
          <Group justify="flex-end">
            <Button variant="default" size="xs" onClick={() => { setConfirm(null); setUnderstood(false) }}>Cancel</Button>
            <Button size="xs" color={want.length ? 'amber' : undefined} disabled={want.length > 0 && !understood} loading={busy} onClick={() => void apply()}>
              {want.length ? 'Open to the LAN' : 'Listen here only'}
            </Button>
          </Group>
        </Stack>
      </Modal>
      <Modal opened={confirm === 'secure'} onClose={() => setConfirm(null)} title="Give the default user a password" size="md">
        <Stack gap="sm">
          <Text size={FS.meta}>The agent makes a long random password on the server and writes it to {n.configFile} as requirepass, where the agent reads its own sign-in. It never leaves the server and is not shown here.</Text>
          {n.defaultClients > 0 && <Alert color="amber">{n.defaultClients} connection{n.defaultClients === 1 ? ' is' : 's are'} signed in as default with no password. Any application that does that must move to its own user, or use the new password from {n.configFile}, before it reconnects.</Alert>}
          <Code block>{'CONFIG SET requirepass <made on the server>\nCONFIG REWRITE'}</Code>
          <Group justify="flex-end">
            <Button variant="default" size="xs" onClick={() => setConfirm(null)}>Cancel</Button>
            <Button size="xs" loading={busy} onClick={() => void secure()}>Set a password</Button>
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
