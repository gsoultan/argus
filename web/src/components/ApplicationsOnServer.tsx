import { useState } from 'react'
import { Badge, Box, Button, Group, Stack, Table, Tabs, Text, Tooltip } from '@mantine/core'
import { ButtonLink } from '~/components/links'
import { isBroker } from '~/lib/engines'
import { useMediaQuery } from '@mantine/hooks'
import { useQuery } from '@tanstack/react-query'
import { IconApps, IconDatabasePlus } from '@tabler/icons-react'
import { AddDatabase } from '~/components/DatabasesOnServer'
import { InstalledSoftware } from '~/components/server/InstalledSoftware'
import { EmptyState, SectionCard } from '~/components/page'
import { absTime, relTime } from '~/components/primitives'
import { isConfigured } from '~/lib/live'
import { applicationsQuery, databasesOnAssetQuery } from '~/lib/queries'
import { FS, SP } from '~/theme'
import type { Asset, DBEngine, Database, ServerApp, ServerAppListen } from '~/types/domain'

/**
 * What runs on a server, as its agent found it: databases, brokers, web
 * servers and containers, the ports each listens on, and whether those ports
 * are reachable from other machines. The agent reads names, versions and
 * ports only; it never reads a command line, an environment or a config
 * file, and it never connects to what it finds.
 *
 * A Postgres found here is one step from being a database people open in the
 * browser: the add form opens with its port, and PgBouncer's, filled in.
 */
export function ApplicationsOnServer({ asset, canEdit }: { asset: Asset; canEdit: boolean }) {
  const { data } = useQuery(applicationsQuery(asset.id))
  const { data: dbs } = useQuery(databasesOnAssetQuery(asset.id))
  const [adding, setAdding] = useState<{ engine: DBEngine; port?: number; poolerPort?: number; configPath?: string } | null>(null)
  const phone = useMediaQuery('(max-width: 48em)') ?? false
  const [tab, setTab] = useState<string | null>('apps')

  if (asset.protocol === 'rdp') return null
  const apps = data?.applications ?? []
  const listeners = data?.listeners ?? []
  const containers = data?.containers ?? []
  const services = data?.services ?? []
  // A database already in Argus says so by name, matched on engine and port:
  // the agent reports ports, and a database in Argus is kept by port too.
  const addedOf = (engine: DBEngine) => new Map((dbs?.databases ?? []).filter((d) => d.engine === engine).map((d) => [d.port, d]))
  const pooler = apps.find((a) => a.kind === 'pgbouncer' && !a.container && a.listen.length > 0)

  return (
    <SectionCard
      title="What runs here"
      icon={IconApps}
      description={data?.reported
        ? <>As the agent found it {data.checkedAt ? <span title={absTime(data.checkedAt)}>{relTime(data.checkedAt)}</span> : 'recently'}: names, versions and ports only. It reads no command lines, environments or config files.</>
        : undefined}
      badge={data?.reported ? <Badge size="xs" variant="light" color="slate">{apps.length}</Badge> : undefined}
    >
      {!data ? null : !data.reported ? (
        <EmptyState compact icon={IconApps} title="Not reported yet."
          description={asset.agentState === 'absent'
            ? 'This server has no agent. Install one and it reports what runs here within a few minutes.'
            : 'The agent on this server has not reported what runs on it. An agent older than this console does not look; upgrading it is enough.'} />
      ) : (
        <Tabs value={tab} onChange={setTab} variant="pills" radius="sm">
          <Tabs.List mb="xs" className="argus-tabs-scroll">
            <Tabs.Tab value="apps" fz={FS.meta}>Applications {apps.length}</Tabs.Tab>
            {isConfigured() && (asset.protocol ?? 'ssh') === 'ssh' && (
              <Tabs.Tab value="installed" fz={FS.meta}>Installed</Tabs.Tab>
            )}
            <Tabs.Tab value="ports" fz={FS.meta}>Listening ports {listeners.length}</Tabs.Tab>
            <Tabs.Tab value="containers" fz={FS.meta} disabled={!data.sources.some((s) => s === 'docker' || s === 'podman')}>
              Containers {containers.length}
            </Tabs.Tab>
            <Tabs.Tab value="services" fz={FS.meta} disabled={!data.sources.includes('systemd')}>Services {services.length}</Tabs.Tab>
          </Tabs.List>

          <Tabs.Panel value="apps">
            {apps.length === 0 ? (
              <EmptyState compact icon={IconApps} title="Nothing Argus recognises."
                description="No known database, broker, web server or container runtime. Every listening port is still on the next tab." />
            ) : (
              <Table.ScrollContainer minWidth={0} type="native">
                <Table verticalSpacing="xs">
                  <Table.Tbody>
                    {apps.map((a) => (
                      <Table.Tr key={a.kind + '/' + a.container}>
                        <Table.Td>
                          <Group gap={SP.snug} wrap="nowrap">
                            <Text size={FS.body} fw={600}>{a.name}</Text>
                            {a.version && (
                              <Text size={FS.micro} c="dimmed" ff="monospace" title={a.version}>{a.container ? a.version : shortVersion(a.version)}</Text>
                            )}
                          </Group>
                          <Text size={FS.micro} c="dimmed">{whereFrom(a)}</Text>
                          {/* On a phone the ports go under the name, and the state column goes. */}
                          {phone && <Box mt={SP.tight}><Listens listen={a.listen} /></Box>}
                        </Table.Td>
                        <Table.Td visibleFrom="sm">
                          <Badge size="xs" variant="light" radius="sm" color={a.running ? 'teal' : 'slate'}>
                            {a.running ? 'running' : 'installed, not running'}
                          </Badge>
                        </Table.Td>
                        {!phone && <Table.Td><Listens listen={a.listen} /></Table.Td>}
                        <Table.Td ta="right">
                          {canEdit && isConfigured() && a.running && engineOfApp(a.kind) && (
                            <AddFound app={a} engine={engineOfApp(a.kind)!} added={addedOf(engineOfApp(a.kind)!)}
                              onAdd={(port) => setAdding({ engine: engineOfApp(a.kind)!, port,
                                poolerPort: a.kind === 'postgres' ? pooler?.listen[0]?.port : undefined,
                                configPath: a.kind === 'valkey' ? VALKEY_CONF : undefined })} />
                          )}
                        </Table.Td>
                      </Table.Tr>
                    ))}
                  </Table.Tbody>
                </Table>
              </Table.ScrollContainer>
            )}
          </Tabs.Panel>

          {isConfigured() && (asset.protocol ?? 'ssh') === 'ssh' && (
            <Tabs.Panel value="installed">
              <InstalledSoftware asset={asset} canEdit={canEdit} apps={apps} active={tab === 'installed'} />
            </Tabs.Panel>
          )}

          <Tabs.Panel value="ports">
            <Table.ScrollContainer minWidth={0} type="native">
              <Table verticalSpacing={4}>
                <Table.Thead>
                  <Table.Tr><Table.Th>Port</Table.Th><Table.Th>Address</Table.Th><Table.Th>Process</Table.Th><Table.Th visibleFrom="sm">Service</Table.Th></Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {listeners.map((l) => (
                    <Table.Tr key={l.address + ':' + l.port}>
                      <Table.Td><Text size={FS.meta} ff="monospace" fw={600}>{l.port}</Text></Table.Td>
                      <Table.Td><Reach l={l} /></Table.Td>
                      <Table.Td><Text size={FS.meta} ff="monospace">{l.process || '—'}</Text></Table.Td>
                      <Table.Td visibleFrom="sm"><Text size={FS.micro} c="dimmed" ff="monospace">{l.unit || ''}</Text></Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            </Table.ScrollContainer>
          </Tabs.Panel>

          <Tabs.Panel value="containers">
            {containers.length === 0 ? (
              <Text size={FS.meta} c="dimmed">No containers.</Text>
            ) : (
              <Table.ScrollContainer minWidth={0} type="native">
                <Table verticalSpacing={4}>
                  <Table.Tbody>
                    {containers.map((c) => (
                      <Table.Tr key={c.name}>
                        <Table.Td>
                          <Text size={FS.meta} fw={600}>{c.name}</Text>
                          <Text size={FS.micro} c="dimmed" ff="monospace">{c.image}</Text>
                        </Table.Td>
                        <Table.Td>
                          <Badge size="xs" variant="light" radius="sm" color={c.state === 'running' ? 'teal' : 'slate'}>{c.state}</Badge>
                        </Table.Td>
                        <Table.Td><Listens listen={c.ports} /></Table.Td>
                      </Table.Tr>
                    ))}
                  </Table.Tbody>
                </Table>
              </Table.ScrollContainer>
            )}
          </Tabs.Panel>

          <Tabs.Panel value="services">
            <Group gap={SP.tight}>
              {services.map((s) => (
                <Badge key={s} size="xs" variant="outline" color="slate" tt="none" ff="monospace">{s.replace(/\.service$/, '')}</Badge>
              ))}
            </Group>
          </Tabs.Panel>
        </Tabs>
      )}
      {adding && (
        <AddDatabase asset={asset} opened onClose={() => setAdding(null)} initialEngine={adding.engine}
          initialPort={adding.port} initialPoolerPort={adding.poolerPort} initialConfigPath={adding.configPath} />
      )}
    </SectionCard>
  )
}

/**
 * The upstream version from a package's: no epoch, no distribution revision.
 * 5:7.0.15-1ubuntu0.24.04.5 is 7.0.15; the whole string is in the tooltip.
 */
export function shortVersion(v: string): string {
  const noEpoch = v.replace(/^\d+:/, '')
  const dash = noEpoch.lastIndexOf('-')
  return dash > 0 ? noEpoch.slice(0, dash) : noEpoch
}

/** Where an application comes from, in a phrase. */
function whereFrom(a: ServerApp): string {
  if (a.container) return `container ${a.container} · ${a.image}`
  const parts = [a.unit && `service ${a.unit.replace(/\.service$/, '')}`, a.package && `package ${a.package}`]
  return parts.filter(Boolean).join(' · ') || (a.process ? `process ${a.process}` : '')
}

/** The ports something listens on, each marked by who can reach it. */
function Listens({ listen }: { listen: ServerAppListen[] }) {
  if (listen.length === 0) return <Text size={FS.micro} c="dimmed">no ports</Text>
  return (
    <Stack gap={2}>
      {listen.map((l) => <Reach key={l.address + ':' + l.port} l={l} withPort />)}
    </Stack>
  )
}

function Reach({ l, withPort = false }: { l: ServerAppListen; withPort?: boolean }) {
  const addr = l.address.includes(':') ? `[${l.address}]` : l.address
  return (
    <Group gap={SP.tight} wrap="nowrap">
      <Text size={FS.micro} ff="monospace">{withPort ? `${addr}:${l.port}` : l.address}</Text>
      <Tooltip label={l.localOnly
        ? 'Loopback: only programs on this server can connect.'
        : 'Other machines can connect, subject to any firewall in between.'}>
        <Badge size="xs" variant={l.localOnly ? 'outline' : 'light'} radius="sm" color={l.localOnly ? 'slate' : 'amber'} tt="none">
          {l.localOnly ? 'this server only' : 'network'}
        </Badge>
      </Tooltip>
    </Group>
  )
}

/**
 * The engine an app the agent found is added as. MariaDB is MySQL's. Valkey
 * speaks Redis's protocol and ACLs, so it is a redis entry; the agent reads
 * its admin password from Valkey's own config file.
 */
const APP_ENGINES: Record<string, DBEngine> = {
  postgres: 'postgres', mysql: 'mysql', mariadb: 'mysql', mongodb: 'mongodb', redis: 'redis', valkey: 'redis', rabbitmq: 'rabbitmq', kafka: 'kafka',
}
function engineOfApp(kind: string): DBEngine | undefined {
  return APP_ENGINES[kind]
}
const VALKEY_CONF = '/etc/valkey/valkey.conf'

/**
 * Adds a Postgres, MySQL, MariaDB, Redis, RabbitMQ or Kafka the agent found. A containerised one is
 * not offered: Argus opens it as the host's own admin over a local socket or
 * the server's own tools, which a container does not share.
 *
 * Which port the form starts from: Postgres and Redis each one they listen
 * on. RabbitMQ is reached through its management API, so 15672 when it
 * listens there and otherwise the form's default; its AMQP port is not it.
 * Kafka from the first port it listens on; 9092 when it listens there.
 * A Redis is one entry per server, so one already in Argus is not offered again.
 */
function AddFound({ app, engine, added, onAdd }: {
  app: ServerApp; engine: DBEngine; added: Map<number, Database>; onAdd: (port?: number) => void
}) {
  const what = engine === 'postgres' ? 'postgres account' : engine === 'mysql' ? 'MySQL root account'
    : engine === 'mongodb' ? 'MongoDB admin file' : engine === 'redis' ? `${app.name} admin` : engine === 'kafka' ? 'Kafka admin' : 'rabbitmqctl'
  if (app.container) {
    return (
      <Tooltip multiline w={260} label={`Argus opens this through the server's own ${what}. Inside a container that is not reachable, so it cannot be added from here yet.`}>
        <Text size={FS.micro} c="dimmed">in a container</Text>
      </Tooltip>
    )
  }
  const listening = [...new Set(app.listen.map((l) => l.port))].sort((a, b) => a - b)
  const ports: (number | undefined)[] = engine === 'rabbitmq'
    ? [listening.includes(15672) ? 15672 : undefined]
    : engine === 'kafka' ? [listening.includes(9092) ? 9092 : listening[0]]
    : listening.length ? listening : [undefined]
  const inArgus = (p: number | undefined) =>
    engine === 'redis' || isBroker(engine) ? [...added.values()][0]
      : added.get(p ?? (engine === 'mysql' ? 3306 : engine === 'mongodb' ? 27017 : 5432))
  return (
    <Stack gap={4} align="flex-end">
      {ports.map((p) => {
        const db = inArgus(p)
        const again = engine !== 'redis' && db
        return (
          <Group key={p ?? 'default'} gap={SP.tight} justify="flex-end" wrap="wrap">
            {db && (
              <ButtonLink {...(isBroker(db.engine) ? { to: '/brokers/$brokerId', params: { brokerId: db.id } } as const : { to: '/databases/$dbId', params: { dbId: db.id } } as const)}
                size="compact-xs" variant="light" color="teal">
                In Argus{engine === 'redis' ? '' : ` as ${db.dbname}`}
              </ButtonLink>
            )}
            {(!db || again) && (
              <Button size="compact-xs" variant={db ? 'subtle' : 'default'}
                leftSection={<IconDatabasePlus size={12} />} onClick={() => onAdd(p)}>
                {db ? 'Add another' : engine === 'rabbitmq' ? 'Add a vhost' : engine === 'kafka' ? 'Add topics' : 'Add as database'}{ports.length > 1 && p ? ` · ${p}` : ''}
              </Button>
            )}
          </Group>
        )
      })}
    </Stack>
  )
}
