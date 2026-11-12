import { useState } from 'react'
import { Badge, Box, Button, Code, Group, Stack, Switch, Text, Textarea } from '@mantine/core'
import { IconShieldLock } from '@tabler/icons-react'
import { FS, SP } from '~/theme'
import { SectionCard } from '~/components/page'
import { useUpdateDatabase } from '~/lib/queries'
import { notifyError, notifyOk } from '~/lib/notify'
import { levelName } from '~/lib/engines'
import { DEFAULT_LEVEL_RULES, keyRuleWords, ruleKind, rulesProblem } from '~/lib/redisManage'
import type { DBLevel, DBPolicy, Database } from '~/types/domain'

/*
What each Argus level's Redis user may do on this database: Argus's own rules
by default, or rules set here (key patterns, channels, commands). Below DBA
the control plane and the agent both refuse rules that reach the server's
configuration or users, whatever is typed. A change applies to the next
session; open ones keep the user they were given.
*/

const LEVELS: DBLevel[] = ['read', 'write', 'owner', 'dba']

export function RedisPermissions({ db }: { db: Database }) {
  const update = useUpdateDatabase()
  const saved = db.policy?.redisLevels ?? {}
  const [own, setOwn] = useState<Partial<Record<DBLevel, string>>>(() =>
    Object.fromEntries(Object.entries(saved).map(([l, r]) => [l, (r ?? []).join(' ')])))

  const rulesOf = (l: DBLevel) => (own[l] ?? '').split(/\s+/).filter(Boolean)
  const problems = Object.fromEntries(LEVELS.map((l) => [l, own[l] !== undefined ? rulesProblem(rulesOf(l), l) : null])) as Record<DBLevel, string | null>
  const next: Partial<Record<DBLevel, string[]>> = {}
  for (const l of LEVELS) if (own[l] !== undefined) next[l] = rulesOf(l)
  const dirty = JSON.stringify(next) !== JSON.stringify(saved)
  const bad = LEVELS.some((l) => problems[l])

  const save = async () => {
    const policy: DBPolicy = { ...(db.policy ?? {}), redisLevels: Object.keys(next).length ? next : undefined }
    try {
      await update.mutateAsync({ id: db.id, label: db.label, policy })
      notifyOk('Saved', 'The next session at each level gets these rules; open ones keep theirs.')
    } catch (err) {
      notifyError('Not saved', err)
    }
  }

  return (
    <Stack gap="md">
      <Text size={FS.body} c="dimmed">
        Each person gets their own Redis user for their level while a session is open. These are the ACL rules that user gets.
      </Text>
      {LEVELS.map((l) => {
        const custom = own[l] !== undefined
        const rules = custom ? rulesOf(l) : DEFAULT_LEVEL_RULES[l]
        return (
          <SectionCard key={l} title={levelName(l, 'redis')} icon={IconShieldLock}
            badge={<Badge size="xs" variant="light" color={custom ? 'azure' : 'slate'}>{custom ? 'set here' : 'Argus’s default'}</Badge>}
            action={
              <Switch size="xs" label="Use Argus’s default" checked={!custom}
                onChange={(e) => {
                  const useDefault = e.currentTarget.checked
                  setOwn((o) => {
                    const n = { ...o }
                    if (useDefault) delete n[l]
                    else n[l] = DEFAULT_LEVEL_RULES[l].join(' ')
                    return n
                  })
                }} />
            }>
            <Stack gap={SP.cozy} px="md" pb="md">
              {custom ? (
                <Textarea value={own[l]} autosize minRows={2} aria-label={`${l} rules`}
                  onChange={(e) => { const v = e.currentTarget.value; setOwn((o) => ({ ...o, [l]: v })) }}
                  error={problems[l] ?? undefined}
                  styles={{ input: { fontFamily: 'var(--font-mono)', fontSize: 12.5 } }} />
              ) : <Code block>{rules.join(' ')}</Code>}
              <Box>
                <Text size={FS.micro} c="dimmed">Keys: {rules.filter((r) => ruleKind(r) === 'key').map(keyRuleWords).join(' · ') || 'none'}</Text>
                <Text size={FS.micro} c="dimmed">Commands: {rules.filter((r) => ruleKind(r) === 'command').join(' ') || 'none'}</Text>
              </Box>
            </Stack>
          </SectionCard>
        )
      })}
      <Text size={FS.micro} c="dimmed">
        Try a level’s rules against a command in the Redis page, Users & ACL → Check a level. Below DBA, rules may not grant +@admin, CONFIG, ACL, FLUSHALL or other admin commands, and +@all needs -@admin with it.
      </Text>
      <Group justify="flex-end">
        <Button onClick={() => void save()} disabled={!dirty || bad} loading={update.isPending}>Save</Button>
      </Group>
    </Stack>
  )
}
