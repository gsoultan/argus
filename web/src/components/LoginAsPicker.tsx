import { useEffect, useMemo } from 'react'
import { Badge, Box, Group, Loader, Radio, Stack, Text, TextInput } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { FS, SP } from '~/theme'
import { requestAccountsQuery } from '~/lib/queries'
import type { RequestableHost } from '~/types/domain'

/**
 * The "Log in as" choice on Ask for access.
 *
 * It used to offer deploy, ops and root on every server, written into the
 * form. Now it offers the picked servers' own Login accounts, each checked
 * against what the server's agent reported, and says why any other account
 * on the list cannot be picked rather than leaving it out without a word.
 */

export interface AccountChoice {
  name: string
  elevated: boolean
  /** Empty when it can be picked; otherwise why not, naming the server. */
  unavailable: string
}

const short = (h: string) => h.split('.')[0] ?? h

/**
 * Every account any picked server lists, in the order the servers list them.
 * One request is one account on every server it names, so an account can be
 * picked only when each server in the inventory has it.
 */
export function accountChoices(hosts: RequestableHost[]): AccountChoice[] {
  const known = hosts.filter((h) => h.known)
  const names: string[] = []
  for (const h of known) for (const a of h.accounts) if (!names.includes(a.name)) names.push(a.name)
  return names.map((name) => {
    let elevated = false
    const missing: string[] = []
    const notFound: string[] = []
    for (const h of known) {
      const a = h.accounts.find((x) => x.name === name)
      if (!a) missing.push(short(h.hostname))
      else {
        elevated ||= a.elevated
        if (!a.onServer) notFound.push(short(h.hostname))
      }
    }
    const why: string[] = []
    if (notFound.length) why.push(`Not found on ${notFound.join(', ')} when its agent last checked`)
    if (missing.length) why.push(`Not a login account on ${missing.join(', ')}`)
    return { name, elevated, unavailable: why.join('. ') }
  })
}

/** The account to start on: the current one if it can still be picked, else an everyday one. */
export function defaultChoice(choices: AccountChoice[], current: string): string {
  const ok = choices.filter((c) => !c.unavailable)
  if (ok.some((c) => c.name === current)) return current
  return (ok.find((c) => !c.elevated) ?? ok[0])?.name ?? ''
}

export function LoginAsPicker({ hostnames, value, onChange }: {
  hostnames: string[]
  value: string
  onChange: (v: string) => void
}) {
  const { data: hosts, isLoading, isError } = useQuery(requestAccountsQuery(hostnames))
  const choices = useMemo(() => accountChoices(hosts ?? []), [hosts])
  const anyKnown = !!hosts?.some((h) => h.known)

  // An account carried over from a server no longer picked may not exist on
  // the ones that are. Left alone, nothing reads as selected and the request
  // would name an account the servers do not have. A server outside the
  // inventory keeps whatever was typed: there is no list to hold it to.
  useEffect(() => {
    if (!hosts || !anyKnown) return
    const next = defaultChoice(choices, value)
    if (next !== value) onChange(next)
  }, [hosts, anyKnown, choices, value, onChange])

  const label = <Text size="sm" fw={500} mb={SP.snug}>Log in as</Text>

  if (hostnames.length === 0) {
    return (
      <Box>
        {label}
        <Text size={FS.meta} c="dimmed">Pick a server to see the accounts on it.</Text>
      </Box>
    )
  }
  if (isLoading) {
    return (
      <Box>
        {label}
        <Group gap="xs"><Loader size="xs" /><Text size={FS.meta} c="dimmed">Checking the accounts on the server…</Text></Group>
      </Box>
    )
  }
  if (isError || !anyKnown) {
    // Nothing to offer from: the servers are not in the inventory (the
    // approver decides whether they exist), or the list could not be read.
    // Typing the account is the honest fallback; the control plane checks it.
    return (
      <TextInput
        size="sm"
        label="Log in as"
        description={isError
          ? 'Could not read the accounts on this server. Type the account; it is checked when you submit.'
          : 'This server is not in your list, so its accounts are not known here. Type the account you need.'}
        placeholder="e.g. ubuntu"
        value={value}
        onChange={(e) => onChange(e.currentTarget.value.trim())}
        autoCapitalize="off"
        autoCorrect="off"
        spellCheck={false}
      />
    )
  }

  const available = choices.filter((c) => !c.unavailable)
  return (
    <Box>
      {label}
      {choices.length === 0 ? (
        <Text size={FS.meta} c="dimmed">
          This server has no login accounts set up yet. An admin adds them with Edit on the
          server&rsquo;s page.
        </Text>
      ) : (
        <Radio.Group value={value} onChange={onChange}>
          <Stack gap="xs">
            {choices.map((c) => (
              <Radio.Card
                key={c.name}
                value={c.name}
                disabled={!!c.unavailable}
                className="argus-pick"
                data-checked={c.name === value || undefined}
                style={c.unavailable ? { opacity: 0.55, cursor: 'not-allowed' } : undefined}
                aria-describedby={`login-as-${c.name}`}
              >
                <Group gap="sm" wrap="nowrap">
                  <Radio.Indicator disabled={!!c.unavailable} />
                  <Box style={{ flex: 1, minWidth: 0 }}>
                    <Text size="sm" fw={600} ff="monospace" style={{ overflowWrap: 'anywhere' }}>{c.name}</Text>
                    <Text id={`login-as-${c.name}`} size={FS.meta} c="dimmed" lh={1.4}>
                      {c.unavailable || (c.elevated ? 'Full control of the server' : 'Everyday account')}
                    </Text>
                  </Box>
                  {c.elevated && !c.unavailable && <Badge size="xs" color="rose">elevated</Badge>}
                </Group>
              </Radio.Card>
            ))}
          </Stack>
        </Radio.Group>
      )}
      <Text size={FS.micro} c="dimmed" mt={SP.snug} lh={1.5}>
        {available.length === 0 && choices.length > 0
          ? 'None of these accounts can be used on every server you picked. Ask for fewer servers, or ask an admin to fix the server’s login accounts.'
          : 'Only the server’s own login accounts are offered. An admin adds or removes them with Edit on the server’s page.'}
      </Text>
    </Box>
  )
}
