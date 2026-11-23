import { Alert, Box, Button, Group, Loader, Modal, Select, Stack, Text } from '@mantine/core'
import { useDebouncedValue } from '@mantine/hooks'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useMemo, useState } from 'react'
import { SectionCard } from '~/components/page'
import { Statements, Steps } from '~/components/CreateOnServer'
import {
  ConnectionDetails, ManualLines, newOwnerState, OwnerFields, ownerName, ownerPassword, ownerReady, ownerRequest,
  ownerWords, PasswordField, passwordProblem, userKey, type OwnerState,
} from '~/components/OwnerFields'
import { discoverDatabases, isConfigured } from '~/lib/live'
import { createOptions, setDatabaseOwner, setOwnerPassword, type CreateAnswer, type OwnerUser } from '~/lib/createApi'
import { notifyError, notifyOk } from '~/lib/notify'
import { FS } from '~/theme'
import { pgLevelRole } from '~/lib/pgRoles'
import type { Database } from '~/types/domain'

/**
 * Who owns a database on the server, and changing it: to Argus's Owner level
 * (Postgres), a user already there, or a new one made in the same step with
 * its password shown once. Run by the agent and recorded in Activity, the
 * password left out. On Postgres the tables inside keep their owners; only
 * the database's own owner moves. Also "Give it a new password" for the
 * owner user, when the app's copy is lost.
 */
export function DatabaseOwner({ db }: { db: Database }) {
  const qc = useQueryClient()
  const pg = db.engine === 'postgres'
  const host = db.hostname?.split('.')[0] ?? 'the server'
  const where = { engine: db.engine, port: db.port, host: db.host, poolerPort: db.poolerPort || 0 }
  const listed = useQuery({
    queryKey: ['discover-databases', db.assetId, where] as const,
    queryFn: () => discoverDatabases(db.assetId, where),
    enabled: isConfigured() && pg, retry: false, staleTime: 30_000,
  })
  const opts = useQuery({
    queryKey: ['create-options', db.assetId, where] as const,
    queryFn: () => createOptions(db.assetId, where),
    enabled: isConfigured(), retry: false, staleTime: 30_000,
  })
  const now = listed.data?.databases.find((d) => d.name === db.dbname)?.owner
  // Argus's Owner level role for the database: what '' means as an owner.
  const { data: mine = '' } = useQuery({
    queryKey: ['pg-level-role', 'owner', db.dbname], queryFn: () => pgLevelRole('owner', db.dbname), staleTime: Infinity, enabled: pg,
  })
  const users: OwnerUser[] = useMemo(() => {
    const o = opts.data?.options
    const list = o?.users ?? (o?.roles ?? []).map((r) => ({ name: r }))
    return pg && now && now !== mine && !list.some((u) => u.name === now) ? [...list, { name: now }] : list
  }, [opts.data, pg, now, mine])

  const [s, setS] = useState<OwnerState>(() => newOwnerState(pg ? '' : 'existing'))
  const [touched, setTouched] = useState(false)
  const change = (v: OwnerState) => { setS(v); setTouched(true); setDone(null) }
  // On Postgres the form starts at who owns it now.
  const form: OwnerState = !touched && pg && now !== undefined && now !== mine
    ? { ...s, kind: 'existing', picked: userKey(users.find((u) => u.name === now) ?? { name: now }) }
    : !touched && pg ? { ...s, kind: '' } : s
  const target = pg && form.kind === '' ? mine : ownerName(form, db.dbname, users)
  const dirty = touched && (form.kind === 'new' || target !== '' ) && !(pg && target === now)
  const ready = dirty && ownerReady(form, db.engine, db.dbname, users)
  const ownerReq = ownerRequest(form, db.engine, db.dbname, users) ?? { kind: '' as const }
  const [reqD] = useDebouncedValue(ownerReq, 300)
  const preview = useQuery({
    queryKey: ['owner-preview', db.id, reqD] as const,
    queryFn: () => setDatabaseOwner(db.id, { ...reqD, dryRun: true }),
    enabled: isConfigured() && ready, retry: false, staleTime: 0,
  })

  const [busy, setBusy] = useState(false)
  const [done, setDone] = useState<{ answer: CreateAnswer; password: string } | null>(null)
  const [renew, setRenew] = useState(false)
  const down = pg ? listed.data && listed.data.state !== 'ok' : opts.data && opts.data.state !== 'ok'
  const words = ownerWords(db.engine, host)

  const save = async () => {
    setBusy(true)
    try {
      const password = ownerPassword(form)
      const a = await setDatabaseOwner(db.id, { ...ownerReq, ...(password ? { password } : {}) })
      if (a.state !== 'ok') throw new Error(a.error || `The owner was not changed (${a.state}).`)
      setDone({ answer: a, password: a.connection ? password : '' })
      setTouched(false)
      setS(newOwnerState(pg ? '' : 'existing'))
      notifyOk('Owner changed', `${db.dbname} is owned by ${target} now. Recorded in Activity.`)
      void qc.invalidateQueries({ queryKey: ['discover-databases', db.assetId] })
      void qc.invalidateQueries({ queryKey: ['create-options', db.assetId] })
    } catch (err) {
      notifyError('Owner not changed', err)
    } finally {
      setBusy(false)
    }
  }

  return (
    <SectionCard title="Owner" description={pg
      ? 'The owner can create and change anything in the database. Tables keep their own owners when this changes.'
      : words.note}>
      <Stack gap="sm">
        {down ? (
          <Alert variant="light" color="amber" p="xs"><Text size={FS.meta}>{(pg ? listed.data?.error : opts.data?.error) || 'The agent could not say who owns it.'}</Text></Alert>
        ) : (pg ? listed.isPending : opts.isPending) ? (
          <Group gap={6}><Loader size={12} /><Text size={FS.micro} c="dimmed">Asking the agent…</Text></Group>
        ) : (
          <>
            {pg && (
              <Text size={FS.meta}>Owned by <Text span ff="monospace" fw={600}>{now ?? 'unknown'}</Text>
                {mine && now === mine ? ', Argus’s Owner level: whoever is given Owner here owns it.' : '.'}</Text>
            )}
            <Box maw={640}>
              <OwnerFields engine={db.engine} host={host} db={db.dbname} users={users} value={form} onChange={change}
                kinds={pg ? ['', 'existing', 'new'] : ['existing', 'new']} currentOwner={pg ? mine : undefined} />
            </Box>
            {dirty && (
              <Statements title="Runs on the server" lines={preview.data?.statements ?? []} loading={preview.isFetching}
                empty={preview.error ? (preview.error as Error).message : ready ? '' : 'Finish the fields to see what runs.'} />
            )}
            <Group gap="xs">
              <Button onClick={() => void save()} loading={busy} disabled={!ready || !!preview.error}>
                {form.kind === 'new' && target ? `Make ${target} and give it ${db.dbname}` : 'Change owner'}
              </Button>
              <Button variant="default" onClick={() => setRenew(true)}>Give it a new password…</Button>
            </Group>
            {done && (
              <Stack gap="xs">
                <Steps steps={done.answer.steps} />
                <ManualLines lines={done.answer.manual ?? []} />
                {done.answer.connection && (
                  <Box maw={640}><ConnectionDetails engine={db.engine} connection={done.answer.connection} password={done.password} onCopied={() => {}} /></Box>
                )}
                <Statements title="Ran" lines={done.answer.statements} />
              </Stack>
            )}
            {pg && <Text size={FS.micro} c="dimmed">Postgres’s own pg_ roles and the roles Argus makes for people are not offered.</Text>}
          </>
        )}
      </Stack>
      {renew && (
        <NewPassword db={db} users={users} owner={pg && now !== mine ? now : undefined} onClose={() => setRenew(false)} />
      )}
    </SectionCard>
  )
}

/** Give the owner user a new password, shown once, checked by signing in. */
function NewPassword({ db, users, owner, onClose }: { db: Database; users: OwnerUser[]; owner?: string; onClose: () => void }) {
  const [picked, setPicked] = useState<string | null>(() => {
    const u = users.find((x) => x.name === owner)
    return u ? userKey(u) : null
  })
  const [s, setS] = useState<OwnerState>(() => newOwnerState('new'))
  const [busy, setBusy] = useState(false)
  const [done, setDone] = useState<{ answer: CreateAnswer; password: string } | null>(null)
  const [copied, setCopied] = useState(false)
  const [leaving, setLeaving] = useState(false)
  const user = users.find((u) => userKey(u) === picked)
  const err = passwordProblem(s, db.engine)

  const save = async () => {
    if (!user) return
    setBusy(true)
    try {
      const password = ownerPassword(s)
      const a = await setOwnerPassword(db.id, { name: user.name, ...(user.from ? { from: user.from } : {}), password })
      if (a.state !== 'ok') throw new Error(a.error || `The password was not changed (${a.state}).`)
      setDone({ answer: a, password })
      notifyOk('New password set', `${user.name} has a new password. Recorded in Activity, the password left out.`)
    } catch (e) {
      notifyError('Password not changed', e)
    } finally {
      setBusy(false)
    }
  }
  const close = () => { if (done && !copied && !leaving) setLeaving(true); else onClose() }

  return (
    <Modal opened onClose={close} title={`New password for ${db.dbname}’s owner`} size="lg">
      <Stack gap="sm">
        {!done ? (
          <>
            <Select label="User" searchable allowDeselect={false} value={picked} onChange={setPicked}
              data={users.filter((u) => !u.all).map((u) => ({ value: userKey(u), label: u.from ? `${u.name} (${u.from})` : u.name }))} />
            <PasswordField value={s} onChange={setS} error={err} />
            {db.label === 'production' && (
              <Alert variant="light" color="amber" p="xs">
                <Text size={FS.meta}>The old password stops working at once. Put the new one in the app before it next reconnects.</Text>
              </Alert>
            )}
            <Group justify="flex-end" gap="xs">
              <Button variant="default" onClick={onClose}>Cancel</Button>
              <Button onClick={() => void save()} loading={busy} disabled={!user || !!err}>Set the new password</Button>
            </Group>
          </>
        ) : (
          <>
            <Steps steps={done.answer.steps} />
            <ManualLines lines={done.answer.manual ?? []} />
            {done.answer.connection && (
              <ConnectionDetails engine={db.engine} connection={done.answer.connection} password={done.password} onCopied={() => setCopied(true)} />
            )}
            {leaving && (
              <Alert variant="light" color="amber" p="xs"><Text size={FS.meta}>The password will not be shown again. Copy it first, or close anyway.</Text></Alert>
            )}
            <Group justify="flex-end">
              <Button onClick={close} color={leaving ? 'amber' : undefined}>{leaving ? 'Close anyway' : 'I’ve saved it, close'}</Button>
            </Group>
          </>
        )}
      </Stack>
    </Modal>
  )
}
