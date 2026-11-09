import { useState, type ReactNode } from 'react'
import { isBroker, isSQLEngine, levelName } from '~/lib/engines'
import {
  Alert, Anchor, Badge, Box, Button, Checkbox, Divider, Group, Modal, MultiSelect, Paper, Radio, SegmentedControl, Stack, Switch, Text,
  ThemeIcon,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconArrowRight, IconCheck, IconClockHour4, IconEye, IconLogin2, IconPencil, IconUsersGroup } from '@tabler/icons-react'
import { EnvBadge, People } from '~/components/ChangeImpact'
import { SectionCard } from '~/components/page'
import { LABEL_NAME, approvalIsDefault, shortName, writesNeedApproval } from '~/lib/changeApproval'
import { notifyError, notifyOk } from '~/lib/notify'
import { changeApprovalQuery, databasesOnAssetQuery, useSetDBAssignment } from '~/lib/queries'
import { useGiveDBAccess } from '~/lib/dbAccessQueries'
import { EngineMark } from '~/components/EngineMark'
import { targetName } from '~/lib/dbTarget'
import { FS } from '~/theme'
import type { DBLabel, DBLevel, DBPolicy, Database } from '~/types/domain'

/*
A database's approvals, as an admin sets them: the Approvals card under
Safety (whether a change waits, and who decides it), and Give access, which
says in plain words what a grant lets someone do on this database and what
waits.
*/

/** One line of what a grant lets someone do, and whether it waits. */
function CanDo({ icon, title, waits, detail }: { icon: ReactNode; title: string; waits: boolean | null; detail: string }) {
  return (
    <Group gap="sm" wrap="nowrap" align="flex-start">
      <ThemeIcon size={26} radius="md" variant="light" color={waits ? 'amber' : waits === null ? 'slate' : 'teal'}>{icon}</ThemeIcon>
      <Box>
        <Group gap={6}>
          <Text size={FS.body} fw={600}>{title}</Text>
          {waits !== null && (
            <Badge size="xs" variant="light" radius="sm" tt="none" color={waits ? 'amber' : 'teal'}
              leftSection={waits ? <IconClockHour4 size={10} /> : <IconCheck size={10} />}>
              {waits ? 'waits for approval' : 'right away'}
            </Badge>
          )}
        </Group>
        <Text size={FS.micro} c="dimmed">{detail}</Text>
      </Box>
    </Group>
  )
}


export function GiveAccess({ dbId, db, levels: LEVELS, opened, onClose, teams, people, onSafety }: {
  dbId: string; db: Database; levels: { value: DBLevel; label: string; blurb: string }[]; opened: boolean; onClose: () => void
  teams: { value: string; label: string }[]; people: { value: string; label: string }[]; onSafety: () => void
}) {
  // Who is any number of teams and people: "t:<team id>" or "u:<email>".
  const [picked, setPicked] = useState<string[]>([])
  const [level, setLevel] = useState<DBLevel>('read')
  const [touched, setTouched] = useState(false)
  const [approval, setApproval] = useState(false)
  // Others of the same kind on this server, given the same in one go.
  const [also, setAlso] = useState<string[]>([])
  const set = useSetDBAssignment()
  const giveMany = useGiveDBAccess()
  const { data: here } = useQuery({ ...databasesOnAssetQuery(db.assetId), enabled: opened })
  const siblings = (here?.databases ?? []).filter((d) => d.id !== db.id && isBroker(d.engine) === isBroker(db.engine))
  const kind: 'team' | 'person' = picked.length === 1 && picked[0]!.startsWith('t:') ? 'team' : 'person'
  const who = picked.length === 1 ? picked[0]!.slice(2) : null
  const { data: changes } = useQuery({ ...changeApprovalQuery(dbId), enabled: opened && isSQLEngine(db.engine) })
  // Production defaults to approval for write and above, unless the admin
  // decided otherwise for this row.
  const effectiveApproval = touched ? approval : db.label === 'production' && level !== 'read'
  const changesWait = writesNeedApproval(db)
  const env = LABEL_NAME[db.label] ?? db.label
  const pick = picked.length === 1 ? (kind === 'team' ? teams : people).find((x) => x.value === who) : undefined
  const name = !pick ? 'they' : kind === 'team' ? pick.label : shortName(pick.value)
  const whose = !pick ? 'their' : kind === 'team' ? `${pick.label}'s` : `${shortName(pick.value)}'s`
  const deciders = changes?.approvers === 'editors' || db.policy?.writeApprovers === 'editors'
    ? `an admin, an approver or another editor of ${db.dbname}` : 'an admin or an approver'
  const changeWhat = level === 'dba' ? 'Change data, tables, roles and grants' : level === 'owner' ? 'Change data and tables' : 'Change data'
  const submit = async () => {
    if (picked.length === 0) return
    try {
      if (picked.length === 1 && also.length === 0) {
        await set.mutateAsync({ id: dbId, level, needsApproval: effectiveApproval,
          ...(kind === 'team' ? { teamId: who! } : { userEmail: who! }) })
      } else {
        // Untouched, each one follows its own label, as a single grant would.
        await giveMany.mutateAsync({
          databaseIds: [dbId, ...also], level, brokerLevel: level,
          teams: picked.filter((k) => k.startsWith('t:')).map((k) => k.slice(2)),
          people: picked.filter((k) => k.startsWith('u:')).map((k) => k.slice(2)),
          approval: touched ? (approval ? 'always' : 'never') : 'label',
        })
      }
      const n = also.length + 1
      notifyOk('Access given', `${levelName(level, db.engine)} on ${n === 1 ? db.dbname : `${n} ${isBroker(db.engine) ? 'brokers' : 'databases'}`}${effectiveApproval ? ', with approval before each session' : ''}.`)
      onClose()
      setPicked([]); setAlso([]); setTouched(false); setLevel('read')
    } catch (err) {
      notifyError('Not given', err)
    }
  }
  return (
    <Modal opened={opened} onClose={onClose} centered size="lg"
      title={<Group gap="xs"><Text fw={650}>Give access to {db.dbname}</Text><EnvBadge label={db.label} /></Group>}>
      <Stack gap="sm">
        <MultiSelect label="Who" searchable value={picked} onChange={setPicked}
          placeholder={picked.length ? 'Add a team or person' : 'Pick teams and people'} nothingFoundMessage="Nobody by that name"
          data={[
            { group: 'Teams', items: teams.map((t) => ({ value: `t:${t.value}`, label: t.label })) },
            { group: 'People', items: people.map((p) => ({ value: `u:${p.value}`, label: p.label })) },
          ]} />
        <Box>
          <Text size={FS.micro} c="dimmed" mb={4}>What they can do</Text>
          <SegmentedControl value={level} onChange={(v) => setLevel(v as DBLevel)} fullWidth
            data={LEVELS.map((l) => ({ label: l.label, value: l.value }))} />
          <Text size={FS.micro} c="dimmed" mt={4}>{LEVELS.find((l) => l.value === level)?.blurb}</Text>
        </Box>
        <Paper withBorder radius="md" p="sm" bg="var(--mantine-color-default-hover)" aria-label="What this access does">
          <Text size={FS.micro} fw={650} tt="uppercase" c="dimmed" mb={6}>On {env.toLowerCase()}, {name} will</Text>
          <Stack gap={8}>
            <CanDo icon={<IconLogin2 size={14} />} title="Open a session" waits={effectiveApproval}
              detail={effectiveApproval ? 'An approver says yes first, and the session lasts as long as they allow.' : `No approval needed to open ${db.dbname}.`} />
            <CanDo icon={<IconEye size={14} />} title="Read data" waits={false}
              detail={isSQLEngine(db.engine) ? 'SELECT and browsing never wait.' : 'Reading never waits.'} />
            {level !== 'read' && (
              isSQLEngine(db.engine) ? (
                <CanDo icon={<IconPencil size={14} />} title={changeWhat} waits={changesWait}
                  detail={changesWait
                    ? `Each change runs once ${deciders} approves it.`
                    : db.engine === 'postgres' ? 'Runs right away. Still recorded, and deleted rows go to the trash.' : 'Runs right away, and is recorded.'} />
              ) : (
                <CanDo icon={<IconPencil size={14} />} title="Change data" waits={false} detail="Runs right away, and is recorded." />
              )
            )}
          </Stack>
          {isSQLEngine(db.engine) && (
            <Text size={FS.micro} c="dimmed" mt={8}>
              Changes {changesWait ? 'wait for approval' : 'run right away'} on {db.dbname}{approvalIsDefault(db) ? `, as for every ${db.label} database` : ', set for this database'}.{' '}
              <Anchor component="button" type="button" size={FS.micro} onClick={() => { onClose(); onSafety() }}>Change it in Safety</Anchor>
            </Text>
          )}
        </Paper>
        {siblings.length > 0 && (
          <Paper withBorder radius="md" p="sm">
            <Text size={FS.meta} fw={600}>Also on other {isBroker(db.engine) ? 'brokers' : 'databases'} on {db.hostname.split('.')[0]}</Text>
            <Text size={FS.micro} c="dimmed" mb={6}>The same level, in one go. Each gets its own grant you can change later.</Text>
            <Stack gap={4}>
              {siblings.map((d) => (
                <Checkbox key={d.id} size="xs" checked={also.includes(d.id)}
                  onChange={(e) => { const on = e.currentTarget.checked; setAlso((a) => on ? [...a, d.id] : a.filter((x) => x !== d.id)) }}
                  label={<Group gap={6} wrap="nowrap"><EngineMark engine={d.engine} size={16} /><Text size={FS.meta}>{targetName(d)}</Text>
                    <Text size={FS.micro} c="dimmed">{d.label}</Text></Group>} />
              ))}
            </Stack>
          </Paper>
        )}
        <Checkbox checked={effectiveApproval}
          label={`Also ask an approver before each of ${whose} sessions`}
          onChange={(e) => { setTouched(true); setApproval(e.currentTarget.checked) }}
          description={db.label === 'production' && level !== 'read'
            ? 'On by default for Write and above on production. For this grant only.'
            : 'Optional, stricter than the database, for this grant only.'} />
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button onClick={() => void submit()} loading={set.isPending || giveMany.isPending} disabled={picked.length === 0}>
            Give {levelName(level, db.engine)} access{also.length ? ` on ${also.length + 1}` : ''}
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}

/**
 * Whether a change on db waits for someone else, and who decides it: the
 * switch starts at the label's default, and the choice of approvers is
 * here too. The second part points at session approval, which is set on
 * each grant, so the two are never confused.
 */
export function ApprovalsCard({ db, policy, label, setPolicy, assignments, onAccess }: {
  db: Database; policy: DBPolicy; label: DBLabel; setPolicy: (p: DBPolicy) => void
  assignments: { needsApproval: boolean }[]; onAccess: () => void
}) {
  const { data: changes } = useQuery(changeApprovalQuery(db.id))
  const labelDefault = label === 'production'
  const on = policy.writeApproval ?? labelDefault
  const approvers = policy.writeApprovers ?? 'approvers'
  const editors = changes?.editors ?? []
  const sessions = assignments.filter((a) => a.needsApproval).length
  const toggle = (v: boolean) => {
    if (!v && label === 'production' && !window.confirm(
      `Changes on ${db.dbname} will run as soon as someone with Write sends them. They are still recorded, and deleted rows can be put back from the trash. Turn approval off?`)) return
    setPolicy({ ...policy, writeApproval: v === labelDefault ? undefined : v })
  }
  return (
    <SectionCard title="Approvals" description={`What waits for a second person on ${db.dbname}.`}>
      <Stack gap="sm">
        <Group justify="space-between" wrap="nowrap" align="flex-start">
          <Box>
            <Text size={FS.body} fw={600}>Changes need a second person's approval</Text>
            <Text size={FS.micro} c="dimmed">Every INSERT, UPDATE, DELETE or schema change waits, then runs once. Reading never waits.</Text>
            <Text size={FS.micro} c="dimmed" mt={2}>
              Default for {label}: {labelDefault ? 'on' : 'off'}
              {policy.writeApproval !== undefined && (
                <> · Changed from the default · <Anchor component="button" type="button" size={FS.micro}
                  onClick={() => setPolicy({ ...policy, writeApproval: undefined })}>Use the default</Anchor></>
              )}
            </Text>
          </Box>
          <Switch size="lg" checked={on} onChange={(e) => toggle(e.currentTarget.checked)} aria-label="Changes need a second person's approval" />
        </Group>
        {label !== db.label && policy.writeApproval === undefined && (
          <Alert color="amber" variant="light" p="xs">
            <Text size={FS.micro}>Moving {db.dbname} to {label} turns this {labelDefault ? 'on' : 'off'}, its default there, when you save.</Text>
          </Alert>
        )}
        {on && (
          <Box pl="md" style={{ borderLeft: '2px solid var(--mantine-color-default-border)' }}>
            <Radio.Group label="Who can approve" value={approvers}
              onChange={(v) => setPolicy({ ...policy, writeApprovers: v === 'editors' ? 'editors' : undefined })}>
              <Stack gap="xs" mt={6}>
                <Radio value="approvers" label="Admins and approvers" description="Only people whose console role can decide requests." />
                <Radio value="editors" label="Admins, approvers, or another editor of this database"
                  description={`Anyone with Write, Owner or DBA on ${db.dbname}, given directly or through a team. Never the person asking.`} />
              </Stack>
            </Radio.Group>
            {approvers === 'editors' && (
              <Stack gap={4} mt="xs">
                <Group gap={6}>
                  <IconUsersGroup size={14} />
                  <Text size={FS.micro} c="dimmed">{editors.length} {editors.length === 1 ? 'editor' : 'editors'} now{editors.length ? ':' : ''}</Text>
                  <People emails={editors} />
                </Group>
                {editors.length <= 1 && (
                  <Text size={FS.micro} c="amber.8">
                    {editors.length === 0 ? `Nobody can edit ${db.dbname} yet` : `Only ${shortName(editors[0]!)} can edit ${db.dbname}`}, so admins and approvers will decide.
                  </Text>
                )}
                <Text size={FS.micro} c="dimmed">An editor whose own access needs approval counts only while that approval is live.</Text>
              </Stack>
            )}
          </Box>
        )}
        <Divider />
        <Group justify="space-between" wrap="nowrap">
          <Box>
            <Text size={FS.body} fw={600}>Opening a session</Text>
            <Text size={FS.micro} c="dimmed">Set on each permission under Who can connect. {sessions} of {assignments.length} need approval.</Text>
          </Box>
          <Button variant="subtle" size="xs" rightSection={<IconArrowRight size={14} />} onClick={onAccess}>Go to access</Button>
        </Group>
      </Stack>
    </SectionCard>
  )
}

