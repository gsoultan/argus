import { useState } from 'react'
import {
  Alert, Badge, Button, Group, Menu, Modal, PasswordInput, Skeleton, Stack, Table, Text,
  TextInput, ActionIcon,
} from '@mantine/core'
import { useDisclosure } from '@mantine/hooks'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  IconAlertTriangle, IconBuilding, IconDots, IconPlus, IconPower, IconRefresh,
} from '@tabler/icons-react'
import { SectionCard } from '~/components/page'
import { Mono, relTime } from '~/components/primitives'
import { generatePassword } from '~/lib/password'
import { notifyError, notifyOk } from '~/lib/notify'
import {
  createOrganization, organizations, setOrganizationDisabled, type Organization,
} from '~/lib/live'
import { FS } from '~/theme'

/**
 * Organizations sharing this control plane.
 *
 * Shown to an owner of the default organization only. It lists each
 * organization by name with two counts, and offers to add one or switch one
 * off. It deliberately shows nothing from inside an organization: no servers,
 * no people, no sessions. A platform owner who needs to see inside one is
 * given an account there by its owner, like anyone else.
 */
const orgsKey = ['platform', 'organizations'] as const

export function Organizations() {
  const qc = useQueryClient()
  const { data, isPending, error } = useQuery({ queryKey: orgsKey, queryFn: organizations })
  const [adding, addModal] = useDisclosure(false)
  const [confirm, setConfirm] = useState<Organization | null>(null)

  const toggle = useMutation({
    mutationFn: ({ id, disabled }: { id: string; disabled: boolean }) => setOrganizationDisabled(id, disabled),
    onSuccess: (org) => {
      void qc.invalidateQueries({ queryKey: orgsKey })
      notifyOk(org.disabledAt ? 'Organization disabled' : 'Organization enabled',
        org.disabledAt
          ? `${org.name} can no longer sign in; its agents are disconnected and its data is kept.`
          : `${org.name} can sign in again.`)
    },
    onError: (err) => notifyError('Could not change the organization', err),
  })

  return (
    <>
      <SectionCard
        title="Organizations"
        icon={IconBuilding}
        iconColor="violet"
        description="Separate organizations sharing this control plane. Each has its own servers, people, approvals, recordings and audit log, and none can see another's."
        action={
          <Button size="xs" leftSection={<IconPlus size={14} />} onClick={addModal.open}>
            New organization
          </Button>
        }
        flush
      >
        {error ? (
          <Alert color="rose" variant="light" icon={<IconAlertTriangle size={16} />} m="md">
            <Text size="xs">{error instanceof Error ? error.message : 'Could not list organizations.'}</Text>
          </Alert>
        ) : isPending ? (
          <Skeleton h={120} m="md" />
        ) : (
          <Table verticalSpacing="xs" highlightOnHover>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Organization</Table.Th>
                <Table.Th>Signs in as</Table.Th>
                <Table.Th ta="right">Servers</Table.Th>
                <Table.Th ta="right">People</Table.Th>
                <Table.Th>Created</Table.Th>
                <Table.Th />
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {(data ?? []).map((o) => (
                <Table.Tr key={o.id} style={o.disabledAt ? { opacity: 0.6 } : undefined}>
                  <Table.Td>
                    <Group gap={6} wrap="nowrap">
                      <Text size={FS.body} fw={600}>{o.name}</Text>
                      {o.default && <Badge size="xs" variant="light" color="slate">default</Badge>}
                      {o.disabledAt && <Badge size="xs" variant="light" color="rose">disabled</Badge>}
                    </Group>
                  </Table.Td>
                  <Table.Td><Mono>{o.default ? '(blank)' : o.slug}</Mono></Table.Td>
                  <Table.Td ta="right">{o.counts ? o.counts.servers : '—'}</Table.Td>
                  <Table.Td ta="right">{o.counts ? o.counts.people : '—'}</Table.Td>
                  <Table.Td>
                    <Text size={FS.meta} c="dimmed">
                      {o.createdAt ? relTime(o.createdAt) : '—'}
                      {o.createdBy && o.createdBy !== 'system' ? ` by ${o.createdBy}` : ''}
                    </Text>
                  </Table.Td>
                  <Table.Td ta="right">
                    {!o.default && (
                      <Menu position="bottom-end" withinPortal>
                        <Menu.Target>
                          <ActionIcon variant="subtle" color="slate" aria-label={`Actions for ${o.name}`}>
                            <IconDots size={16} />
                          </ActionIcon>
                        </Menu.Target>
                        <Menu.Dropdown>
                          {o.disabledAt ? (
                            <Menu.Item
                              leftSection={<IconRefresh size={14} />}
                              onClick={() => toggle.mutate({ id: o.id, disabled: false })}
                            >
                              Enable again
                            </Menu.Item>
                          ) : (
                            <Menu.Item
                              color="rose"
                              leftSection={<IconPower size={14} />}
                              onClick={() => setConfirm(o)}
                            >
                              Disable
                            </Menu.Item>
                          )}
                        </Menu.Dropdown>
                      </Menu>
                    )}
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        )}
      </SectionCard>

      <NewOrganizationModal
        opened={adding}
        onClose={addModal.close}
        onCreated={() => void qc.invalidateQueries({ queryKey: orgsKey })}
      />

      <Modal opened={confirm !== null} onClose={() => setConfirm(null)} title="Disable organization" size="md">
        {confirm && (
          <Stack gap="sm">
            <Text size={FS.body} lh={1.5}>
              Nobody in <b>{confirm.name}</b> will be able to sign in, its agents will be
              disconnected, and sessions through it will be refused. Its servers, people,
              recordings and audit log are kept, and it can be enabled again here.
            </Text>
            <Group justify="flex-end">
              <Button variant="default" size="xs" onClick={() => setConfirm(null)}>Keep it on</Button>
              <Button
                color="rose"
                size="xs"
                loading={toggle.isPending}
                onClick={() => {
                  toggle.mutate({ id: confirm.id, disabled: true }, { onSettled: () => setConfirm(null) })
                }}
              >
                Disable {confirm.name}
              </Button>
            </Group>
          </Stack>
        )}
      </Modal>
    </>
  )
}

function NewOrganizationModal({ opened, onClose, onCreated }: {
  opened: boolean
  onClose: () => void
  onCreated: () => void
}) {
  const [name, setName] = useState('')
  const [slug, setSlug] = useState('')
  const [ownerEmail, setOwnerEmail] = useState('')
  const [ownerName, setOwnerName] = useState('')
  const [password, setPassword] = useState('')
  const [created, setCreated] = useState<Organization | null>(null)

  const reset = () => {
    setName(''); setSlug(''); setOwnerEmail(''); setOwnerName(''); setPassword(''); setCreated(null)
  }
  const close = () => { reset(); onClose() }

  const create = useMutation({
    mutationFn: createOrganization,
    onSuccess: (org) => { setCreated(org); onCreated() },
    onError: (err) => notifyError('Could not create the organization', err),
  })

  // The short name follows the display name until it is edited by hand.
  const slugFrom = (n: string) => n.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 40)
  const [slugTouched, setSlugTouched] = useState(false)

  const valid = name.trim() && /^[a-z0-9][a-z0-9-]{0,38}[a-z0-9]$/.test(slug) &&
    ownerEmail.includes('@') && password.length >= 12

  return (
    <Modal opened={opened} onClose={close} title="New organization" size="md">
      {created ? (
        <Stack gap="sm">
          <Alert color="teal" variant="light">
            <Text size={FS.body} lh={1.5}>
              <b>{created.name}</b> is ready. Its owner signs in at this console with organization{' '}
              <Mono>{created.slug}</Mono>, the email <Mono>{ownerEmail}</Mono> and the password below.
              Pass the password on now; it is not shown again.
            </Text>
          </Alert>
          <TextInput readOnly value={password} label="Owner's password" styles={{ input: { fontFamily: 'monospace' } }} />
          <Group justify="flex-end">
            <Button size="xs" onClick={close}>Done</Button>
          </Group>
        </Stack>
      ) : (
        <form
          onSubmit={(e) => {
            e.preventDefault()
            if (!valid) return
            create.mutate({ name: name.trim(), slug, ownerEmail: ownerEmail.trim(), ownerName: ownerName.trim(), ownerPassword: password })
          }}
        >
          <Stack gap="sm">
            <TextInput
              label="Name"
              placeholder="Northwind Traders"
              value={name}
              autoFocus
              onChange={(e) => {
                setName(e.currentTarget.value)
                if (!slugTouched) setSlug(slugFrom(e.currentTarget.value))
              }}
            />
            <TextInput
              label="Short name"
              description="What its people type at sign-in. Lowercase letters, digits and hyphens; it cannot be changed later."
              value={slug}
              onChange={(e) => { setSlugTouched(true); setSlug(e.currentTarget.value.toLowerCase()) }}
              error={slug && !/^[a-z0-9][a-z0-9-]{0,38}[a-z0-9]$/.test(slug) ? '2 to 40 characters: a-z, 0-9, hyphens' : undefined}
            />
            <Text size={FS.meta} fw={600} mt="xs">First owner</Text>
            <TextInput label="Email" type="email" value={ownerEmail} onChange={(e) => setOwnerEmail(e.currentTarget.value)} />
            <TextInput label="Display name" value={ownerName} onChange={(e) => setOwnerName(e.currentTarget.value)} />
            <PasswordInput
              label="Password"
              description="At least 12 characters. Shown once after the organization is created."
              value={password}
              onChange={(e) => setPassword(e.currentTarget.value)}
              rightSection={
                <ActionIcon variant="subtle" color="slate" aria-label="Generate a password" onClick={() => setPassword(generatePassword())}>
                  <IconRefresh size={14} />
                </ActionIcon>
              }
            />
            <Group justify="flex-end" mt="xs">
              <Button variant="default" size="xs" onClick={close}>Cancel</Button>
              <Button type="submit" size="xs" loading={create.isPending} disabled={!valid}>
                Create organization
              </Button>
            </Group>
          </Stack>
        </form>
      )}
    </Modal>
  )
}
