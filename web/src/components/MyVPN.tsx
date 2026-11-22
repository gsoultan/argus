import { Alert, Badge, Box, Button, Group, Modal, Stack, Table, Text, TextInput } from '@mantine/core'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { IconDownload, IconPlus, IconShieldLock } from '@tabler/icons-react'
import qrcode from 'qrcode-generator'
import { useMemo, useState } from 'react'
import { DataTable, EmptyState, PageBody, PageHeader, SectionCard } from '~/components/page'
import { relTime } from '~/components/primitives'
import { isConfigured } from '~/lib/live'
import { run } from '~/lib/notify'
import {
  makeStaffDevice, removeStaffDevice, renewStaffDevice, STAFF_MODE, staffVPNsQuery,
  type StaffDevice, type StaffProfile, type StaffVPN,
} from '~/lib/networks'
import { meQuery } from '~/lib/queries'
import { FS, SP } from '~/theme'

/**
 * My VPN: anyone's own devices on the staff VPN. What their teams reach, their
 * traffic setting, and a profile per laptop or phone, made with an
 * authenticator code and shown once.
 */
export function MyVPNPage() {
  const { data, isLoading, error } = useQuery({ ...staffVPNsQuery(), enabled: isConfigured() })
  const { data: me } = useQuery({ ...meQuery(), enabled: isConfigured() })
  const vpns = data ?? []
  return (
    <Box>
      <PageHeader title="My VPN" description="Your laptop and phone on the company VPN. Each device gets its own profile; it lasts a set time and you renew it here." />
      <PageBody>
        {!isConfigured() ? (
          <Alert color="slate"><Text size={FS.body}>Connect this console to Argus to use the VPN.</Text></Alert>
        ) : error ? (
          <Alert color="rose"><Text size={FS.body}>Could not read the VPN: {error.message}</Text></Alert>
        ) : isLoading ? null : vpns.length === 0 ? (
          <EmptyState icon={IconShieldLock} title="There is no company VPN yet." description="An admin sets it up on Networks." />
        ) : (
          <Stack gap="lg">{vpns.map((v) => <MyVPNCard key={v.network.id} v={v} me={me?.email} />)}</Stack>
        )}
      </PageBody>
    </Box>
  )
}

function MyVPNCard({ v, me }: { v: StaffVPN; me?: string }) {
  const qc = useQueryClient()
  const done = () => qc.invalidateQueries({ queryKey: ['networks', 'staff'] })
  const [making, setMaking] = useState(false)
  const [renewing, setRenewing] = useState<StaffDevice | null>(null)
  const remove = useMutation({ mutationFn: (id: string) => removeStaffDevice(id), onSettled: done })
  const mine = v.devices.filter((d) => d.userEmail === me)
  const canMake = v.ready && v.mine.reach.length > 0 && (!v.mine.needsCode || v.mine.hasMfa)
  return (
    <Box data-testid="my-vpn">
    <SectionCard title={v.network.name} icon={IconShieldLock} iconColor="grape"
      description={`${STAFF_MODE[v.mine.mode].label}: ${STAFF_MODE[v.mine.mode].hint}`}
      action={canMake && <Button size="xs" leftSection={<IconPlus size={14} />} onClick={() => setMaking(true)}>Add a device</Button>}>
      <Stack gap="md">
        {!v.ready ? (
          <Alert color="slate" p="xs"><Text size={FS.meta}>The VPN server is being set up. Devices can be added once it is running.</Text></Alert>
        ) : v.mine.reach.length === 0 ? (
          <Alert color="slate" p="xs"><Text size={FS.meta}>None of your teams reaches anything through the VPN yet. Ask an admin to give your team access.</Text></Alert>
        ) : v.mine.needsCode && !v.mine.hasMfa ? (
          <Alert color="amber" p="xs">
            <Text size={FS.meta}>The VPN asks for an authenticator code. Choose "Turn on two-step sign-in" in your account menu first.</Text>
          </Alert>
        ) : null}
        {v.mine.reach.length > 0 && (
          <Stack gap={4}>
            <Text size={FS.body} fw={600}>You reach</Text>
            <Group gap={SP.tight}>{v.mine.reach.map((r) => <Badge key={r} variant="light" ff="monospace" tt="none">{r}</Badge>)}</Group>
            <Text size={FS.meta} c="dimmed">
              {v.mine.logins.length > 0
                ? `SSH and RDP straight to ${v.mine.logins.join(', ')} is allowed, and is not recorded by Argus. Logins through Argus are recorded.`
                : 'SSH and RDP go through Argus, which records them; the VPN refuses them straight to a server.'}
            </Text>
          </Stack>
        )}
        <DataTable columns={['Device', 'Address', 'Lasts until', '']} minWidth={520} isEmpty={mine.length === 0}
          empty={<EmptyState icon={IconShieldLock} title="No devices yet." description="Add one for each laptop or phone." />}>
          {mine.map((d) => (
            <Table.Tr key={d.id} data-testid="my-device">
              <Table.Td><Text size={FS.body} fw={500}>{d.name}</Text></Table.Td>
              <Table.Td><Text size={FS.body} ff="monospace">{d.address}</Text></Table.Td>
              <Table.Td>{d.expired ? <Badge color="rose" variant="light">Expired</Badge> : <Text size={FS.body}>{relTime(d.expiresAt)}</Text>}</Table.Td>
              <Table.Td align="right">
                <Group gap={4} justify="flex-end" wrap="nowrap">
                  <Button size="compact-xs" variant="subtle" onClick={() => setRenewing(d)}>Renew</Button>
                  <Button size="compact-xs" variant="subtle" color="rose"
                    onClick={() => void run(() => remove.mutateAsync(d.id), { failure: 'Not removed', success: ['Removed', 'It can no longer connect.'] })}>Remove</Button>
                </Group>
              </Table.Td>
            </Table.Tr>
          ))}
        </DataTable>
      </Stack>
      {making && <NewDevice v={v} onClose={() => { setMaking(false); void done() }} />}
      {renewing && <Renew d={renewing} needsCode={v.mine.needsCode} days={v.network.staff?.days ?? 90} onClose={() => { setRenewing(null); void done() }} />}
    </SectionCard>
    </Box>
  )
}

function Code({ value, onChange }: { value: string; onChange: (s: string) => void }) {
  return (
    <TextInput label="Code from your authenticator app" inputMode="numeric" autoComplete="one-time-code" maxLength={6} w={220}
      placeholder="123456" value={value} onChange={(e) => onChange(e.currentTarget.value.replace(/\D/g, ''))} />
  )
}

function NewDevice({ v, onClose }: { v: StaffVPN; onClose: () => void }) {
  const [name, setName] = useState('')
  const [code, setCode] = useState('')
  const [made, setMade] = useState<StaffProfile | null>(null)
  const make = useMutation({ mutationFn: () => makeStaffDevice(v.network.id, name.trim(), v.mine.needsCode ? code : undefined) })
  return (
    <Modal opened onClose={onClose} title={made ? `${made.device.name} is ready` : 'Add a device'} size="lg" closeOnClickOutside={!made}>
      {made ? <Profile p={made} onDone={onClose} /> : (
        <Stack gap="md">
          <TextInput label="Device" placeholder="Work laptop" maxLength={60} value={name} onChange={(e) => setName(e.currentTarget.value)} />
          {v.mine.needsCode && <Code value={code} onChange={setCode} />}
          <Text size={FS.meta} c="dimmed">It lasts {v.network.staff?.days ?? 90} days. Install the WireGuard app on the device to use it.</Text>
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <Button disabled={!name.trim() || (v.mine.needsCode && code.length !== 6)} loading={make.isPending}
              onClick={() => void run(async () => setMade(await make.mutateAsync()), { failure: 'Not made' })}>Make the profile</Button>
          </Group>
        </Stack>
      )}
    </Modal>
  )
}

function Profile({ p, onDone }: { p: StaffProfile; onDone: () => void }) {
  const [saved, setSaved] = useState(false)
  const svg = useMemo(() => {
    const q = qrcode(0, 'L')
    q.addData(p.config)
    q.make()
    return q.createSvgTag({ cellSize: 3, margin: 2, scalable: true })
  }, [p.config])
  const file = `${p.device.name.replace(/[^A-Za-z0-9_-]+/g, '-').slice(0, 15) || 'argus'}.conf`
  const download = () => {
    const a = document.createElement('a')
    a.href = URL.createObjectURL(new Blob([p.config], { type: 'text/plain' }))
    a.download = file
    a.click()
    URL.revokeObjectURL(a.href)
    setSaved(true)
  }
  return (
    <Stack gap="md" data-testid="staff-profile">
      <Alert color="amber" p="xs">
        <Text size={FS.meta}>
          This is the only time the profile is shown: it holds the device's private key, which Argus does not keep. Scan it with the
          WireGuard app on a phone, or download it for a laptop. What it routes is fixed now; if your teams' access changes, the VPN
          enforces it at once, and a new profile picks up new ranges.
        </Text>
      </Alert>
      <Group align="flex-start" gap="lg" wrap="wrap">
        <Box w={200} h={200} bg="white" p={4} style={{ borderRadius: 8 }} role="img" aria-label="QR code of the VPN profile"
          onClick={() => setSaved(true)}
          // Drawn here from the profile the control plane just made.
          dangerouslySetInnerHTML={{ __html: svg }} />
        <Stack gap="xs" style={{ flex: 1, minWidth: 220 }}>
          <Text size={FS.meta} c="dimmed">Address {p.device.address}, until {new Date(p.device.expiresAt).toLocaleDateString()}</Text>
          <Button variant="default" leftSection={<IconDownload size={14} />} onClick={download}>Download {file}</Button>
          <Button variant="subtle" size="xs" onClick={() => setSaved(true)}>I scanned it</Button>
        </Stack>
      </Group>
      <Group justify="flex-end">
        <Button onClick={onDone} disabled={!saved}>{saved ? 'Done' : 'Scan or download it first'}</Button>
      </Group>
    </Stack>
  )
}

function Renew({ d, needsCode, days, onClose }: { d: StaffDevice; needsCode: boolean; days: number; onClose: () => void }) {
  const [code, setCode] = useState('')
  const renew = useMutation({ mutationFn: () => renewStaffDevice(d.id, needsCode ? code : undefined) })
  return (
    <Modal opened onClose={onClose} title={`Renew ${d.name}`}>
      <Stack gap="md">
        <Text size={FS.body}>It will last another {days} days from now. The profile on the device stays as it is.</Text>
        {needsCode && <Code value={code} onChange={setCode} />}
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button disabled={needsCode && code.length !== 6} loading={renew.isPending}
            onClick={() => void run(() => renew.mutateAsync(), { failure: 'Not renewed', success: ['Renewed', `${d.name} lasts another ${days} days.`] }).then((ok) => { if (ok) onClose() })}>Renew</Button>
        </Group>
      </Stack>
    </Modal>
  )
}
