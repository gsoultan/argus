import { Badge, Button, Group, Modal, NumberInput, SegmentedControl, Stack, Table, Text, TextInput } from '@mantine/core'
import { useDisclosure } from '@mantine/hooks'
import { useQuery } from '@tanstack/react-query'
import { IconDatabase, IconPlus } from '@tabler/icons-react'
import { useState } from 'react'
import { ButtonLink } from '~/components/links'
import { EmptyState, SectionCard } from '~/components/page'
import { isConfigured } from '~/lib/live'
import { notifyError, notifyOk } from '~/lib/notify'
import { databasesOnAssetQuery, useCreateDatabase } from '~/lib/queries'
import { FS } from '~/theme'
import type { Asset, DBLabel } from '~/types/domain'

/**
 * The databases Argus opens on one server, on that server's page: adding one
 * is a name and a label, because the agent already on the host does the rest
 * over Postgres's own peer authentication. No password is ever typed in.
 */
export function DatabasesOnServer({ asset, canEdit }: { asset: Asset; canEdit: boolean }) {
  const { data } = useQuery(databasesOnAssetQuery(asset.id))
  const [adding, add] = useDisclosure(false)
  if (!isConfigured()) return null
  const list = data?.databases ?? []
  return (
    <SectionCard
      title="Databases"
      icon={IconDatabase}
      description="Postgres databases on this server that people can open in the browser. Every session is recorded statement by statement."
    >
      {canEdit && (
        <Group justify="flex-end" mb="xs">
          <Button size="compact-xs" variant="default" leftSection={<IconPlus size={12} />} onClick={add.open}>
            Add database
          </Button>
        </Group>
      )}
      {list.length === 0 ? (
        <EmptyState compact icon={IconDatabase} title="No databases on this server yet."
          description={canEdit
            ? 'Add one by name. The agent on this host opens it as the postgres account; nothing to install, no password to store.'
            : 'An admin can add one from here.'} />
      ) : (
        <Table verticalSpacing="xs">
          <Table.Tbody>
            {list.map((d) => (
              <Table.Tr key={d.id}>
                <Table.Td>
                  <Text size={FS.body} fw={600}>{d.dbname}</Text>
                  <Text size={FS.micro} c="dimmed" ff="monospace">{d.host}:{d.port}</Text>
                </Table.Td>
                <Table.Td>
                  <Badge size="xs" variant="light" radius="sm"
                    color={{ production: 'rose', staging: 'amber', development: 'teal' }[d.label]}>{d.label}</Badge>
                </Table.Td>
                <Table.Td ta="right">
                  <Group gap="xs" justify="flex-end" wrap="nowrap">
                    {canEdit && (
                      <ButtonLink to="/databases/$dbId/settings" params={{ dbId: d.id }} size="compact-xs" variant="subtle">
                        Who can connect
                      </ButtonLink>
                    )}
                    <ButtonLink to="/databases/$dbId" params={{ dbId: d.id }} size="compact-xs" variant="default">Open</ButtonLink>
                  </Group>
                </Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      )}
      {canEdit && <AddDatabase asset={asset} opened={adding} onClose={add.close} />}
    </SectionCard>
  )
}

function AddDatabase({ asset, opened, onClose }: { asset: Asset; opened: boolean; onClose: () => void }) {
  const create = useCreateDatabase()
  const [dbname, setDbname] = useState('')
  const [port, setPort] = useState<number | string>(5432)
  const [label, setLabel] = useState<DBLabel>('production')
  const [poolerPort, setPoolerPort] = useState<number | string>('')
  const valid = /^[A-Za-z_][A-Za-z0-9_$-]{0,62}$/.test(dbname)
  const submit = async () => {
    try {
      const db = await create.mutateAsync({ assetId: asset.id, dbname, port: Number(port) || 5432, label,
        poolerPort: Number(poolerPort) || 0 })
      notifyOk('Database added', `${db.dbname} on ${asset.hostname}. Give a team access from its page; nobody can open it until you do.`)
      onClose()
      setDbname('')
    } catch (err) {
      notifyError('Not added', err)
    }
  }
  return (
    <Modal opened={opened} onClose={onClose} title={`Add a database on ${asset.hostname.split('.')[0]}`} centered>
      <Stack gap="sm">
        <TextInput label="Database name" placeholder="orders" value={dbname} autoFocus
          onChange={(e) => setDbname(e.currentTarget.value)}
          description="As Postgres knows it. The agent connects over the local socket as the postgres account." />
        <NumberInput label="Port" value={port} onChange={setPort} min={1} max={65535}
          description="Only if Postgres listens somewhere other than 5432." />
        <NumberInput label="PgBouncer port" value={poolerPort} onChange={setPoolerPort} min={1} max={65535} placeholder="none"
          description="If applications here connect through PgBouncer, sessions can too. Its address and one-time setup are on the database's Connection tab." />
        <Stack gap={4}>
          <Text size={FS.meta} fw={500}>Label</Text>
          <SegmentedControl value={label} onChange={(v) => setLabel(v as DBLabel)}
            data={[{ label: 'Production', value: 'production' }, { label: 'Staging', value: 'staging' }, { label: 'Development', value: 'development' }]} />
          <Text size={FS.micro} c="dimmed">On production, Write and above need approval each time unless you say otherwise.</Text>
        </Stack>
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button onClick={() => void submit()} loading={create.isPending} disabled={!valid}>Add</Button>
        </Group>
      </Stack>
    </Modal>
  )
}
