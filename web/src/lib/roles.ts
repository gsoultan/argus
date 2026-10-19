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
