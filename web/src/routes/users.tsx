import { useMemo, useState } from 'react'
import {
  Alert, Badge, Box, Card, Grid, Group, Table, Text, TextInput, Tooltip,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { IconAlertTriangle, IconInfoCircle, IconSearch } from '@tabler/icons-react'
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
  const approvers = (users ?? []).filter(
    (u) => u.role === 'approver' || u.role === 'admin' || u.role === 'owner',
  )

  return (
    <Box>
      <PageHeader
        title="Users & roles"
        description="Argus holds its own accounts and the authorization layer on top — and separation of duty between requester and approver."
      />

      <Box p="lg">
        {/* The page used to flag problems it offered no way to act on. It still
            cannot change a role — there is no endpoint for it — so it says
            where the change is made instead of implying it happens here. */}
        <Alert color="sky" variant="light" icon={<IconInfoCircle size={16} />} mb="md">
          <Text size="xs">
            Accounts live in Argus. Roles are set on the host with{' '}
            <Text span ff="monospace" inherit>argus-control users add --role</Text>, not from
            this page, so granting or revoking one is an action with a shell audit trail
            behind it. MFA is enrolled by the account holder from their own profile.
          </Text>
        </Alert>

        <Grid gap="sm" mb="md">
          <Grid.Col span={{ base: 12, sm: 6 }}>
            <Stat
              label="Users"
              value={users?.length ?? 0}
              sub={`${approvers.length} can decide an access request`}
              tone="ok"
            />
          </Grid.Col>
          <Grid.Col span={{ base: 12, sm: 6 }}>
            <Stat
              label="Without MFA"
              value={withoutMfa.length}
              sub="A password alone is one phish away from a brokered root session."
              tone={withoutMfa.length > 0 ? 'warn' : 'ok'}
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
                      {u.mfaEnrolled ? (
                        <Badge size="xs" color="teal" variant="light">enrolled</Badge>
                      ) : (
                        <Tooltip
                          label="An account without MFA is a standing password to your gateway. Enrol it or delete it."
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
