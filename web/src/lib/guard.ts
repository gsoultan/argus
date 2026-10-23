import type { QueryClient } from '@tanstack/react-query'
import { redirect } from '@tanstack/react-router'
import { meQuery } from '~/lib/queries'
import { canSee, type Audience } from '~/lib/roles'

/**
 * Sends a role that is not this page's audience to Servers, which every role
 * may open.
 *
 * The menu hides these pages, but a bookmark or a typed address would still
 * land on them, and "only my servers" has to hold for those too. This is about
 * what is shown, not what is allowed: the control plane already narrows every
 * list, and refuses every write, for these roles on its own.
 *
 * A failed identity lookup lets the page render: the sign-in gate and the
 * page's own error state are better placed to say what went wrong than a
 * redirect would be.
 */
export async function guard(queryClient: QueryClient, audience: Audience): Promise<void> {
  const me = await queryClient.ensureQueryData(meQuery()).catch(() => undefined)
  if (me && !canSee(me.role, audience)) throw redirect({ to: '/assets', replace: true })
}
