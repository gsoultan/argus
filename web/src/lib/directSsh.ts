import type { DirectSsh } from '~/types/domain'

/**
 * The three positions for direct SSH logins, in the words the console uses
 * everywhere: Settings, a server's page, and the approval queue. Mirrors
 * `directSSHLabel` in `internal/control/direct_login.go`.
 */
export const DIRECT_SSH_CHOICES: { value: DirectSsh; label: string; description: string }[] = [
  {
    value: 'refuse',
    label: 'Refuse',
    description: 'The agent turns the login away before anything runs, tells the person to use the console, and writes it to the audit log.',
  },
  {
    value: 'approve',
    label: 'Ask for approval',
    description: 'The login waits at the terminal while an admin or approver approves or denies it on Access requests. Nobody answering in two minutes, or Argus being unreachable, refuses it.',
  },
  {
    value: 'allow',
    label: 'Allow and record',
    description: 'The login opens straight away with nobody approving it. It is recorded like any session and reported in the audit log.',
  },
]

export function directSshLabel(mode: DirectSsh): string {
  return DIRECT_SSH_CHOICES.find((c) => c.value === mode)?.label ?? 'Refuse'
}

const RANK: Record<DirectSsh, number> = { refuse: 0, approve: 1, allow: 2 }

/** Whether moving from one position to another lets more logins in. */
export function directSshLoosens(from: DirectSsh, to: DirectSsh): boolean {
  return RANK[to] > RANK[from]
}
