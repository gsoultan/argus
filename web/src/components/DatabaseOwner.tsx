import { Alert, Button, Group, Loader, Select, Stack, Text } from '@mantine/core'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { SectionCard } from '~/components/page'
import { Statements } from '~/components/CreateOnServer'
import { createOptions, discoverDatabases, isConfigured, setDatabaseOwner } from '~/lib/live'
import { notifyError, notifyOk } from '~/lib/notify'
import { FS } from '~/theme'
import { pgLevelRole } from '~/lib/pgRoles'
import type { Database } from '~/types/domain'


/**
 * Who owns a Postgres database on the server, and changing it: ALTER DATABASE
 * OWNER TO, run by the agent as postgres and recorded in Activity. The tables
 * inside keep their owners; only the database's own owner moves.
 */
export function DatabaseOwner({ db }: { db: Database }) {
  const qc = useQueryClient()
  const where = { engine: db.engine, port: db.port, host: db.host }
  const listed = useQuery({
    queryKey: ['discover-databases', db.assetId, where] as const,
    queryFn: () => discoverDatabases(db.assetId, where),
    enabled: isConfigured(), retry: false, staleTime: 30_000,
  })
  const opts = useQuery({
    queryKey: ['create-options', db.assetId, where] as const,
    queryFn: () => createOptions(db.assetId, where),
    enabled: isConfigured(), retry: false, staleTime: 30_000,
  })
  const now = listed.data?.databases.find((d) => d.name === db.dbname)?.owner
  const [want, setWant] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [ran, setRan] = useState<string[]>([])
  // Argus's Owner level role for the database: what '' means as an owner.
  const { data: mine = '' } = useQuery({ queryKey: ['pg-level-role', 'owner', db.dbname], queryFn: () => pgLevelRole('owner', db.dbname), staleTime: Infinity })
  const roles = [...new Set([...(opts.data?.options?.roles ?? []), ...(now && now !== mine ? [now] : [])])].sort()
  const pick = want ?? (now === undefined ? null : now === mine ? '' : now)
  const target = pick === '' ? mine : pick
  const dirty = pick !== null && now !== undefined && !(now === mine ? pick === '' : pick === now)
  const down = listed.data && listed.data.state !== 'ok'

  const save = async () => {
    if (pick === null) return
    setBusy(true)
    try {
      const a = await setDatabaseOwner(db.id, pick)
      if (a.state !== 'ok') throw new Error(a.error || `The owner was not changed (${a.state}).`)
      setRan(a.statements)
      setWant(null)
      notifyOk('Owner changed', `${db.dbname} is owned by ${target} now. Recorded in Activity.`)
      void qc.invalidateQueries({ queryKey: ['discover-databases', db.assetId] })
    } catch (err) {
      notifyError('Owner not changed', err)
    } finally {
      setBusy(false)
    }
  }

  return (
    <SectionCard title="Owner" description="The owner can create and change anything in the database. Tables keep their own owners when this changes.">
      <Stack gap="sm">
        {down ? (
          <Alert variant="light" color="amber" p="xs"><Text size={FS.meta}>{listed.data?.error || 'The agent could not say who owns it.'}</Text></Alert>
        ) : listed.isPending ? (
          <Group gap={6}><Loader size={12} /><Text size={FS.micro} c="dimmed">Asking the agent…</Text></Group>
        ) : (
          <>
            <Text size={FS.meta}>Owned by <Text span ff="monospace" fw={600}>{now ?? 'unknown'}</Text>
              {mine && now === mine ? ', Argus’s Owner level: whoever is given Owner here owns it.' : '.'}</Text>
            <Group align="flex-end" gap="xs" wrap="wrap">
              <Select label="Change to" searchable allowDeselect={false} w={{ base: '100%', sm: 360 }} value={pick}
                onChange={setWant} data={[{ value: '', label: `Argus Owner level (${mine})` }, ...roles.filter((r) => r !== mine).map((r) => ({ value: r, label: r }))]} />
              <Button onClick={() => void save()} loading={busy} disabled={!dirty}>Change owner</Button>
            </Group>
            {dirty && <Statements title="Runs on the server" lines={[`ALTER DATABASE ${db.dbname} OWNER TO ${target}`]} />}
            {ran.length > 0 && !dirty && <Statements title="Ran" lines={ran} />}
            <Text size={FS.micro} c="dimmed">Postgres’s own pg_ roles and the roles Argus makes for people are not offered.</Text>
          </>
        )}
      </Stack>
    </SectionCard>
  )
}
