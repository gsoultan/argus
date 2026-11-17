import { useMemo, useState } from 'react'
import {
  ActionIcon, Alert, Anchor, Avatar, Badge, Box, Button, Card, Code, CopyButton, Group, Menu, Modal, MultiSelect,
  Radio, SegmentedControl, Select, SimpleGrid, Stack, Table, Tabs, Text, TextInput, Tooltip, UnstyledButton,
} from '@mantine/core'
import { useDisclosure } from '@mantine/hooks'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import {
  IconAlertTriangle, IconCopy, IconDots, IconMail, IconPencil, IconPlus, IconSearch, IconServer2, IconTrash,
  IconUserPlus, IconUsers, IconUsersGroup, IconX,
} from '@tabler/icons-react'
import { EmptyState, PageBody, PageHeader, Toolbar } from '~/components/page'
import { Mono, relTime } from '~/components/primitives'
import { guard } from '~/lib/guard'
import { notifyError, notifyOk } from '~/lib/notify'
import {
  meQuery, teamsQuery, useAddAccount, useAddSharedAccount, useAddTeamMember, useCreateTeam, useDeleteTeam,
  useRemoveTeamMember, useRenameTeam, useResetMFA, useSetDisabled, useSetLoginName, useSetRole,
  useSendTemporaryPassword, useSignOutAccount, usersQuery, fromOrganizationsQuery, personOrganizationsQuery,
  useAllowPersonOrganization, useRemovePersonOrganization,
} from '~/lib/queries'
import { canSee, roleLabel } from '~/lib/roles'
import { FS, SP } from '~/theme'
import type { Team, TemporaryPasswordOutcome, User, UserRole } from '~/types/domain'
import { GiveAccess, PersonServersModal } from '~/components/PersonServers'

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
    <SimpleGrid cols={{ base: 1, md: 2, xl: 3 }} spacing="md">
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
  const [viewing, setViewing] = useState<User | null>(null)
  const [placing, setPlacing] = useState<User | null>(null)

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
  const resetMFA = useResetMFA()
  const signOut = useSignOutAccount()
  const sendTemp = useSendTemporaryPassword()
  const [handOver, setHandOver] = useState<TemporaryPasswordOutcome | null>(null)
  const sendNewPassword = async (u: User) => {
    const what = me?.canEmailPeople
      ? `Email ${u.displayName} a new temporary password?`
      : `Make a new temporary password for ${u.displayName}? Email is not set up, so it is shown to you once to hand over.`
    if (!window.confirm(`${what} Their current password stops working and they are signed out everywhere. Their authenticator stays as it is.`)) return
    try {
      const out = await sendTemp.mutateAsync(u.email)
      if (out.emailed) {
        notifyOk('Temporary password emailed', `${u.email} gets it by email. It works until ${new Date(out.temporaryPasswordExpiresAt).toLocaleString()}.`)
      } else {
        setHandOver(out)
      }
    } catch (err) {
      notifyError('Not sent', err)
    }
  }
  const reset2fa = async (u: User) => {
    if (!window.confirm(`Remove ${u.displayName}'s authenticator and recovery codes? They are signed out everywhere and set up a new one at their next sign-in. Only do this for a person you have identified yourself.`)) return
    try {
      await resetMFA.mutateAsync(u.email)
      notifyOk('Second step removed', `${u.email} signs in with their password next, then sets up a new authenticator.`)
    } catch (err) {
      notifyError('Not reset', err)
    }
  }
  const signOutAll = async (u: User) => {
    try {
      await signOut.mutateAsync(u.email)
      notifyOk('Signed out everywhere', `Every browser signed in as ${u.email} has to sign in again.`)
    } catch (err) {
      notifyError('Not signed out', err)
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
                        {u.shared && (
                          <Tooltip label="They sign in with the password and second step of another organization. Their role here is this organization's.">
                            <Badge color="violet" variant="light">shared</Badge>
                          </Tooltip>
                        )}
                        {(u.alsoIn ?? []).map((o) => (
                          <Tooltip key={o} label={`Also works in ${o}, signing in here. Organizations, on this row, says where else.`}>
                            <Badge color="violet" variant="light">also in {o}</Badge>
                          </Tooltip>
                        ))}
                        {u.pickWhere && (
                          <Tooltip label="Was open to every organization before. No organization can add them now until you pick which, under Organizations.">
                            <Badge color="amber" variant="light">pick where they may work</Badge>
                          </Tooltip>
                        )}
                        {u.disabled && <Badge color="rose" variant="light">cannot sign in</Badge>}
                        {!u.disabled && u.temporaryPassword && (
                          <InvitedBadge expiresAt={u.temporaryPasswordExpiresAt} />
                        )}
                        {!u.disabled && !u.temporaryPassword && !u.mfaEnrolled && (
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
                      <Button size="compact-sm" variant="subtle" onClick={() => setViewing(u)}>
                        What they can do
                      </Button>
                      {admin && me?.multiOrg && !u.shared && !u.disabled && (
                        <Tooltip label="Which other organizations may have this person, and add them to one you run.">
                          <Button size="compact-sm" variant="subtle" color="violet" onClick={() => setPlacing(u)}>
                            Organizations
                          </Button>
                        </Tooltip>
                      )}
                      {admin && !self && (
                        <>
                          {!u.disabled && (
                            <Menu position="bottom-end" withinPortal>
                              <Menu.Target>
                                <Button size="compact-sm" variant="subtle" color="slate"
                                  aria-label={`Security of ${u.email}`}>
                                  Security
                                </Button>
                              </Menu.Target>
                              <Menu.Dropdown>
                                {!u.shared && (
                                  <Menu.Item leftSection={<IconMail size={14} />} onClick={() => void sendNewPassword(u)}>
                                    {u.temporaryPassword ? 'Send the invitation again' : 'Send a new temporary password'}
                                  </Menu.Item>
                                )}
                                <Menu.Item onClick={() => void signOutAll(u)}>
                                  Sign out everywhere
                                </Menu.Item>
                                <Menu.Item disabled={!u.mfaEnrolled} onClick={() => void reset2fa(u)}>
                                  {u.mfaEnrolled ? 'Reset lost authenticator' : 'No authenticator to reset'}
                                </Menu.Item>
                              </Menu.Dropdown>
                            </Menu>
                          )}
                          <Button
                            size="compact-sm"
                            variant="subtle"
                            color={u.disabled ? 'azure' : 'rose'}
                            loading={setDisabled.isPending && setDisabled.variables?.email === u.email}
                            onClick={() => void toggle(u)}
                          >
                            {u.disabled ? 'Let back in' : 'Remove'}
                          </Button>
                        </>
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
      {handOver && <HandOverPassword outcome={handOver} onClose={() => setHandOver(null)} />}
      <Text size={FS.meta} c="dimmed">
        Removing someone stops them signing in, and locks their own accounts on every server within a
        minute. Nothing is deleted: their sessions, recordings and activity stay under their name.
      </Text>
      {renaming && <RenameLoginModal user={renaming} onClose={() => setRenaming(null)} />}
      {viewing && <PersonServersModal user={viewing} onClose={() => setViewing(null)} />}
      {placing && <WhereTheyWorkModal user={placing} onClose={() => setPlacing(null)} />}
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
  const addShared = useAddSharedAccount()
  const { data: me } = useQuery(meQuery())
  const { data: teams } = useQuery(teamsQuery())
  const [email, setEmail] = useState('')
  const [name, setName] = useState('')
  const [role, setRole] = useState<UserRole>('operator')
  const [handOver, setHandOver] = useState<TemporaryPasswordOutcome | null>(null)
  // A person who already signs in to another organization on this control
  // plane keeps their password and second step. From an organization the
  // caller runs, they are picked from a list and added in one step; from any
  // other, by email, once their own organization has allowed this one.
  const canShare = me?.multiOrg === true
  const [mode, setMode] = useState<'new' | 'other'>('new')
  const [picked, setPicked] = useState<{ email: string; displayName: string; org: string } | null>(null)
  const [byEmail, setByEmail] = useState(false)
  const [teamId, setTeamId] = useState<string | null>(null)
  const [search, setSearch] = useState('')
  const [added, setAdded] = useState<{ email: string; name: string; role: UserRole; team: string } | null>(null)
  const from = useQuery({ ...fromOrganizationsQuery(), enabled: opened && canShare && mode === 'other' })
  const canEmail = me?.canEmailPeople === true
  const here = me?.org?.name ?? 'this organization'
  const reset = () => {
    setEmail('')
    setName('')
    setRole('operator')
    setMode('new')
    setPicked(null)
    setByEmail(false)
    setTeamId(null)
    setSearch('')
    setAdded(null)
  }
  const close = () => {
    reset()
    onClose()
  }
  const orgs = from.data?.organizations ?? []
  const needle = search.trim().toLowerCase()
  const shared = mode === 'other'
  const listMode = shared && !byEmail && orgs.length > 0
  const submit = async () => {
    try {
      if (shared) {
        const who = listMode ? picked : null
        const out = await addShared.mutateAsync({
          email: who ? who.email : email,
          displayName: who ? who.displayName : name,
          role,
          ...(who ? { homeOrg: who.org } : {}),
          ...(teamId ? { teamId } : {}),
        })
        setAdded({ email: who ? who.email : email, name: who ? who.displayName : (name || email), role, team: out.team })
        return
      }
      const out = await add.mutateAsync({ email, displayName: name, role })
      if (out.emailed) {
        notifyOk('Person added', `We emailed ${out.email} a temporary password. They choose their own and set up two-step sign-in when they first sign in.`)
        close()
      } else {
        // Shown in place of the form, once; closing it forgets it.
        setHandOver(out)
        reset()
      }
    } catch (err) {
      notifyError('Not added', err)
    }
  }
  const pending = add.isPending || addShared.isPending
  if (handOver) {
    return <HandOverPassword outcome={handOver} onClose={() => { setHandOver(null); onClose() }} />
  }
  if (added) {
    return (
      <Modal opened={opened} onClose={close} title={`${added.name} is in ${here}`}>
        <Stack gap="md">
          <Text size={FS.body} c="dimmed">
            They are a {roleLabel(added.role)} here, and keep signing in where they always have. Nothing was sent to them.
          </Text>
          {added.team && added.team !== 'added' && (
            <Alert color="amber" variant="light" icon={<IconAlertTriangle size={16} />} p="xs">
              <Text size={FS.body}>They were added, but not to the team: {added.team.replace(/^not added to the team: /, '')}</Text>
            </Alert>
          )}
          <Box>
            <Text fw={600} size="sm">Give them what they need here</Text>
            <Text size={FS.meta} c="dimmed" mb={SP.tight}>
              Nothing in {here} is theirs yet{added.team === 'added' ? ' beyond what their team has' : ''}. What they have in another organization does not carry over.
            </Text>
            <GiveAccess email={added.email} onGo={close} />
          </Box>
          <Box>
            <Text fw={600} size="sm">They switch from the organization menu</Text>
            <Text size={FS.meta} c="dimmed">
              {here} is in the menu at the top right of their console now. A page they already have open picks it up within a minute.
            </Text>
          </Box>
          <Group justify="flex-end">
            <Button onClick={close}>Done</Button>
          </Group>
        </Stack>
      </Modal>
    )
  }
  const ready = shared ? (listMode ? picked !== null : email.includes('@')) : email.includes('@')
  return (
    <Modal opened={opened} onClose={close} title={`Add person to ${here}`} size={shared ? 'lg' : 'md'}>
      <Stack gap="md">
        {canShare && (
          <SegmentedControl
            fullWidth
            value={mode}
            onChange={(v) => { setMode(v as 'new' | 'other'); setPicked(null) }}
            data={[{ value: 'new', label: 'New to Argus' }, { value: 'other', label: 'From another organization' }]}
          />
        )}
        {shared && (
          <Text size={FS.body} c="dimmed">
            They keep the password and second step they already have, and switch here from the organization menu. Nothing to send them.
          </Text>
        )}
        {shared && from.isLoading && <Text size={FS.meta} c="dimmed">Loading the organizations you run…</Text>}
        {listMode ? (
          <>
            <TextInput placeholder="Search by name or email" leftSection={<IconSearch size={14} />}
              value={search} onChange={(e) => setSearch(e.currentTarget.value)} />
            <Box style={{ border: '1px solid var(--mantine-color-default-border)', borderRadius: 8, maxHeight: 300, overflowY: 'auto' }}>
              {orgs.map((o) => {
                const people = o.people.filter((p) => !needle || `${p.displayName} ${p.email}`.toLowerCase().includes(needle))
                return (
                  <Box key={o.slug}>
                    <Text size={FS.micro} fw={600} c="dimmed" tt="uppercase" px="sm" py={6}
                      style={{ background: 'var(--mantine-color-default-hover)', letterSpacing: '.05em' }}>
                      {o.name} · you are {roleLabel(o.yourRole as UserRole).toLowerCase()}
                    </Text>
                    {people.length === 0 && (
                      <Text size={FS.meta} c="dimmed" px="sm" py={8}>
                        {o.people.length === 0 ? `Everyone in ${o.name} is already here.` : 'Nobody matches.'}
                      </Text>
                    )}
                    {people.map((p) => {
                      const on = picked?.email === p.email && picked.org === o.slug
                      return (
                        <UnstyledButton key={p.email} w="100%" px="sm" py={8} aria-pressed={on}
                          onClick={() => setPicked({ email: p.email, displayName: p.displayName, org: o.slug })}
                          style={{ display: 'block', borderTop: '1px solid var(--mantine-color-default-border)',
                            background: on ? 'var(--mantine-color-blue-light)' : undefined }}>
                          <Group gap="sm" wrap="nowrap">
                            <Radio checked={on} readOnly tabIndex={-1} aria-hidden />
                            <Avatar size="sm" radius="xl">{initials(p.displayName || p.email)}</Avatar>
                            <Box style={{ minWidth: 0, flex: 1 }}>
                              <Text size="sm" fw={500} truncate>{p.displayName || p.email}</Text>
                              <Text size={FS.meta} c="dimmed" truncate>{p.email}</Text>
                            </Box>
                            <Badge variant="light" color="slate">{roleLabel(p.role as UserRole)}</Badge>
                          </Group>
                        </UnstyledButton>
                      )
                    })}
                  </Box>
                )
              })}
            </Box>
          </>
        ) : (
          <>
            <TextInput label="Email" type="email" data-autofocus value={email} onChange={(e) => setEmail(e.currentTarget.value)} />
            <TextInput label="Name" description="Optional" value={name} onChange={(e) => setName(e.currentTarget.value)} />
          </>
        )}
        <SimpleGrid cols={shared ? 2 : 1}>
          <Select
            label={`Role in ${here}`}
            allowDeselect={false}
            data={ROLES.filter((r) => r.value !== 'owner' || me?.role === 'owner').map((r) => ({
              value: r.value,
              label: shared ? r.label : `${r.label}: ${r.what}`,
            }))}
            value={role}
            onChange={(v) => setRole((v ?? 'operator') as UserRole)}
          />
          {shared && (
            <Select label="Team" placeholder="Optional" clearable value={teamId}
              data={(teams ?? []).map((t) => ({ value: t.id, label: t.name }))}
              onChange={setTeamId} />
          )}
        </SimpleGrid>
        {shared && listMode && picked && (
          <Text size={FS.meta} c="dimmed">
            {orgs.find((o) => o.slug === picked.org)?.name} will list {here} under where {picked.displayName || picked.email} works,
            and both audit logs record that you added them.
          </Text>
        )}
        {shared && (
          <Text size={FS.meta} c="dimmed">
            {listMode ? (
              <>Someone from an organization you don&rsquo;t run? <Anchor size={FS.meta} onClick={() => { setByEmail(true); setPicked(null) }}>Enter their email</Anchor>. </>
            ) : orgs.length > 0 ? (
              <><Anchor size={FS.meta} onClick={() => setByEmail(false)}>Pick from the organizations you run</Anchor>. </>
            ) : null}
            {!listMode && <>Their own organization has to have allowed {here} first, under Teams, People, Organizations.</>}
          </Text>
        )}
        {!shared && (canEmail ? (
          <Alert color="azure" variant="light" icon={<IconMail size={16} />} p="xs">
            <Text size={FS.body}>
              Argus emails them a temporary password. At their first sign-in they choose a strong
              password of their own, then set up two-step sign-in.
            </Text>
          </Alert>
        ) : (
          <Alert color="amber" variant="light" icon={<IconAlertTriangle size={16} />} p="xs">
            <Text size={FS.body}>
              Email is not set up, so Argus shows you their temporary password once to hand over.
              Set up email in Settings, Alerts to send it to them instead.
            </Text>
          </Alert>
        ))}
        <Group justify="flex-end">
          <Button variant="default" onClick={close}>Cancel</Button>
          <Button loading={pending} disabled={!ready} onClick={() => void submit()}>
            {shared && listMode && picked ? `Add ${picked.displayName || picked.email}` : 'Add person'}
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}

function initials(s: string): string {
  const parts = s.replace(/@.*/, '').split(/[\s._-]+/).filter(Boolean)
  return ((parts[0]?.[0] ?? '') + (parts[1]?.[0] ?? '')).toUpperCase() || '?'
}

/**
 * Where one of this organization's own people works: home, and each other
 * organization allowed to have them, by name. There is no "any organization"
 * setting. Taking one away refuses them there at their next click. An
 * organization the caller runs too is added in the same step.
 */
function WhereTheyWorkModal({ user, onClose }: { user: User; onClose: () => void }) {
  const { data, isLoading, error } = useQuery(personOrganizationsQuery(user.email))
  const allow = useAllowPersonOrganization()
  const remove = useRemovePersonOrganization()
  const [org, setOrg] = useState<string | null>(null)
  const [role, setRole] = useState<UserRole>('operator')
  const [byId, setById] = useState(false)
  const [slug, setSlug] = useState('')
  const name = user.displayName || user.email
  const target = data?.canAddTo.find((o) => o.slug === org) ?? data?.canAddTo[0]
  const addHere = async () => {
    if (!target) return
    try {
      const out = await allow.mutateAsync({ email: user.email, org: target.slug, role })
      notifyOk(out.added ? `Added to ${target.name}` : `${target.name} allowed`,
        out.added
          ? `${name} is a ${roleLabel(role)} in ${target.name} now, with the password they already have. Give them what they need there from ${target.name}.`
          : `${target.name}'s admin can add ${name} now.`)
      setOrg(null)
    } catch (err) {
      notifyError('Not added', err)
    }
  }
  const allowById = async () => {
    try {
      await allow.mutateAsync({ email: user.email, org: slug.trim().toLowerCase() })
      notifyOk('Organization allowed', `Its admin can add ${name} now, from Add person, by email.`)
      setSlug('')
      setById(false)
    } catch (err) {
      notifyError('Not allowed', err)
    }
  }
  const takeAway = async (o: { slug: string; name: string; added: boolean }) => {
    const what = o.added
      ? `Take ${o.name} away from ${name}? They are signed out of it at their next click, and can no longer switch to it. ${o.name} keeps its records of them.`
      : `Stop allowing ${o.name} to add ${name}?`
    if (!window.confirm(what)) return
    try {
      await remove.mutateAsync({ email: user.email, org: o.slug })
      notifyOk(`${o.name} taken away`, `${name} can no longer sign in to ${o.name}.`)
    } catch (err) {
      notifyError('Not changed', err)
    }
  }
  return (
    <Modal opened onClose={onClose} title={`Where ${name} works`} size="lg">
      <Stack gap="sm">
        {isLoading && <Text size="sm" c="dimmed">Loading…</Text>}
        {error && <Text size="sm" c="rose">{error instanceof Error ? error.message : 'Could not load.'}</Text>}
        {data && (
          <>
            <Text size={FS.body} c="dimmed">
              {name} signs in here, at {data.home.name}, with their own password and second step. Only the
              organizations below may have them. Taking one away stops them signing in there at once.
            </Text>
            {data.wasShareable && (
              <Alert color="amber" variant="light" icon={<IconAlertTriangle size={16} />} p="xs">
                <Text size={FS.body}>
                  They were open to every organization before. No organization can add them now until you pick one here.
                </Text>
              </Alert>
            )}
            <OrgLine initials={initials(data.home.name)} name={data.home.name}
              detail="Home: their password and second step are kept here"
              right={<Badge variant="light" color="slate">{roleLabel(data.home.role as UserRole)}</Badge>} />
            {data.organizations.map((o) => (
              <OrgLine key={o.slug} initials={initials(o.name)} name={o.name}
                detail={o.added
                  ? `${o.disabled ? 'Removed by that organization' : 'Added'} · allowed by ${o.grantedBy}, ${new Date(o.grantedAt).toLocaleDateString()}`
                  : `Allowed by ${o.grantedBy}, ${new Date(o.grantedAt).toLocaleDateString()}. Waiting for its admin to add them.`}
                right={
                  <Group gap="xs" wrap="nowrap">
                    {o.added
                      ? <Badge variant="light" color={o.disabled ? 'rose' : 'slate'}>{o.disabled ? 'removed there' : roleLabel((o.role ?? 'operator') as UserRole)}</Badge>
                      : <Badge variant="light" color="amber">not added yet</Badge>}
                    <Button size="compact-sm" variant="default" color="rose" c="rose"
                      loading={remove.isPending && remove.variables?.org === o.slug}
                      onClick={() => void takeAway(o)}>
                      Take away
                    </Button>
                  </Group>
                } />
            ))}
            {data.canAddTo.length > 0 && target && (
              <Card withBorder radius="md" p="md" mt="xs">
                <Text fw={600} size="sm" mb={SP.tight}>Add them to another organization</Text>
                <SimpleGrid cols={{ base: 1, sm: 2 }}>
                  <Select label="Organization" allowDeselect={false} value={target.slug}
                    data={data.canAddTo.map((o) => ({ value: o.slug, label: o.name }))}
                    onChange={setOrg} />
                  <Select label="Role there" allowDeselect={false} value={role}
                    data={ROLES.filter((r) => r.value !== 'owner' || target.yourRole === 'owner').map((r) => ({ value: r.value, label: r.label }))}
                    onChange={(v) => setRole((v ?? 'operator') as UserRole)} />
                </SimpleGrid>
                <Text size={FS.meta} c="dimmed" mt={SP.tight}>
                  You are {roleLabel(target.yourRole as UserRole).toLowerCase() === 'owner' ? 'an owner' : 'an admin'} of {target.name}, so they are added now.
                  They keep their password and second step, and nothing is sent to them.
                </Text>
                <Group justify="flex-end" mt="sm">
                  <Button loading={allow.isPending && allow.variables?.org === target.slug} onClick={() => void addHere()}>
                    Add to {target.name}
                  </Button>
                </Group>
              </Card>
            )}
            {byId ? (
              <Group align="flex-end" gap="xs">
                <TextInput label="Organization ID" description="The short name its people sign in with"
                  value={slug} onChange={(e) => setSlug(e.currentTarget.value.toLowerCase())}
                  styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)' } }} style={{ flex: 1 }} />
                <Button variant="default" onClick={() => setById(false)}>Cancel</Button>
                <Button disabled={!slug.trim()} loading={allow.isPending && !allow.variables?.role} onClick={() => void allowById()}>Allow</Button>
              </Group>
            ) : (
              <Text size={FS.meta} c="dimmed">
                An organization you do not run? <Anchor size={FS.meta} onClick={() => setById(true)}>Allow it by its ID</Anchor>, and its admin adds them there.
              </Text>
            )}
          </>
        )}
      </Stack>
    </Modal>
  )
}

function OrgLine({ initials, name, detail, right }: { initials: string; name: string; detail: string; right: React.ReactNode }) {
  return (
    <Group justify="space-between" wrap="nowrap" gap="sm" py={SP.tight}
      style={{ borderTop: '1px solid var(--mantine-color-default-border)' }}>
      <Group gap="sm" wrap="nowrap" style={{ minWidth: 0 }}>
        <Avatar size="sm" radius="sm" color="violet">{initials}</Avatar>
        <Box style={{ minWidth: 0 }}>
          <Text size="sm" fw={600} truncate>{name}</Text>
          <Text size={FS.meta} c="dimmed">{detail}</Text>
        </Box>
      </Group>
      {right}
    </Group>
  )
}

function InvitedBadge({ expiresAt }: { expiresAt?: string | null | undefined }) {
  const expired = !!expiresAt && new Date(expiresAt).getTime() <= Date.now()
  const when = expiresAt ? new Date(expiresAt).toLocaleString() : ''
  return (
    <Tooltip label={expired
      ? 'Their temporary password ran out before they used it. Send them a new one from Security.'
      : `They have not signed in yet. Their temporary password works until ${when}.`}>
      <Badge color={expired ? 'rose' : 'azure'} variant="light">{expired ? 'invitation expired' : 'invited'}</Badge>
    </Tooltip>
  )
}

/**
 * A temporary password that could not be emailed, shown once to hand over.
 * Closing it forgets it: the control plane keeps only a hash.
 */
function HandOverPassword({ outcome, onClose }: { outcome: TemporaryPasswordOutcome; onClose: () => void }) {
  const pw = outcome.temporaryPassword ?? ''
  return (
    <Modal opened onClose={onClose} title="Hand over the temporary password" closeOnClickOutside={false}>
      <Stack gap="md">
        {outcome.emailError ? (
          <Alert color="rose" variant="light" icon={<IconAlertTriangle size={16} />} p="xs">
            <Text size={FS.body}>Argus could not email it: {outcome.emailError}</Text>
          </Alert>
        ) : (
          <Text size={FS.body}>Email is not set up, so here is the temporary password for {outcome.email}.</Text>
        )}
        <Group gap="xs" wrap="nowrap" align="center">
          <Code block style={{ flex: 1, fontSize: 16, letterSpacing: 1, wordBreak: 'break-all' }}>{pw}</Code>
          <CopyButton value={pw}>
            {({ copied, copy }) => (
              <Button variant="light" leftSection={<IconCopy size={14} />} onClick={copy}>
                {copied ? 'Copied' : 'Copy'}
              </Button>
            )}
          </CopyButton>
        </Group>
        <Text size={FS.meta} c="dimmed">
          Give it to them over a channel you trust. It works until{' '}
          {new Date(outcome.temporaryPasswordExpiresAt).toLocaleString()}, once: at their first
          sign-in they must choose their own password and set up two-step sign-in. This is the only
          time it is shown.
        </Text>
        <Group justify="flex-end">
          <Button onClick={onClose}>Done</Button>
        </Group>
      </Stack>
    </Modal>
  )
}
