import { useMemo, useState } from 'react'
import {
  ActionIcon, Avatar, Badge, Box, Button, Card, Group, Menu, Modal, MultiSelect, SegmentedControl, Select,
  SimpleGrid, Stack, Table, Tabs, Text, TextInput, Tooltip, UnstyledButton,
} from '@mantine/core'
import { useDisclosure } from '@mantine/hooks'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import {
  IconDots, IconPencil, IconPlus, IconSearch, IconServer2, IconTrash, IconUserPlus, IconUsers,
  IconUsersGroup, IconX,
} from '@tabler/icons-react'
import { EmptyState, PageBody, PageHeader, Toolbar } from '~/components/page'
import { Mono, relTime } from '~/components/primitives'
import { generatePassword } from '~/components/Setup'
import { guard } from '~/lib/guard'
import { notifyError, notifyOk } from '~/lib/notify'
import {
  meQuery, teamsQuery, useAddAccount, useAddTeamMember, useCreateTeam, useDeleteTeam,
  useRemoveTeamMember, useRenameTeam, useSetDisabled, useSetLoginName, useSetRole, usersQuery,
} from '~/lib/queries'
import { canSee, roleLabel } from '~/lib/roles'
import { FS, SP } from '~/theme'
import type { Team, User, UserRole } from '~/types/domain'

/**
 * Teams, and the people in them.
 *
 * A server is given to a team on the server's own page, so this page is only
 * about who is in which team. People is a tab here rather than a menu item:
 * "who is in Ops" and "add Sara" are the same errand.
 */
export const Route = createFileRoute('/teams')({
  beforeLoad: ({ context }) => guard(context.queryClient, 'fleet'),
  validateSearch: (s: Record<string, unknown>): { tab?: 'people' } =>
    s.tab === 'people' ? { tab: 'people' } : {},
  loader: ({ context }) =>
    Promise.all([
      context.queryClient.ensureQueryData(teamsQuery()),
      context.queryClient.ensureQueryData(usersQuery()),
    ]),
  component: TeamsPage,
})

/** The roles in the order the picker shows them, with what each can do. */
const ROLES: { value: UserRole; label: string; what: string }[] = [
  { value: 'operator', label: 'Member', what: 'Connects to the servers they are given.' },
  { value: 'approver', label: 'Approver', what: 'A member who also answers approvals.' },
  { value: 'auditor', label: 'Viewer', what: 'Sees every server, session and recording. Cannot connect.' },
  { value: 'admin', label: 'Admin', what: 'Adds servers, gives access, changes settings.' },
  { value: 'owner', label: 'Owner', what: 'An admin who can also make other owners.' },
]

function TeamsPage() {
  const { tab } = Route.useSearch()
  const navigate = useNavigate()
  const { data: me } = useQuery(meQuery())
  const admin = canSee(me?.role, 'admin')
  const [newTeam, newTeamModal] = useDisclosure(false)
  const [newPerson, newPersonModal] = useDisclosure(false)
  const people = tab === 'people'

  return (
    <Box>
      <PageHeader
        title="Teams"
        description={
          people
            ? 'Everyone who can sign in, and what they can do.'
            : 'Give a server to a team once, on the server’s page. Anyone you add to the team can connect to it.'
        }
        actions={
          admin &&
          (people ? (
            <Button leftSection={<IconUserPlus size={16} />} onClick={newPersonModal.open}>
              Add person
            </Button>
          ) : (
            <Button leftSection={<IconPlus size={16} />} onClick={newTeamModal.open}>
              New team
            </Button>
          ))
        }
      />
      <PageBody>
        <Tabs
          value={people ? 'people' : 'teams'}
          onChange={(v) => void navigate({ to: '/teams', search: v === 'people' ? { tab: 'people' } : {} })}
        >
          <Tabs.List>
            <Tabs.Tab value="teams" leftSection={<IconUsersGroup size={16} />}>Teams</Tabs.Tab>
            <Tabs.Tab value="people" leftSection={<IconUsers size={16} />}>People</Tabs.Tab>
          </Tabs.List>
        </Tabs>
        {people ? <People admin={admin} me={me} /> : <TeamList admin={admin} onNew={newTeamModal.open} />}
      </PageBody>
      {admin && <NewTeamModal opened={newTeam} onClose={newTeamModal.close} />}
      {admin && <NewPersonModal opened={newPerson} onClose={newPersonModal.close} />}
    </Box>
  )
}

/* ── Teams ─────────────────────────────────────────────────────────────── */

function TeamList({ admin, onNew }: { admin: boolean; onNew: () => void }) {
  const { data: teams } = useQuery(teamsQuery())
  if (teams && teams.length === 0) {
    return (
      <Card>
        <EmptyState
          icon={IconUsersGroup}
          title="No teams yet"
          description="Make a team, such as Ops or Developers, and put people in it. Then give a server to the team and everyone in it can connect."
          action={admin ? <Button leftSection={<IconPlus size={16} />} onClick={onNew}>New team</Button> : undefined}
        />
      </Card>
    )
  }
  return (
    <SimpleGrid cols={{ base: 1, md: 2 }} spacing="md">
      {teams?.map((t) => <TeamCard key={t.id} team={t} admin={admin} />)}
    </SimpleGrid>
  )
}

function TeamCard({ team, admin }: { team: Team; admin: boolean }) {
  const { data: users } = useQuery(usersQuery())
  const add = useAddTeamMember()
  const remove = useRemoveTeamMember()
  const del = useDeleteTeam()
  const [renaming, rename] = useDisclosure(false)
  const [confirming, confirm] = useDisclosure(false)
  const [adding, setAdding] = useState<string | null>(null)

  const names = useMemo(() => new Map((users ?? []).map((u) => [u.email, u.displayName])), [users])
  const candidates = (users ?? [])
    .filter((u) => !u.disabled && !team.members.includes(u.email))
    .map((u) => ({ value: u.email, label: `${u.displayName} (${u.email})` }))

  const onAdd = async (email: string | null) => {
    if (!email) return
    setAdding(email)
    try {
      await add.mutateAsync({ id: team.id, email })
      notifyOk('Added', `${names.get(email) ?? email} is in ${team.name} now.`)
    } catch (err) {
      notifyError('Not added', err)
    } finally {
      setAdding(null)
    }
  }

  return (
    <Card padding="lg">
      <Group justify="space-between" wrap="nowrap" mb="md">
        <Group gap="sm" wrap="nowrap" style={{ minWidth: 0 }}>
          <Avatar color="azure" variant="light" radius="md">
            {team.name.slice(0, 2).toUpperCase()}
          </Avatar>
          <Box style={{ minWidth: 0 }}>
            <Text fw={600} truncate>{team.name}</Text>
            <Text size={FS.meta} c="dimmed">
              {team.members.length} {team.members.length === 1 ? 'person' : 'people'} ·{' '}
              {team.servers.length} {team.servers.length === 1 ? 'server' : 'servers'}
            </Text>
          </Box>
        </Group>
        {admin && (
          <Menu position="bottom-end" withArrow>
            <Menu.Target>
              <ActionIcon variant="subtle" color="slate" aria-label={`More for ${team.name}`}>
                <IconDots size={18} />
              </ActionIcon>
            </Menu.Target>
            <Menu.Dropdown>
              <Menu.Item leftSection={<IconPencil size={14} />} onClick={rename.open}>Rename</Menu.Item>
              <Menu.Item leftSection={<IconTrash size={14} />} color="rose" onClick={confirm.open}>
                Delete team
              </Menu.Item>
            </Menu.Dropdown>
          </Menu>
        )}
      </Group>

      <Text size={FS.meta} fw={600} c="dimmed" mb={SP.tight}>People</Text>
      <Stack gap={SP.tight} mb="md">
        {team.members.length === 0 && <Text size="sm" c="dimmed">Nobody yet.</Text>}
        {team.members.map((m) => (
          <Group key={m} justify="space-between" wrap="nowrap" gap="xs">
            <Group gap="xs" wrap="nowrap" style={{ minWidth: 0 }}>
              <Avatar size={26} radius="xl" color="slate" variant="light">
                {(names.get(m) ?? m).slice(0, 2).toUpperCase()}
              </Avatar>
              <Box style={{ minWidth: 0 }}>
                <Text size="sm" truncate>{names.get(m) ?? m}</Text>
              </Box>
            </Group>
            {admin && (
              <Tooltip label={`Take ${names.get(m) ?? m} out of ${team.name}`}>
                <ActionIcon
                  variant="subtle"
                  color="slate"
                  size="sm"
                  aria-label={`Remove ${m} from ${team.name}`}
                  loading={remove.isPending && remove.variables?.email === m}
                  onClick={() =>
                    void remove.mutateAsync({ id: team.id, email: m }).then(
                      () => notifyOk('Removed', `${names.get(m) ?? m} left ${team.name} and its servers.`),
                      (err: unknown) => notifyError('Not removed', err),
                    )
                  }
                >
                  <IconX size={14} />
                </ActionIcon>
              </Tooltip>
            )}
          </Group>
        ))}
        {admin && (
          <Select
            mt={SP.tight}
            placeholder="+ Add a person"
            searchable
            data={candidates}
            value={null}
            disabled={adding !== null}
            onChange={(v) => void onAdd(v)}
            nothingFoundMessage="Everyone is already in this team"
            aria-label={`Add a person to ${team.name}`}
          />
        )}
      </Stack>

      <Text size={FS.meta} fw={600} c="dimmed" mb={SP.tight}>Servers</Text>
      {team.servers.length === 0 ? (
        <Text size="sm" c="dimmed">
          None yet. Open a server and use Add team or person.
        </Text>
      ) : (
        <Group gap={SP.tight}>
          {team.servers.map((g) => (
            <Badge key={g.assetId} variant="light" color="slate" leftSection={<IconServer2 size={12} />}>
              {g.hostname} as {g.principals.join(', ')}
            </Badge>
          ))}
        </Group>
      )}

      {admin && <RenameTeamModal team={team} opened={renaming} onClose={rename.close} />}
      <Modal opened={confirming} onClose={confirm.close} title={`Delete ${team.name}?`}>
        <Text size="sm" mb="md">
          Everyone in it loses the servers they had only through this team, right away.
          The activity log keeps who was in it.
        </Text>
        <Group justify="flex-end">
          <Button variant="default" onClick={confirm.close}>Keep it</Button>
          <Button
            color="rose"
            loading={del.isPending}
            onClick={() =>
              void del.mutateAsync(team.id).then(
                () => {
                  notifyOk('Team deleted', `${team.name} is gone.`)
                  confirm.close()
                },
                (err: unknown) => notifyError('Not deleted', err),
              )
            }
          >
            Delete team
          </Button>
        </Group>
      </Modal>
    </Card>
  )
}

function NewTeamModal({ opened, onClose }: { opened: boolean; onClose: () => void }) {
  const { data: users } = useQuery(usersQuery())
  const create = useCreateTeam()
  const [name, setName] = useState('')
  const [members, setMembers] = useState<string[]>([])
  const close = () => {
    setName('')
    setMembers([])
    onClose()
  }
  const submit = async () => {
    try {
      const t = await create.mutateAsync({ name, members })
      notifyOk('Team made', `${t.name} is ready. Give it a server from the server’s page.`)
      close()
    } catch (err) {
      notifyError('Team not made', err)
    }
  }
  return (
    <Modal opened={opened} onClose={close} title="New team">
      <Stack gap="md">
        <TextInput
          label="Name"
          placeholder="Ops"
          data-autofocus
          value={name}
          maxLength={80}
          onChange={(e) => setName(e.currentTarget.value)}
        />
        <MultiSelect
          label="People"
          description="You can add more later."
          placeholder="Pick people"
          searchable
          data={(users ?? []).filter((u) => !u.disabled).map((u) => ({ value: u.email, label: u.displayName }))}
          value={members}
          onChange={setMembers}
        />
        <Group justify="flex-end">
          <Button variant="default" onClick={close}>Cancel</Button>
          <Button disabled={!name.trim()} loading={create.isPending} onClick={() => void submit()}>
            Make team
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}

function RenameTeamModal({ team, opened, onClose }: { team: Team; opened: boolean; onClose: () => void }) {
  const rename = useRenameTeam()
  const [name, setName] = useState(team.name)
  return (
    <Modal opened={opened} onClose={onClose} title={`Rename ${team.name}`}>
      <Stack gap="md">
        <TextInput label="Name" value={name} maxLength={80} onChange={(e) => setName(e.currentTarget.value)} />
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button
            disabled={!name.trim() || name.trim() === team.name}
            loading={rename.isPending}
            onClick={() =>
              void rename.mutateAsync({ id: team.id, name }).then(onClose, (err: unknown) =>
                notifyError('Not renamed', err),
              )
            }
          >
            Save
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}

/* ── People ────────────────────────────────────────────────────────────── */

function People({ admin, me }: { admin: boolean; me: User | undefined }) {
  const { data: users } = useQuery(usersQuery())
  const { data: teams } = useQuery(teamsQuery())
  const setRole = useSetRole()
  const setDisabled = useSetDisabled()
  const [search, setSearch] = useState('')
  const [show, setShow] = useState<'active' | 'removed'>('active')
  const [renaming, setRenaming] = useState<User | null>(null)

  const teamsOf = useMemo(() => {
    const m = new Map<string, string[]>()
    for (const t of teams ?? []) for (const e of t.members) m.set(e, [...(m.get(e) ?? []), t.name])
    return m
  }, [teams])

  const needle = search.trim().toLowerCase()
  const removedCount = (users ?? []).filter((u) => u.disabled).length
  const rows = (users ?? []).filter(
    (u) =>
      (show === 'removed' ? !!u.disabled : !u.disabled) &&
      (!needle || `${u.displayName} ${u.email} ${u.loginName ?? ''}`.toLowerCase().includes(needle)),
  )

  const changeRole = async (u: User, role: UserRole) => {
    try {
      await setRole.mutateAsync({ email: u.email, role })
      notifyOk('Role changed', `${u.displayName} is ${roleLabel(role)} now.`)
    } catch (err) {
      notifyError('Role not changed', err)
    }
  }
  const toggle = async (u: User) => {
    try {
      await setDisabled.mutateAsync({ email: u.email, disabled: !u.disabled })
      notifyOk(u.disabled ? 'Can sign in again' : 'Signed out for good', u.email)
    } catch (err) {
      notifyError('Not changed', err)
    }
  }

  return (
    <Stack gap="md">
      <Toolbar right={users && <Text size={FS.meta} c="dimmed">{rows.length} of {users.length}</Text>}>
        <SegmentedControl
          value={show}
          onChange={(v) => setShow(v as 'active' | 'removed')}
          data={[
            { value: 'active', label: 'Can sign in' },
            { value: 'removed', label: `Removed (${removedCount})` },
          ]}
        />
        <TextInput
          w={300}
          placeholder="Find a person"
          leftSection={<IconSearch size={14} />}
          value={search}
          onChange={(e) => setSearch(e.currentTarget.value)}
        />
      </Toolbar>
      <Card padding={0}>
        <Table.ScrollContainer minWidth={860}>
          <Table verticalSpacing="sm">
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Person</Table.Th>
                <Table.Th>Role</Table.Th>
                <Table.Th>Teams</Table.Th>
                <Table.Th>Server account</Table.Th>
                <Table.Th>{show === 'removed' ? 'Removed' : 'Last seen'}</Table.Th>
                <Table.Th />
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {rows.map((u) => {
                const self = u.email.toLowerCase() === me?.email.toLowerCase()
                return (
                  <Table.Tr key={u.email} style={u.disabled ? { opacity: 0.55 } : undefined}>
                    <Table.Td>
                      <Group gap="sm" wrap="nowrap">
                        <Avatar size={32} radius="xl" color="azure" variant="light">
                          {u.displayName.slice(0, 2).toUpperCase()}
                        </Avatar>
                        <Box style={{ minWidth: 0 }}>
                          <Text size="sm" fw={500}>
                            {u.displayName}
                            {self && <Text span c="dimmed" size="sm"> (you)</Text>}
                          </Text>
                          <Mono c="dimmed">{u.email}</Mono>
                        </Box>
                      </Group>
                    </Table.Td>
                    <Table.Td>
                      {admin && !self && !u.disabled ? (
                        <Select
                          w={140}
                          size="xs"
                          aria-label={`Role of ${u.email}`}
                          allowDeselect={false}
                          data={ROLES.filter((r) => r.value !== 'owner' || me?.role === 'owner' || u.role === 'owner')
                            .map((r) => ({ value: r.value, label: r.label }))}
                          value={u.role}
                          disabled={setRole.isPending}
                          onChange={(v) => v && v !== u.role && void changeRole(u, v as UserRole)}
                        />
                      ) : (
                        <Tooltip label={ROLES.find((r) => r.value === u.role)?.what}>
                          <Badge variant="light" color="slate">{roleLabel(u.role)}</Badge>
                        </Tooltip>
                      )}
                    </Table.Td>
                    <Table.Td>
                      <Group gap={SP.tight}>
                        {(teamsOf.get(u.email) ?? []).map((t) => (
                          <Badge key={t} variant="outline" color="slate">{t}</Badge>
                        ))}
                        {u.disabled && <Badge color="rose" variant="light">cannot sign in</Badge>}
                        {!u.disabled && !u.mfaEnrolled && (
                          <Tooltip label="They sign in with a password only. They are asked to add a second step next time.">
                            <Badge color="amber" variant="light">no second step</Badge>
                          </Tooltip>
                        )}
                      </Group>
                    </Table.Td>
                    <Table.Td>
                      {u.loginName ? (
                        <Tooltip
                          label={admin ? 'Their login on servers. Click to rename it.' : 'Their login on servers'}
                        >
                          <UnstyledButton
                            onClick={admin ? () => setRenaming(u) : undefined}
                            aria-label={`Server account of ${u.email}`}
                          >
                            <Group gap={SP.tight} wrap="nowrap">
                              <Mono>{u.loginName}</Mono>
                              {u.hasSshKey && <Badge size="xs" variant="light" color="azure">SSH key</Badge>}
                            </Group>
                          </UnstyledButton>
                        </Tooltip>
                      ) : (
                        <Text size={FS.meta} c="dimmed">none yet</Text>
                      )}
                    </Table.Td>
                    <Table.Td>
                      {show === 'removed' ? (
                        <Text size={FS.meta} c="dimmed">
                          {u.disabledAt ? relTime(u.disabledAt) : 'yes'}
                          {u.disabledBy ? ` by ${u.disabledBy}` : ''}
                        </Text>
                      ) : (
                        <Text size={FS.meta} c="dimmed">{relTime(u.lastSeenAt)}</Text>
                      )}
                    </Table.Td>
                    <Table.Td ta="right">
                      {admin && !self && (
                        <Button
                          size="compact-sm"
                          variant="subtle"
                          color={u.disabled ? 'azure' : 'rose'}
                          loading={setDisabled.isPending && setDisabled.variables?.email === u.email}
                          onClick={() => void toggle(u)}
                        >
                          {u.disabled ? 'Let back in' : 'Remove'}
                        </Button>
                      )}
                    </Table.Td>
                  </Table.Tr>
                )
              })}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
        {rows.length === 0 && (
          show === 'removed' && !needle
            ? <EmptyState icon={IconUsers} title="Nobody removed" description="People you remove are listed here." />
            : <EmptyState icon={IconUsers} title="Nobody matches" description="Clear the search to see everyone." />
        )}
      </Card>
      <Text size={FS.meta} c="dimmed">
        Removing someone stops them signing in, and locks their own accounts on every server within a
        minute. Nothing is deleted: their sessions, recordings and activity stay under their name.
      </Text>
      {renaming && <RenameLoginModal user={renaming} onClose={() => setRenaming(null)} />}
    </Stack>
  )
}

/** Rename someone's server account, for a name a server already has. */
function RenameLoginModal({ user, onClose }: { user: User; onClose: () => void }) {
  const rename = useSetLoginName()
  const [name, setName] = useState(user.loginName ?? '')
  const save = async () => {
    try {
      await rename.mutateAsync({ email: user.email, name: name.trim() })
      notifyOk('Server account renamed', `${user.displayName} logs in as ${name.trim()} now. Their old account is locked, not deleted.`)
      onClose()
    } catch (err) {
      notifyError('Not renamed', err)
    }
  }
  return (
    <Modal opened onClose={onClose} title={`Server account of ${user.displayName}`}>
      <Stack gap="md">
        <TextInput
          label="Account name"
          description="Lowercase letters, digits, dot, underscore and hyphen. The old account is locked on every server and this one made in its place."
          value={name}
          onChange={(e) => setName(e.currentTarget.value)}
          data-autofocus
        />
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button
            loading={rename.isPending}
            disabled={!name.trim() || name.trim() === user.loginName}
            onClick={() => void save()}
          >
            Rename
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}

function NewPersonModal({ opened, onClose }: { opened: boolean; onClose: () => void }) {
  const add = useAddAccount()
  const { data: me } = useQuery(meQuery())
  const [email, setEmail] = useState('')
  const [name, setName] = useState('')
  const [role, setRole] = useState<UserRole>('operator')
  const [password, setPassword] = useState(() => generatePassword())
  const close = () => {
    setEmail('')
    setName('')
    setRole('operator')
    setPassword(generatePassword())
    onClose()
  }
  const submit = async () => {
    try {
      await add.mutateAsync({ email, displayName: name, role, password })
      notifyOk(
        'Person added',
        `${email} can sign in now. Send them the first password over a channel you trust; they set up a second step when they first sign in.`,
      )
      close()
    } catch (err) {
      notifyError('Not added', err)
    }
  }
  return (
    <Modal opened={opened} onClose={close} title="Add person">
      <Stack gap="md">
        <TextInput label="Email" type="email" data-autofocus value={email} onChange={(e) => setEmail(e.currentTarget.value)} />
        <TextInput label="Name" description="Optional" value={name} onChange={(e) => setName(e.currentTarget.value)} />
        <Select
          label="Role"
          allowDeselect={false}
          data={ROLES.filter((r) => r.value !== 'owner' || me?.role === 'owner').map((r) => ({
            value: r.value,
            label: `${r.label}: ${r.what}`,
          }))}
          value={role}
          onChange={(v) => setRole((v ?? 'operator') as UserRole)}
        />
        <Group align="flex-end" gap="xs" wrap="nowrap">
          <TextInput
            label="First password"
            description="They can change it after signing in"
            style={{ flex: 1 }}
            value={password}
            onChange={(e) => setPassword(e.currentTarget.value)}
            styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)' } }}
          />
          <Button variant="default" onClick={() => setPassword(generatePassword())}>New</Button>
        </Group>
        <Group justify="flex-end">
          <Button variant="default" onClick={close}>Cancel</Button>
          <Button
            loading={add.isPending}
            disabled={!email.includes('@') || [...password].length < 12}
            onClick={() => void submit()}
          >
            Add person
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}
