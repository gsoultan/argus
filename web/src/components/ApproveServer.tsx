import { useEffect, useMemo, useState } from 'react'
import {
  Alert, Avatar, Button, Checkbox, Group, Modal, MultiSelect, Stack, Text, TextInput,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { IconCircleCheck, IconPlus } from '@tabler/icons-react'
import { meQuery, useEnrolHost, usersQuery } from '~/lib/queries'
import { notifyError, notifyOk } from '~/lib/notify'
import { isElevatedAccount } from '~/lib/roles'
import type { DiscoveredHost } from '~/types/domain'
import { FS, SP } from '~/theme'

/**
 * Approving a server, with the two questions that make it usable.
 *
 * Approval used to grant nothing, by design, and stop there. The admin who had
 * just added a server then met "ask an admin to add an account" on the Connect
 * page, and the way on was an Edit dialog with a field called Principals and a
 * second dialog for assignments that nothing pointed at. So the approval asks
 * now: which people may connect, and which accounts they log in as. Both are
 * the admin's choice -- the agent's list of accounts is only the starting
 * point offered, never a grant -- and both may be left empty.
 */
export function ApproveServer({
  host, onClose,
}: {
  host: DiscoveredHost | null
  onClose: () => void
}) {
  const navigate = useNavigate()
  const { data: me } = useQuery(meQuery())
  const { data: users } = useQuery({ ...usersQuery(), enabled: host !== null })
  const enrol = useEnrolHost()

  const [accounts, setAccounts] = useState<string[]>([])
  const [picked, setPicked] = useState<string[]>([])
  const [people, setPeople] = useState<string[]>([])
  const [extra, setExtra] = useState('')

  // Offer what the server reported, with the first ordinary account ticked:
  // on most servers that is the one person who set it up logs in as.
  useEffect(() => {
    const reported = host?.accounts ?? []
    setAccounts(reported)
    const first = reported.find((a) => !isElevatedAccount(a))
    setPicked(first ? [first] : [])
    setPeople([])
    setExtra('')
  }, [host])

  const others = useMemo(
    () =>
      (users ?? [])
        .filter((u) => !u.disabled && u.email.toLowerCase() !== me?.email.toLowerCase())
        .map((u) => ({
          value: u.email,
          label: u.displayName ? `${u.displayName} (${u.email})` : u.email,
        })),
    [users, me?.email],
  )

  const addExtra = () => {
    const name = extra.trim()
    if (!name) return
    if (!accounts.includes(name)) setAccounts((a) => [...a, name])
    if (!picked.includes(name)) setPicked((p) => [...p, name])
    setExtra('')
  }

  const peopleWithoutAccount = people.length > 0 && picked.length === 0

  const approve = async (connect: boolean) => {
    if (!host) return
    let res: { assetId: string; assetHostname: string }
    try {
      res = await enrol.mutateAsync({
        hostname: host.hostname,
        grant: { principals: picked, people },
      })
    } catch (err) {
      notifyError(`${host.hostname} was not added`, err)
      return
    }
    onClose()
    if (connect && picked.length > 0) {
      void navigate({
        to: '/connect',
        search: { host: res.assetHostname, as: picked[0], open: true },
      })
      return
    }
    notifyOk(
      `${host.hostname} is added`,
      picked.length === 0
        ? 'Nobody can log in yet. Add a login account on its page under Servers.'
        : people.length === 0
          ? 'You can connect to it from Servers.'
          : `You and ${people.length === 1 ? people[0] : `${people.length} teammates`} can connect to it now.`,
    )
  }

  return (
    <Modal
      opened={host !== null}
      onClose={onClose}
      title={host ? `Add ${host.hostname} to Argus` : ''}
      size="md"
    >
      {host && (
        <Stack gap="lg">
          <Text size={FS.meta} c="dimmed" mt={-8}>
            {[host.os, host.addresses.join(', ')].filter(Boolean).join(' · ')}
          </Text>

          <Stack gap="xs">
            <Text fw={600} size="sm">Who can connect?</Text>
            <Group gap="sm" wrap="nowrap">
              <Avatar size={28} radius="xl" color="azure">
                {(me?.displayName || me?.email || '?').slice(0, 2).toUpperCase()}
              </Avatar>
              <Text size="sm">You</Text>
              <Text size={FS.meta} c="dimmed" ml="auto">Admin, always can</Text>
            </Group>
            <MultiSelect
              aria-label="Teammates who can connect"
              placeholder={others.length === 0 ? 'No teammates yet: add them under People' : 'Add teammates'}
              disabled={others.length === 0}
              searchable
              data={others}
              value={people}
              onChange={setPeople}
              nothingFoundMessage="Nobody by that name"
            />
          </Stack>

          <Stack gap="xs">
            <Text fw={600} size="sm">They log in to the server as</Text>
            {accounts.length === 0 && (
              <Text size={FS.meta} c="dimmed">
                The server reported no accounts. Type the one you log in with.
              </Text>
            )}
            <Group gap="xs">
              {accounts.map((a) => (
                <Checkbox
                  key={a}
                  label={<Text ff="monospace" size="sm">{a}</Text>}
                  checked={picked.includes(a)}
                  onChange={(e) => {
                    const on = e.currentTarget.checked
                    setPicked((p) => (on ? [...p, a] : p.filter((x) => x !== a)))
                  }}
                  styles={{ root: { padding: '8px 12px', border: '1px solid var(--color-line)', borderRadius: 8 } }}
                />
              ))}
            </Group>
            <Group gap="xs" wrap="nowrap">
              <TextInput
                aria-label="Another account"
                placeholder="Another account"
                value={extra}
                onChange={(e) => setExtra(e.currentTarget.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter') addExtra()
                }}
                style={{ flex: 1 }}
                ff="monospace"
              />
              <Button variant="default" leftSection={<IconPlus size={14} />} onClick={addExtra}>
                Add
              </Button>
            </Group>
            {picked.some(isElevatedAccount) && (
              <Text size={FS.meta} c="dimmed" lh={1.5}>
                Teammates who aren&rsquo;t admins still need an approved request for each{' '}
                {picked.filter(isElevatedAccount).join(', ')} login.
              </Text>
            )}
          </Stack>

          {peopleWithoutAccount && (
            <Alert color="amber" variant="light">
              <Text size={FS.meta}>Tick at least one account for your teammates to log in as.</Text>
            </Alert>
          )}

          <Group justify="flex-end" gap={SP.cozy}>
            <Button variant="subtle" color="slate" onClick={onClose} disabled={enrol.isPending}>
              Cancel
            </Button>
            <Button
              variant="default"
              loading={enrol.isPending}
              disabled={peopleWithoutAccount}
              onClick={() => void approve(false)}
            >
              Add server
            </Button>
            <Button
              leftSection={<IconCircleCheck size={15} />}
              loading={enrol.isPending}
              disabled={picked.length === 0}
              onClick={() => void approve(true)}
            >
              Add and connect
            </Button>
          </Group>
        </Stack>
      )}
    </Modal>
  )
}
