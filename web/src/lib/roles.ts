import type { UserRole } from '~/types/domain'

/**
 * The control plane's read rule, mirrored: `seesWholeFleet` in
 * `internal/control/asset_routes.go`.
 *
 * Admins, owners and auditors are sent the whole inventory; everyone else is
 * sent only the servers assigned to them. The console uses this to decide what
 * to *show*, never what to allow -- the server has already narrowed the data.
 */
export function seesWholeFleet(role: UserRole | undefined): boolean {
  return role === 'admin' || role === 'owner' || role === 'auditor'
}

/**
 * Who a page is for, in the menu's terms.
 *
 * - `fleet`: roles sent the whole fleet (admin, owner, auditor).
 * - `admin`: those who can change it (admin, owner). People and roles are an
 *   administrator's page, and an auditor has nothing to do on it.
 * - `approvers`: the fleet roles, and approvers, whose job is the request queue.
 *
 * Omitted means everyone. Someone who was only given servers sees Servers
 * and nothing else: they asked for a console that is a list of their servers.
 */
export type Audience = 'fleet' | 'admin' | 'approvers'

export function canSee(role: UserRole | undefined, audience: Audience | undefined): boolean {
  switch (audience) {
    case undefined:
      return true
    case 'fleet':
      return seesWholeFleet(role)
    case 'admin':
      return role === 'admin' || role === 'owner'
    case 'approvers':
      return seesWholeFleet(role) || role === 'approver'
  }
}
