import {
  ActionIcon, Alert, Badge, Box, Button, Checkbox, Group, Modal, NumberInput, SegmentedControl, Select, Stack,
  Switch, Table, Text, Tooltip,
} from '@mantine/core'
import { useDisclosure } from '@mantine/hooks'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { IconInfoCircle, IconPlus, IconTrash, IconUser, IconUsersGroup } from '@tabler/icons-react'
import { useState } from 'react'
import { PageBody, PageHeader, SectionCard } from '~/components/page'
import { ButtonLink } from '~/components/links'
import { notifyError, notifyOk } from '~/lib/notify'
import { isConfigured } from '~/lib/live'
import {
  databaseQuery, dbAssignmentsQuery, meQuery, teamsQuery, useDeleteDatabase, useRemoveDBAssignment,
  useSetDBAssignment, useUpdateDatabase, usersQuery,
} from '~/lib/queries'
import { guard } from '~/lib/guard'
import { FS } from '~/theme'
import type { DBLabel, DBLevel, DBPolicy, Database } from '~/types/domain'

/**
 * Who can connect to one database, and its safety settings. Admin only.
 *
 * Levels are plain words; Argus turns them into Postgres roles. The default
 * when giving Write or above on a production database is "needs approval
 * each time", which is the one security step most people ever meet.
 */
export const Route = createFileRoute('/databases/$dbId_/settings')({
  beforeLoad: ({ context }) => guard(context.queryClient, 'admin'),
  component: DatabaseSettings,
  loader: ({ context, params }) =>
    Promise.all([
      context.queryClient.ensureQueryData(meQuery()).catch(() => undefined),
      isConfigured() ? context.queryClient.ensureQueryData(databaseQuery(params.dbId)).catch(() => undefined) : undefined,
    ]),
})

const LEVELS: { value: DBLevel; label: string; blurb: string }[] = [
  { value: 'read', label: 'Read', blurb: 'Look at data and run reports. The default.' },
  { value: 'write', label: 'Write', blurb: 'Change rows. On production, needs approval each time unless you turn that off.' },
  { value: 'owner', label: 'Owner', blurb: 'Change tables, columns and indexes too.' },
  { value: 'dba', label: 'DBA', blurb: 'Manage roles and grants, end other people’s queries. Never a Postgres superuser.' },
]

function DatabaseSettings() {
  const { dbId } = Route.useParams()
  const { data, error } = useQuery(databaseQuery(dbId))
  const { data: assignments } = useQuery(dbAssignmentsQuery(dbId))
  const { data: teams } = useQuery(teamsQuery())
  const { data: users } = useQuery(usersQuery())
  const remove = useRemoveDBAssignment()
  const del = useDeleteDatabase()
  const navigate = useNavigate()
  const [adding, add] = useDisclosure(false)
  const [tab, setTab] = useState<'who' | 'safety'>('who')

  if (error) {
    return (
      <Box><PageHeader title="Database" crumbs={[{ label: 'Databases', to: '/databases' }]} />
        <PageBody><Alert color="rose" variant="light">{(error as Error).message}</Alert></PageBody></Box>
    )
  }
  if (!data) return null
  const db = data.database
  const names = new Map((users ?? []).map((u) => [u.email, u.displayName]))

  const onRemove = async (id: string, label: string) => {
    try {
      await remove.mutateAsync({ id: dbId, assignmentId: id })
      notifyOk('Removed', `${label} can no longer open ${db.dbname}. Their Postgres role is locked at their next session end.`)
    } catch (err) {
      notifyError('Not removed', err)
    }
  }
  const onDelete = async () => {
    if (!window.confirm(`Remove ${db.dbname} from Argus? Sessions and recordings are kept; nobody can open it from here any more.`)) return
    try {
      await del.mutateAsync(dbId)
      notifyOk('Removed', `${db.dbname} is no longer in Argus.`)
      void navigate({ to: '/databases' })
    } catch (err) {
      notifyError('Not removed', err)
    }
  }

  return (
    <Box>
      <PageHeader
        title={db.dbname}
        crumbs={[{ label: 'Databases', to: '/databases' }]}
        status={<Badge variant="light" color={{ production: 'rose', staging: 'amber', development: 'teal' }[db.label]} radius="sm">{db.label}</Badge>}
        description={`On ${db.hostname}. Who can open this database and what they can do; Argus keeps a Postgres role for each person to match.`}
        actions={
          <Group gap="xs">
            <ButtonLink to="/databases/$dbId" params={{ dbId }} variant="default" size="sm">Open</ButtonLink>
            <Button size="sm" leftSection={<IconPlus size={16} />} onClick={add.open}>Give access</Button>
          </Group>
        }
      />
      <PageBody>
        <Stack gap="md">
          <SegmentedControl value={tab} onChange={(v) => setTab(v as 'who' | 'safety')}
            data={[{ label: 'Who can connect', value: 'who' }, { label: 'Safety', value: 'safety' }]} w="fit-content" />

          {tab === 'who' && (
            <Stack gap="md">
              <SectionCard title="Teams and people" description="Admins can always open it at DBA, and that is audited like everyone else.">
                {(assignments?.length ?? 0) === 0 ? (
                  <Text size={FS.meta} c="dimmed">Only admins so far. Give a team Read to start.</Text>
                ) : (
                  <Table verticalSpacing="xs">
                    <Table.Thead><Table.Tr>
                      <Table.Th></Table.Th><Table.Th>Team or person</Table.Th><Table.Th>Access</Table.Th>
                      <Table.Th>Approval</Table.Th><Table.Th></Table.Th>
                    </Table.Tr></Table.Thead>
                    <Table.Tbody>
                      {assignments?.map((g) => (
                        <Table.Tr key={g.id}>
                          <Table.Td w={40}>{g.teamId ? <IconUsersGroup size={16} /> : <IconUser size={16} />}</Table.Td>
                          <Table.Td>
                            <Text fw={600} size={FS.body}>{g.teamId ? g.teamName : names.get(g.userEmail ?? '') ?? g.userEmail}</Text>
                            <Text size={FS.micro} c="dimmed">{g.teamId ? 'Team' : g.userEmail}</Text>
                          </Table.Td>
                          <Table.Td><Badge variant="light" color={g.level === 'read' ? 'slate' : 'azure'} radius="sm">{g.level}</Badge></Table.Td>
                          <Table.Td>
                            {g.needsApproval
                              ? <Badge variant="light" color="amber" radius="sm">Needs approval each time</Badge>
                              : <Text size={FS.meta} c="dimmed">Opens straight away</Text>}
                          </Table.Td>
                          <Table.Td ta="right">
                            <Tooltip label="Remove" withArrow>
                              <ActionIcon variant="subtle" color="rose" aria-label="Remove"
                                onClick={() => void onRemove(g.id, g.teamName || g.userEmail || 'they')}>
                                <IconTrash size={16} />
                              </ActionIcon>
                            </Tooltip>
                          </Table.Td>
                        </Table.Tr>
                      ))}
                    </Table.Tbody>
                  </Table>
                )}
              </SectionCard>
              <SectionCard title="What each level means">
                <Table verticalSpacing="xs">
                  <Table.Tbody>
                    {LEVELS.map((l) => (
                      <Table.Tr key={l.value}>
                        <Table.Td w={90}><Badge variant="light" color={l.value === 'read' ? 'slate' : 'azure'} radius="sm">{l.label}</Badge></Table.Td>
                        <Table.Td><Text size={FS.meta} c="dimmed">{l.blurb}</Text></Table.Td>
                      </Table.Tr>
                    ))}
                  </Table.Tbody>
                </Table>
              </SectionCard>
            </Stack>
          )}

          {tab === 'safety' && <Safety db={db} />}

          <SectionCard title="Remove from Argus" description="Sessions and recordings are kept. Nothing is changed in Postgres; locked roles stay locked.">
            <Button color="rose" variant="light" onClick={() => void onDelete()} loading={del.isPending}>Remove {db.dbname}</Button>
          </SectionCard>
        </Stack>
        <GiveAccess dbId={dbId} db={db} opened={adding} onClose={add.close}
          teams={(teams ?? []).map((t) => ({ value: t.id, label: t.name }))}
          people={(users ?? []).map((u) => ({ value: u.email, label: `${u.displayName} · ${u.email}` }))} />
      </PageBody>
    </Box>
  )
}

function GiveAccess({ dbId, db, opened, onClose, teams, people }: {
  dbId: string; db: Database; opened: boolean; onClose: () => void
  teams: { value: string; label: string }[]; people: { value: string; label: string }[]
}) {
  const [kind, setKind] = useState<'team' | 'person'>('team')
  const [who, setWho] = useState<string | null>(null)
  const [level, setLevel] = useState<DBLevel>('read')
  const [touched, setTouched] = useState(false)
  const [approval, setApproval] = useState(false)
  const set = useSetDBAssignment()
  // Production defaults to approval for write and above, unless the admin
  // decided otherwise for this row.
  const effectiveApproval = touched ? approval : db.label === 'production' && level !== 'read'
  const submit = async () => {
    if (!who) return
    try {
      await set.mutateAsync({ id: dbId, level, needsApproval: effectiveApproval,
        ...(kind === 'team' ? { teamId: who } : { userEmail: who }) })
      notifyOk('Access given', `${level[0]!.toUpperCase() + level.slice(1)} on ${db.dbname}${effectiveApproval ? ', with approval each time' : ''}.`)
      onClose()
      setWho(null); setTouched(false); setLevel('read')
    } catch (err) {
      notifyError('Not given', err)
    }
  }
  return (
    <Modal opened={opened} onClose={onClose} title={`Give access to ${db.dbname}`} centered>
      <Stack gap="sm">
        <SegmentedControl value={kind} onChange={(v) => { setKind(v as 'team' | 'person'); setWho(null) }}
          data={[{ label: 'A team', value: 'team' }, { label: 'A person', value: 'person' }]} />
        <Select label={kind === 'team' ? 'Team' : 'Person'} data={kind === 'team' ? teams : people} value={who}
          onChange={setWho} searchable placeholder={kind === 'team' ? 'Pick a team' : 'Pick a person'} />
        <Box>
          <Text size={FS.micro} c="dimmed" mb={4}>Access</Text>
          <SegmentedControl value={level} onChange={(v) => setLevel(v as DBLevel)}
            data={LEVELS.map((l) => ({ label: l.label, value: l.value }))} />
          <Text size={FS.micro} c="dimmed" mt={4}>{LEVELS.find((l) => l.value === level)?.blurb}</Text>
        </Box>
        <Checkbox label="Needs approval each time" checked={effectiveApproval}
          onChange={(e) => { setTouched(true); setApproval(e.currentTarget.checked) }}
          description="Every session at this level waits for an approver first, and lasts as long as they allow." />
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button onClick={() => void submit()} loading={set.isPending} disabled={!who}>Give access</Button>
        </Group>
      </Stack>
    </Modal>
  )
}

function Safety({ db }: { db: Database }) {
  const update = useUpdateDatabase()
  const [label, setLabel] = useState<DBLabel>(db.label)
  const [policy, setPolicy] = useState<DBPolicy>(db.policy ?? {})
  const dirty = label !== db.label || JSON.stringify(policy) !== JSON.stringify(db.policy ?? {})
  const save = async () => {
    try {
      await update.mutateAsync({ id: db.id, label, policy })
      notifyOk('Saved', 'The new limits apply to the next session; running ones keep theirs.')
    } catch (err) {
      notifyError('Not saved', err)
    }
  }
  const num = (k: keyof DBPolicy, label: string, hint: string, def: number, unit: string) => (
    <Group justify="space-between" wrap="nowrap" key={k}>
      <Box><Text size={FS.body} fw={600}>{label}</Text><Text size={FS.micro} c="dimmed">{hint}</Text></Box>
      <NumberInput w={140} value={policy[k] || ''} placeholder={`${def.toLocaleString()} ${unit}`} min={0} suffix={` ${unit}`}
        onChange={(v) => setPolicy({ ...policy, [k]: typeof v === 'number' ? v : 0 })} aria-label={label} />
    </Group>
  )
  return (
    <Stack gap="md">
      <Alert icon={<IconInfoCircle size={16} />} color="azure" variant="light">
        Every value has a safe default. A new database is safe the moment it is added; change these only if this
        one needs something different.
      </Alert>
      <SectionCard title="Label" description="Production defaults to approval for Write and above when you give access.">
        <SegmentedControl value={label} onChange={(v) => setLabel(v as DBLabel)}
          data={[{ label: 'Production', value: 'production' }, { label: 'Staging', value: 'staging' }, { label: 'Development', value: 'development' }]} />
      </SectionCard>
      <SectionCard title="Limits">
        <Stack gap="sm">
          {num('statementTimeoutS', 'Stop a statement after', 'A runaway query ends on its own.', 60, 's')}
          {num('idleTxTimeoutS', 'End a transaction left idle after', 'So a forgotten BEGIN does not hold locks.', 120, 's')}
          {num('maxRows', 'Rows a person can see per statement', 'They can always narrow with LIMIT.', 100000, 'rows')}
          {num('idleTimeoutS', 'End idle sessions after', '', 1800, 's')}
          {num('maxSessionS', 'Longest session', 'The role’s one-time password expires with it.', 28800, 's')}
        </Stack>
      </SectionCard>
      <SectionCard title="Recording">
        <Group justify="space-between"><Box><Text size={FS.body} fw={600}>Statements</Text><Text size={FS.micro} c="dimmed">Always recorded, before they run.</Text></Box>
          <Badge variant="light" color="teal" radius="sm">Always on</Badge></Group>
        <Group justify="space-between" mt="sm"><Box><Text size={FS.body} fw={600}>Query results</Text><Text size={FS.micro} c="dimmed">Off keeps customer data out of recordings. Row counts and a hash of each result are kept.</Text></Box>
          <Switch checked={false} disabled aria-label="Record results" /></Group>
      </SectionCard>
      <Group><Button onClick={() => void save()} disabled={!dirty} loading={update.isPending}>Save</Button></Group>
    </Stack>
  )
}
