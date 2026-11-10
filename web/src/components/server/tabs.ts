/**
 * The tabs of an asset's page, in order. The first is the default. Every type
 * keeps this order; a tab that does not apply to a type is left out, never
 * shown empty (see TABS_BY_TYPE in tabsByType.ts, kept apart so the route's
 * search check, which is in the shell, does not pull in the type labels).
 */
export const SERVER_TABS = ['overview', 'metrics', 'access', 'sessions', 'apps', 'updates', 'disks', 'logs', 'agent', 'settings'] as const
export type ServerTab = (typeof SERVER_TABS)[number]

/** A tab named in the address, or none for the default. */
export function parseServerTab(v: unknown): ServerTab | undefined {
  return typeof v === 'string' && v !== 'overview' && (SERVER_TABS as readonly string[]).includes(v)
    ? (v as ServerTab)
    : undefined
}
