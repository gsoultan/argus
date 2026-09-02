import { Badge, Box, Card, Group, Table, Text, Tooltip } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { IconAlertTriangle, IconKey } from '@tabler/icons-react'
import { PageHeader } from '~/components/Shell'
import { Mono, relTime } from '~/components/primitives'
import { usersQuery } from '~/lib/queries'
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

  return (
    <Box>
      <PageHeader
        title="Users & roles"
        description="Identity comes from the OIDC provider. Argus adds the authorization layer on top — and separation of duty between requester and approver."
      />

      <Box p="lg">
        <Card padding={0}>
          <Table verticalSpacing={9} horizontalSpacing="md" highlightOnHover striped="even">
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
              {users?.map((u) => (
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
                      <Tooltip label="Local account, deliberately outside the IdP so it still works when SSO is down">
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
                      <Group gap={4}>
                        <IconAlertTriangle size={12} className="text-rose-400" />
                        <Text size="xs" c="rose.4">not enrolled</Text>
                      </Group>
                    )}
                  </Table.Td>
                  <Table.Td>
                    <Text size="xs" c="dimmed">{relTime(u.lastSeenAt)}</Text>
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Card>
      </Box>
    </Box>
  )
}
