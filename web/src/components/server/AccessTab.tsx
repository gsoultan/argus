import { useState } from 'react'
import { Alert, Autocomplete, Badge, Box, Button, Card, Group, Select, Stack, Text, Tooltip } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconAlertTriangle } from '@tabler/icons-react'
import { Mono, relTime } from '~/components/primitives'
import { LoginAccounts, SettingsSection, SettingsSections } from '~/components/server/layout'
import { WhoCanConnect } from '~/components/WhoCanConnect'
import { DIRECT_SSH_CHOICES, directSshLabel, directSshLoosens } from '~/lib/directSsh'
import { run } from '~/lib/notify'
import {
  loginAccountsQuery, policyQuery, useSetAssetDirectSsh, useSetAssetDirectSshAccount,
} from '~/lib/queries'
import { FS, SP } from '~/theme'
import type { Asset, DirectSsh, ServerLoginAccount, ServerLoginAccounts } from '~/types/domain'

/**
 * Who can reach this server and as whom: the teams and people it was given
 * to, the accounts sessions log in as, and what happens to a login that does
 * not come through Argus at all.
 */
export function AccessTab({ asset, canEdit }: { asset: Asset; canEdit: boolean }) {
  return (
    <SettingsSections>
      <SettingsSection
        title="Who can connect"
        description="Teams and people who can open sessions here, and as which login account."
      >
        <Card padding="md">
          <WhoCanConnect asset={asset} canEdit={canEdit} />
        </Card>
      </SettingsSection>
      <SettingsSection
        title="Login accounts"
        description="Accounts on the server that sessions through Argus log in as."
      >
        <LoginAccounts
          asset={asset}
          empty={
            <Text size={FS.meta} c="dimmed">
              None yet.{canEdit && ' Press Edit and add the account you log in with.'}
            </Text>
          }
        />
      </SettingsSection>
      <SettingsSection
        title="Logins that don't go through Argus"
        description="Someone with a key or password can still reach the server with ssh, scp or sftp. Pick what happens then."
      >
        <DirectSshCard asset={asset} canEdit={canEdit} />
      </SettingsSection>
    </SettingsSections>
  )
}

const INHERIT = 'inherit'

/**
 * Direct SSH logins on this server: its own choice, or the one in Settings,
 * and whether its agent has said it is applying it.
 */
export function DirectSshCard({ asset, canEdit }: { asset: Asset; canEdit: boolean }) {
  const { data: policy } = useQuery(policyQuery())
  const setMode = useSetAssetDirectSsh()
  const own = asset.directSsh ?? null
  const global = policy?.directSsh ?? null
  const effective: DirectSsh | null = own ?? global
  const applied = asset.agentDirectSsh ?? null

  const choose = (v: string | null) => {
    if (!v) return
    const next = v === INHERIT ? null : (v as DirectSsh)
    if (next === own) return
    const nextEffective = next ?? global
    const loosens = effective && nextEffective ? directSshLoosens(effective, nextEffective) : false
    void run(
      () => setMode.mutateAsync({ id: asset.id, mode: next }),
      {
        success: ['Direct SSH logins changed',
          `${asset.hostname}: ${next ? directSshLabel(next) : `the setting in Settings (${global ? directSshLabel(global) : 'unknown'})`}. ` +
          (loosens ? 'This lets more logins in, and is in the audit log as a loosening. ' : '') +
          'The agent applies it at its next check-in, within a minute.'],
        failure: 'Direct SSH logins not changed',
      },
    )
  }

  return (
    <Card padding="md">
      <Stack gap="sm">
        <Select
          label="When someone logs in directly"
          size="xs"
          allowDeselect={false}
          disabled={!canEdit || setMode.isPending}
          value={own ?? INHERIT}
          onChange={choose}
          data={[
            { value: INHERIT, label: `Use Settings (${global ? directSshLabel(global) : '…'})` },
            ...DIRECT_SSH_CHOICES.map((c) => ({ value: c.value, label: c.label })),
          ]}
          description={effective
            ? DIRECT_SSH_CHOICES.find((c) => c.value === effective)?.description
            : undefined}
        />
        <Text size={FS.meta} c="dimmed" lh={1.5}>
          {applied === null
            ? "This server's agent has not confirmed a setting. An agent older than this setting refuses direct logins under Refuse and Ask for approval, and accepts and records them under Allow; upgrade it to hold logins for approval."
            : applied === effective
              ? `This server's agent confirmed at its last check-in that it is applying ${directSshLabel(applied)}.`
              : `This server's agent is still applying ${directSshLabel(applied)}; it picks up ${effective ? directSshLabel(effective) : 'the change'} at its next check-in.`}
        </Text>
        <AccountDirectSsh asset={asset} canEdit={canEdit} server={effective} />
      </Stack>
    </Card>
  )
}

/** Login account names as the control plane accepts them. */
const ACCOUNT_NAME = /^[A-Za-z_][A-Za-z0-9._-]{0,63}$/

const PASSWORD_LABEL: Record<ServerLoginAccount['password'], string> = {
  set: 'Password',
  none: 'Empty password',
  locked: 'No password',
  unknown: 'Password unknown',
}

/** What an account could sign in with, as badges. */
function SignInBadges({ account, auth }: { account: ServerLoginAccount; auth: ServerLoginAccounts }) {
  const password = account.password === 'set' || account.password === 'none' || account.password === 'unknown'
  return (
    <Group gap={SP.tight} wrap="wrap">
      {password && (
        <Tooltip label={auth.passwords ? 'sshd accepts passwords' : 'sshd does not accept passwords, so this password cannot be used over SSH'}>
          <Badge size="xs" variant={auth.passwords ? 'light' : 'outline'} color={account.password === 'none' ? 'rose' : 'amber'}>
            {PASSWORD_LABEL[account.password]}
          </Badge>
        </Tooltip>
      )}
      {account.keys > 0 && (
        <Badge size="xs" variant={auth.keys ? 'light' : 'outline'} color="azure">
          {account.keys} {account.keys === 1 ? 'key' : 'keys'}
        </Badge>
      )}
      {!account.canLogin && <Badge size="xs" variant="outline" color="slate">Cannot log in</Badge>}
    </Group>
  )
}

/**
 * Every account that can log in to this server directly -- with a password,
 * a key, or both, as its agent last reported -- and what each one does with a
 * direct login: the server's setting, or its own. An account the server
 * names that the agent did not report is listed too.
 */
function AccountDirectSsh({ asset, canEdit, server }: {
  asset: Asset; canEdit: boolean; server: DirectSsh | null
}) {
  const setAccount = useSetAssetDirectSshAccount()
  const { data: report } = useQuery(loginAccountsQuery(asset.id))
  const [name, setName] = useState('')
  const [mode, setMode] = useState<DirectSsh>('approve')
  const own = asset.directSshAccounts ?? {}
  const serverLabel = server ? directSshLabel(server) : 'the server\'s setting'
  const trimmed = name.trim()
  const nameError = trimmed && !ACCOUNT_NAME.test(trimmed)
    ? 'Letters, digits, ".", "_" or "-", starting with a letter or "_"'
    : null

  const reported = new Map((report?.accounts ?? []).map((a) => [a.user, a]))
  const rows = [
    ...(report?.accounts ?? []).filter((a) => a.canLogin || a.user in own).map((a) => a.user),
    ...Object.keys(own).filter((u) => !reported.has(u)).sort((a, b) => a.localeCompare(b)),
  ]
  const hidden = (report?.accounts ?? []).filter((a) => !a.canLogin && !(a.user in own)).length

  const change = (account: string, next: DirectSsh | null) => {
    const before = own[account] ?? server
    const after = next ?? server
    const loosens = before && after ? directSshLoosens(before, after) : false
    return run(
      () => setAccount.mutateAsync({ id: asset.id, account, mode: next }),
      {
        success: ['Direct SSH logins changed',
          `${account} on ${asset.hostname}: ${next ? directSshLabel(next) : `the server's setting (${serverLabel})`}. ` +
          (loosens ? 'This lets more logins in, and is in the audit log as a loosening. ' : '') +
          'The agent applies it at its next check-in, within a minute.'],
        failure: 'Direct SSH logins not changed',
      },
    )
  }

  const add = async () => {
    if (!trimmed || nameError) return
    if (await change(trimmed, mode)) setName('')
  }

  return (
    <Stack gap={SP.cozy} mt={SP.cozy}>
      <div>
        <Text size="sm" fw={500}>Accounts that can log in</Text>
        <Text size={FS.meta} c="dimmed" lh={1.5}>
          {report?.reported
            ? `Every account on this server with a password or a key sshd accepts, from its agent's last check-in${report.checkedAt ? ` (${relTime(report.checkedAt)})` : ''}. Each one uses ${serverLabel} unless you pick something else for it.`
            : `This server's agent has not reported its accounts yet; an older agent does not. Accounts you name here still get their own setting, and every other account uses ${serverLabel}.`}
        </Text>
      </div>
      {report?.reported && !report.exposeAuthInfo && (
        <Alert color="amber" variant="light" p="xs" icon={<IconAlertTriangle size={16} />}>
          <Text size={FS.meta} lh={1.5}>
            This server&rsquo;s sshd does not say how each login signed in, so Argus records the
            login but not whether it used a password or which key. Add <Mono>ExposeAuthInfo yes</Mono> to
            /etc/ssh/sshd_config and reload sshd.
          </Text>
        </Alert>
      )}
      {rows.map((account) => {
        const r = reported.get(account)
        const m = own[account]
        return (
          <Group key={account} gap={SP.cozy} wrap="wrap" align="center">
            <Stack gap={SP.hair} style={{ flex: '1 1 140px', minWidth: 0 }}>
              <Box style={{ whiteSpace: 'nowrap' }}><Mono>{account}</Mono></Box>
              {r && report
                ? <SignInBadges account={r} auth={report} />
                : report?.reported && <Text size={FS.micro} c="dimmed">Not on this server now</Text>}
            </Stack>
            <Select
              size="xs"
              aria-label={`Direct SSH logins as ${account}`}
              allowDeselect={false}
              disabled={!canEdit || setAccount.isPending}
              value={m ?? INHERIT}
              onChange={(v) => {
                if (!v || v === (m ?? INHERIT)) return
                void change(account, v === INHERIT ? null : (v as DirectSsh))
              }}
              data={[
                { value: INHERIT, label: `Server's (${serverLabel})` },
                ...DIRECT_SSH_CHOICES.map((c) => ({ value: c.value, label: c.label })),
              ]}
              style={{ flex: '1 1 170px' }}
            />
          </Group>
        )
      })}
      {hidden > 0 && (
        <Text size={FS.micro} c="dimmed">
          {hidden} more {hidden === 1 ? 'account has' : 'accounts have'} a login shell but nothing sshd would accept.
        </Text>
      )}
      {canEdit && (
        <Group gap={SP.cozy} align="flex-start" wrap="wrap">
          <Autocomplete
            size="xs"
            placeholder="Another account, e.g. sys-admin"
            aria-label="Login account"
            value={name}
            onChange={setName}
            data={asset.principals.filter((p) => !rows.includes(p))}
            error={nameError}
            style={{ flex: '1 1 160px' }}
          />
          <Select
            size="xs"
            aria-label="What happens when this account logs in directly"
            allowDeselect={false}
            value={mode}
            onChange={(v) => v && setMode(v as DirectSsh)}
            data={DIRECT_SSH_CHOICES.map((c) => ({ value: c.value, label: c.label }))}
            style={{ flex: '0 1 180px' }}
          />
          <Button size="xs" variant="light" disabled={!trimmed || !!nameError} loading={setAccount.isPending} onClick={() => void add()}>
            Add
          </Button>
        </Group>
      )}
    </Stack>
  )
}

