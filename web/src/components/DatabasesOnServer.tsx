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
import { ENGINES, engineOf } from '~/lib/engines'
import type { Asset, DBEngine, DBLabel } from '~/types/domain'

/**
 * The databases Argus opens on one server, on that server's page: adding one
 * is an engine, a name and a label, because the agent already on the host
 * does the rest as the engine's admin there -- Postgres's peer
 * authentication, Redis's own redis.conf, rabbitmqctl. No password is ever
 * typed in.
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
      description="Postgres databases, Redis and RabbitMQ on this server that people can open in the browser. Every session is recorded command by command."
    >
      {canEdit && (
        <Group justify="flex-end" mb="xs">
          <Button size="compact-xs" variant="default" leftSection={<IconPlus size={12} />} onClick={add.open}>
            Add
          </Button>
        </Group>
      )}
      {list.length === 0 ? (
        <EmptyState compact icon={IconDatabase} title="No databases on this server yet."
          description={canEdit
            ? 'Add a Postgres database, a Redis or a RabbitMQ vhost. The agent on this host signs in as its admin locally; nothing to install, no password to store.'
            : 'An admin can add one from here.'} />
      ) : (
        <Table verticalSpacing="xs">
          <Table.Tbody>
            {list.map((d) => (
              <Table.Tr key={d.id}>
                <Table.Td>
                  <Text size={FS.body} fw={600}>{d.engine === 'redis' ? `Redis · db ${d.dbname}` : d.engine === 'rabbitmq' ? `RabbitMQ · ${d.dbname}` : d.dbname}</Text>
                  <Text size={FS.micro} c="dimmed" ff="monospace">{engineOf(d.engine).label} · {d.host}:{d.port}</Text>
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
  const [engine, setEngine] = useState<DBEngine>('postgres')
  const info = ENGINES[engine]
  const [dbname, setDbname] = useState('')
  const [host, setHost] = useState('')
  const [port, setPort] = useState<number | string>('')
  const [configPath, setConfigPath] = useState('')
  const [label, setLabel] = useState<DBLabel>('production')
  const [poolerPort, setPoolerPort] = useState<number | string>('')
  const name = dbname.trim() || info.defaults.dbname
  const valid = engine === 'postgres' ? /^[A-Za-z_][A-Za-z0-9_$-]{0,62}$/.test(name)
    : engine === 'redis' ? /^\d{1,4}$/.test(name)
    : name.length > 0 && name.length <= 63 && !/["'\\]/.test(name)
  const pick = (e: DBEngine) => { setEngine(e); setDbname(''); setHost(''); setPort(''); setConfigPath('') }
  const submit = async () => {
    try {
      const db = await create.mutateAsync({
        assetId: asset.id, engine, dbname: name, label,
        port: Number(port) || info.defaults.port,
        ...(host.trim() ? { host: host.trim() } : {}),
        ...(engine === 'postgres' ? { poolerPort: Number(poolerPort) || 0 } : {}),
        ...(engine === 'redis' ? { configPath: configPath.trim() || info.defaults.configPath } : {}),
      })
      notifyOk('Added', `${info.label} ${db.dbname} on ${asset.hostname}. Give a team access from its page; nobody can open it until you do.`)
      onClose()
      setDbname('')
    } catch (err) {
      notifyError('Not added', err)
    }
  }
  return (
    <Modal opened={opened} onClose={onClose} title={`Add to ${asset.hostname.split('.')[0]}`} centered>
      <Stack gap="sm">
        <SegmentedControl value={engine} onChange={(v) => pick(v as DBEngine)} fullWidth
          data={(Object.keys(ENGINES) as DBEngine[]).map((e) => ({ label: ENGINES[e].label, value: e }))} />
        <TextInput label={info.nameLabel} placeholder={info.defaults.dbname || 'orders'} value={dbname} autoFocus
          onChange={(e) => setDbname(e.currentTarget.value)} description={info.nameHint} />
        {engine !== 'postgres' && (
          <TextInput label={engine === 'redis' ? 'Address' : 'Management API address'} placeholder={info.defaults.host}
            value={host} onChange={(e) => setHost(e.currentTarget.value)}
            description={engine === 'redis'
              ? 'On this server: 127.0.0.1, or the path of its unix socket.'
              : 'On this server, with the management plugin on (rabbitmq-plugins enable rabbitmq_management).'} />
        )}
        <NumberInput label={engine === 'rabbitmq' ? 'Management port' : 'Port'} value={port} onChange={setPort} min={1} max={65535}
          placeholder={String(info.defaults.port)}
          description={`Only if it is not ${info.defaults.port}.`} />
        {engine === 'redis' && (
          <TextInput label="redis.conf" placeholder={info.defaults.configPath} value={configPath}
            onChange={(e) => setConfigPath(e.currentTarget.value)}
            description="The agent reads requirepass from it to sign in as admin. The password never leaves the server and is never stored in Argus." />
        )}
        {engine === 'rabbitmq' && (
          <Text size={FS.micro} c="dimmed">
            The agent uses rabbitmqctl on the server to give each person a user for their session only. No password is typed in or stored.
          </Text>
        )}
        {engine === 'postgres' && (
          <NumberInput label="PgBouncer port" value={poolerPort} onChange={setPoolerPort} min={1} max={65535} placeholder="none"
            description="If applications here connect through PgBouncer, sessions can too. Its address and one-time setup are on the database's Connection tab." />
        )}
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
