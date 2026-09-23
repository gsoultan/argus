import { useState } from 'react'
import {
  ActionIcon, Alert, Badge, Button, Drawer, Group, MultiSelect, Select, Stack,
  Table, Text, Tooltip,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconInfoCircle, IconTrash, IconUserPlus } from '@tabler/icons-react'
import { assignmentsQuery, usersQuery, useRemoveAssignment, useSetAssignment } from '~/lib/queries'
import { run } from '~/lib/notify'
import { Mono, relTime } from '~/components/primitives'
import { FS, SP } from '~/theme'
import type { Asset } from '~/types/domain'

/**
 * Who may reach one host, and as which accounts.
 *
 * The principal list is the asset's own — assigning someone an account the host
 * does not permit would produce access that looks granted here and is refused
 * at the gateway, which is the worst of both. The control plane refuses it too;
 * this only keeps the console from offering it.
 */
export function AssetAssignments({
  asset,
  opened,
  onClose,
  canEdit,
}: {
  asset: Asset
  opened: boolean
  onClose: () => void
  /** Auditors read this panel; only admins and owners change it. */
  canEdit: boolean
}) {
  const { data: assignments } = useQuery({ ...assignmentsQuery(asset.id), enabled: opened })
  const { data: users } = useQuery({ ...usersQuery(), enabled: opened && canEdit })
  const set = useSetAssignment()
  const remove = useRemoveAssignment()

  const [email, setEmail] = useState<string | null>(null)
  const [principals, setPrincipals] = useState<string[]>([])

  const assigned = new Set((assignments ?? []).map((g) => g.userEmail))
  const candidates = (users ?? [])
    .filter((u) => !u.disabled)
    .map((u) => ({
      value: u.email,
      label: u.displayName ? `${u.displayName} — ${u.email}` : u.email,
      disabled: assigned.has(u.email),
    }))

  const add = async () => {
    if (!email || principals.length === 0) return
    const ok = await run(
      () => set.mutateAsync({ assetId: asset.id, email, principals }),
      {
        failure: 'Not assigned',
        success: [
          'Assigned',
          `${email} can open ${asset.hostname} as ${principals.join(', ')}. Gateways pick it up within a minute.`,
        ],
      },
    )
    if (ok) {
      setEmail(null)
      setPrincipals([])
    }
  }

  const revoke = (userEmail: string) =>
    void run(() => remove.mutateAsync({ assetId: asset.id, email: userEmail }), {
      failure: 'Not removed',
      success: ['Access removed', `${userEmail} can no longer reach ${asset.hostname}.`],
    })

  return (
    <Drawer
      opened={opened}
      onClose={onClose}
      position="right"
      size="lg"
      title={
        <Group gap={SP.cozy}>
          <Text fw={600}>Who can reach</Text>
          <Mono>{asset.hostname}</Mono>
        </Group>
      }
    >
      <Stack gap="md">
        {asset.principals.length === 0 && (
          <Alert variant="light" color="amber" icon={<IconInfoCircle size={16} />} title="No principals">
            This host permits no accounts yet, so there is nothing to assign. Edit the
            asset and list the accounts Argus may broker a session as.
          </Alert>
        )}

        <Table highlightOnHover>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>Account</Table.Th>
              <Table.Th>May open as</Table.Th>
              <Table.Th>Assigned</Table.Th>
              {canEdit && <Table.Th />}
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {(assignments ?? []).map((g) => (
              <Table.Tr key={g.userEmail}>
                <Table.Td><Mono>{g.userEmail}</Mono></Table.Td>
                <Table.Td>
                  <Group gap={SP.tight}>
                    {g.principals.map((p) => (
                      <Badge key={p} size="xs" variant="light" color="slate">{p}</Badge>
                    ))}
                  </Group>
                </Table.Td>
                <Table.Td>
                  <Tooltip label={`by ${g.grantedBy || 'an administrator'}`}>
                    <Text size={FS.micro} c="dimmed">{relTime(g.grantedAt)}</Text>
                  </Tooltip>
                </Table.Td>
                {canEdit && (
                  <Table.Td align="right">
                    <ActionIcon
                      variant="subtle"
                      color="rose"
                      aria-label={`Remove ${g.userEmail}`}
                      loading={remove.isPending && remove.variables?.email === g.userEmail}
                      onClick={() => revoke(g.userEmail)}
                    >
                      <IconTrash size={14} />
                    </ActionIcon>
                  </Table.Td>
                )}
              </Table.Tr>
            ))}
            {(assignments ?? []).length === 0 && (
              <Table.Tr>
                <Table.Td colSpan={canEdit ? 4 : 3}>
                  {/* Says what this means rather than only that the list is
                      empty: an unassigned host is not broken, it is unreachable
                      by design until someone decides otherwise. */}
                  <Text size={FS.meta} c="dimmed">
                    Nobody is assigned. Only admins and owners can reach this host, and
                    anyone else needs an approved access request.
                  </Text>
                </Table.Td>
              </Table.Tr>
            )}
          </Table.Tbody>
        </Table>

        {canEdit && asset.principals.length > 0 && (
          <Stack gap={SP.cozy}>
            <Select
              label="Assign to"
              placeholder="Pick an account"
              searchable
              value={email}
              onChange={setEmail}
              data={candidates}
              nothingFoundMessage="No account matches"
            />
            <MultiSelect
              label="As"
              description="Only the accounts this host permits. Elevated ones still need an approved request on the day."
              placeholder="ops"
              value={principals}
              onChange={setPrincipals}
              data={asset.principals}
            />
            <Group justify="flex-end">
              <Button
                leftSection={<IconUserPlus size={14} />}
                loading={set.isPending}
                disabled={!email || principals.length === 0}
                onClick={() => void add()}
              >
                Assign
              </Button>
            </Group>
          </Stack>
        )}
      </Stack>
    </Drawer>
  )
}
