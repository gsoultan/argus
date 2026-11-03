import { useState } from 'react'
import { Alert, Button, Center, Group, List, Modal, Paper, PasswordInput, Stack, Text, ThemeIcon } from '@mantine/core'
import { IconCheck, IconKey, IconX } from '@tabler/icons-react'
import { changePassword, logout } from '~/lib/live'
import { checkPassword, passwordAcceptable } from '~/lib/passwordStrength'
import { FS } from '~/theme'
import { Brand, ErrorNote } from '~/components/SignIn'

/**
 * Choosing a password: the current one, the new one twice, and the rules
 * ticked off as they are met.
 *
 * The control plane checks the same rules and has the last word; its refusal
 * is shown as it says it. Changing the password signs out every other
 * browser, which is said up front rather than discovered.
 */
export function ChoosePasswordForm({ email, name, temporary, onDone }: {
  email?: string | undefined
  name?: string | undefined
  /** The current password is a temporary one they were sent. */
  temporary: boolean
  onDone: () => void
}) {
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [again, setAgain] = useState('')
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)
  const ctx = { email: email ?? '', name: name ?? '' }
  const checks = checkPassword(next, ctx)
  const matches = next !== '' && next === again
  const same = next !== '' && next === current
  const ready = current !== '' && passwordAcceptable(next, ctx) && matches && !same

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!ready) return
    setBusy(true)
    setError(undefined)
    try {
      await changePassword(current, next)
      setCurrent('')
      setNext('')
      setAgain('')
      onDone()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'The password was not changed.')
    } finally {
      setBusy(false)
    }
  }

  return (
    <form onSubmit={submit}>
      <Stack gap="sm">
        {error && <ErrorNote>{error}</ErrorNote>}
        <PasswordInput
          label={temporary ? 'Temporary password' : 'Current password'}
          description={temporary ? 'The one in the email, or the one your administrator gave you.' : undefined}
          autoComplete="current-password"
          value={current}
          onChange={(e) => setCurrent(e.currentTarget.value)}
          data-autofocus
          required
        />
        <PasswordInput
          label="New password"
          autoComplete="new-password"
          value={next}
          onChange={(e) => setNext(e.currentTarget.value)}
          required
        />
        <List size="xs" spacing={4} center aria-label="Password rules">
          {checks.map((c) => (
            <List.Item key={c.label}
              icon={
                <ThemeIcon size={16} radius="xl" color={c.ok ? 'teal' : 'slate'} variant={c.ok ? 'filled' : 'light'}>
                  {c.ok ? <IconCheck size={11} /> : <IconX size={11} />}
                </ThemeIcon>
              }>
              <Text size={FS.meta} c={c.ok ? undefined : 'dimmed'}>{c.label}</Text>
            </List.Item>
          ))}
        </List>
        <PasswordInput
          label="New password again"
          autoComplete="new-password"
          value={again}
          onChange={(e) => setAgain(e.currentTarget.value)}
          error={again !== '' && !matches ? 'The two do not match' : same ? 'Choose a different password from the current one' : undefined}
          required
        />
        <Text size={FS.micro} c="dimmed">
          A password manager can make and keep one for you. Changing it signs out every other
          browser signed in as you.
        </Text>
        <Button type="submit" loading={busy} disabled={!ready} fullWidth>
          {temporary ? 'Set my password' : 'Change password'}
        </Button>
      </Stack>
    </form>
  )
}

/**
 * The whole console while an account is signed in with a temporary password.
 * The control plane refuses everything else, so drawing anything else would
 * be drawing errors.
 */
export function PasswordChangeRequired({ email, name, expiresAt, onDone }: {
  email?: string | undefined
  name?: string | undefined
  expiresAt?: string | null | undefined
  onDone: () => void
}) {
  return (
    <Center mih="100vh" p="md">
      <Paper p="xl" withBorder maw={440} w="100%" radius="lg">
        <Brand />
        <Alert color="azure" variant="light" icon={<IconKey size={16} />} mb="md">
          <Text size="xs">
            You signed in{email ? ` as ${email}` : ''} with a temporary password. Choose one of your
            own to continue; the temporary one stops working once you do
            {expiresAt ? `, and on its own at ${new Date(expiresAt).toLocaleString()}` : ''}.
          </Text>
        </Alert>
        <ChoosePasswordForm email={email} name={name} temporary onDone={onDone} />
        <Group justify="center" mt="md">
          <Button variant="subtle" color="slate" size="compact-xs"
            onClick={async () => {
              await logout()
              window.location.reload()
            }}>
            Sign out
          </Button>
        </Group>
      </Paper>
    </Center>
  )
}

/** "Change my password", from the account menu. */
export function ChangePasswordModal({ opened, email, name, onClose }: {
  opened: boolean
  email?: string | undefined
  name?: string | undefined
  onClose: () => void
}) {
  const [done, setDone] = useState(false)
  return (
    <Modal opened={opened} onClose={onClose} title="Change my password">
      {done ? (
        <Stack gap="md">
          <Text size={FS.body}>Your password is changed. Every other browser signed in as you has to sign in again.</Text>
          <Group justify="flex-end"><Button onClick={onClose}>Done</Button></Group>
        </Stack>
      ) : (
        <ChoosePasswordForm email={email} name={name} temporary={false} onDone={() => setDone(true)} />
      )}
    </Modal>
  )
}
