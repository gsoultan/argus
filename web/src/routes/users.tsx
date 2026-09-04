import { useMemo, useState } from 'react'
import {
  Alert, Badge, Box, Card, Grid, Group, Table, Text, TextInput, Tooltip,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { IconAlertTriangle, IconInfoCircle, IconKey, IconSearch } from '@tabler/icons-react'
import { PageHeader } from '~/components/Shell'
import { Mono, Stat, relTime } from '~/components/primitives'
import { usersQuery } from '~/lib/queries'
import { FS } from '~/theme'
import type { UserRole } from '~/types/domain'

export const Route = createFileRoute('/users')({
  component: Users,
  loader: ({ context }) => context.queryClient.ensureQueryData(usersQuery()),
})

const ROLE_COLOR: Record<UserRole, string> = {
  owner: 'rose',
  admin: 'amber',
  approver: 'sky',
  operator: 'teal',
  auditor: 'slate',
}

const ROLE_DESC: Record<UserRole, string> = {
  owner: 'Full control including billing and tenant deletion.',
  admin: 'Manage assets, policy and users. Cannot approve their own requests.',
  approver: 'Decide access requests. No standing access of their own.',
  operator: 'Request and open sessions within granted windows.',
  auditor: 'Read sessions, recordings and the audit log. No session access.',
}

function Users() {
  const { data: users } = useQuery(usersQuery())
  const [search, setSearch] = useState('')

  const rows = useMemo(() => {
    const needle = search.trim().toLowerCase()
    if (!needle) return users ?? []
    return (users ?? []).filter((u) =>
      `${u.displayName} ${u.email} ${u.role}`.toLowerCase().includes(needle),
    )
  }, [users, search])

  const withoutMfa = (users ?? []).filter((u) => !u.mfaEnrolled)
  const local = (users ?? []).filter((u) => !u.idpSubject)
  const approvers = (users ?? []).filter(
    (u) => u.role === 'approver' || u.role === 'admin' || u.role === 'owner',
  )

  return (
    <Box>
      <PageHeader
        title="Users & roles"
        description="Identity comes from the OIDC provider. Argus adds the authorization layer on top — and separation of duty between requester and approver."
      />

      <Box p="lg">
        {/* The page used to flag problems it offered no way to act on. It still
            cannot change a role — that is the IdP's job, deliberately — so it
            says where the change is made instead of implying it happens here. */}
        <Alert color="sky" variant="light" icon={<IconInfoCircle size={16} />} mb="md">
          <Text size="xs">
            Roles and MFA enrolment are read from your identity provider on each sign-in;
            Argus does not store passwords and cannot change them. Grant or revoke a role by
            moving the person between the mapped IdP groups, then have them sign in again.
            The one exception is the local break-glass account below, which exists precisely
            so an IdP outage does not lock you out of your own infrastructure.
          </Text>
        </Alert>

        <Grid gap="sm" mb="md">
          <Grid.Col span={{ base: 12, sm: 4 }}>
            <Stat
              label="Users"
              value={users?.length ?? 0}
              sub={`${approvers.length} can decide an access request`}
              tone="ok"
            />
          </Grid.Col>
          <Grid.Col span={{ base: 12, sm: 4 }}>
            <Stat
              label="Without MFA"
              value={withoutMfa.length}
              sub="A password alone is one phish away from a brokered root session."
              tone={withoutMfa.length > 0 ? 'warn' : 'ok'}
            />
          </Grid.Col>
          <Grid.Col span={{ base: 12, sm: 4 }}>
            <Stat
              label="Local accounts"
              value={local.length}
              sub="Outside the IdP by design, so they survive an SSO outage."
              tone={local.length > 1 ? 'warn' : 'ok'}
            />
          </Grid.Col>
        </Grid>

        {approvers.length < 2 && (users?.length ?? 0) > 0 && (
          <Alert
            color="amber"
            variant="light"
            icon={<IconAlertTriangle size={16} />}
            mb="md"
            title="Separation of duty is not enforceable"
          >
            <Text size="xs">
              Argus refuses to let anyone approve their own request. With only{' '}
              {approvers.length} account able to approve, a request from that person cannot be
              decided by anyone — and break-glass becomes the only route. Grant the approver
              role to at least one more person.
            </Text>
          </Alert>
        )}

        <TextInput
          size="xs"
          mb="sm"
          maw={340}
          placeholder="Filter by name, email or role"
          leftSection={<IconSearch size={14} />}
          value={search}
          onChange={(e) => setSearch(e.currentTarget.value)}
        />

        <Card padding={0}>
          <Table.ScrollContainer minWidth={720} type="native">
            <Table verticalSpacing={10} horizontalSpacing="md" highlightOnHover striped="even">
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>User</Table.Th>
                  <Table.Th>Role</Table.Th>
                  <Table.Th>Identity source</Table.Th>
                  <Table.Th>MFA</Table.Th>
                  <Table.Th>Last seen</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {rows.map((u) => (
                  <Table.Tr key={u.id}>
                    <Table.Td>
                      <Text size="xs" fw={500}>{u.displayName}</Text>
                      <Mono c="dimmed">{u.email}</Mono>
                    </Table.Td>
                    <Table.Td>
                      <Tooltip label={ROLE_DESC[u.role]} maw={300} multiline>
                        <Badge size="sm" color={ROLE_COLOR[u.role]} variant="light">
                          {u.role}
                        </Badge>
                      </Tooltip>
                    </Table.Td>
                    <Table.Td>
                      {u.idpSubject ? (
                        <Mono c="dimmed">{u.idpSubject}</Mono>
                      ) : (
                        <Tooltip
                          label="Local account, deliberately outside the IdP so it still works when SSO is down"
                          maw={300}
                          multiline
                        >
                          <Badge size="xs" color="rose" leftSection={<IconKey size={10} />}>
                            local break-glass
                          </Badge>
                        </Tooltip>
                      )}
                    </Table.Td>
                    <Table.Td>
                      {u.mfaEnrolled ? (
                        <Badge size="xs" color="teal" variant="light">enrolled</Badge>
                      ) : (
                        <Tooltip
                          label={
                            u.idpSubject
                              ? 'Enrol this person in your identity provider — Argus reads the claim, it cannot set it.'
                              : 'A local account without MFA is a standing password to your gateway. Enrol it or delete it.'
                          }
                          maw={300}
                          multiline
                        >
                          <Group gap={4} wrap="nowrap" style={{ cursor: 'help' }}>
                            <IconAlertTriangle size={12} className="text-rose-400" />
                            <Text size="xs" c="rose.4">not enrolled</Text>
                          </Group>
                        </Tooltip>
                      )}
                    </Table.Td>
                    <Table.Td>
                      <Text size="xs" c="dimmed">{relTime(u.lastSeenAt)}</Text>
                    </Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          </Table.ScrollContainer>
          {rows.length === 0 && (
            <Text size="xs" c="dimmed" ta="center" py="xl">No users match.</Text>
          )}
        </Card>

        <Text size={FS.micro} c="dimmed" mt="xs">
          Showing {rows.length} of {users?.length ?? 0} users.
        </Text>
      </Box>
    </Box>
  )
}
