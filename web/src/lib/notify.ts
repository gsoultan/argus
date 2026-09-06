import { notifications } from '@mantine/notifications'

/**
 * Outcome reporting for mutations.
 *
 * Every privileged action in Argus either happened or did not, and the operator
 * has to be able to tell which. `mutateAsync` rejects on refusal, so a call site
 * without a catch shows neither the success toast nor an error — the button
 * simply un-loads and the user concludes it worked. That is the wrong failure
 * for approving access or pinning a host key, so these wrappers exist to make
 * the guarded form the shortest one to write.
 */

export function notifyOk(title: string, message: string): void {
  notifications.show({ color: 'teal', title, message })
}

export function notifyWarn(title: string, message: string): void {
  notifications.show({ color: 'amber', title, message })
}

/** The refusal text from the control plane is shown verbatim — it is the useful part. */
export function notifyError(title: string, err: unknown): void {
  notifications.show({
    color: 'rose',
    title,
    message:
      err instanceof Error && err.message
        ? err.message
        : 'The control plane refused the request and gave no reason.',
  })
}

/**
 * Runs a mutation and reports either outcome.
 *
 * Returns whether it succeeded, so a caller can keep a dialog open on failure
 * rather than dismissing it over a session that is still running.
 */
export async function run(
  action: () => Promise<unknown>,
  on: { failure: string; success?: [string, string] },
): Promise<boolean> {
  try {
    await action()
  } catch (err) {
    notifyError(on.failure, err)
    return false
  }
  if (on.success) notifyOk(on.success[0], on.success[1])
  return true
}
