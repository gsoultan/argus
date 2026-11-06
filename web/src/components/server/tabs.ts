/** The tabs of a server's page, in order. The first is the default. */
export const SERVER_TABS = ['overview', 'access', 'sessions', 'apps', 'updates', 'settings'] as const
export type ServerTab = (typeof SERVER_TABS)[number]

export const SERVER_TAB_LABEL: Record<ServerTab, string> = {
  overview: 'Overview',
  access: 'Access',
  sessions: 'Sessions',
  apps: 'What runs here',
  updates: 'Updates',
  settings: 'Settings',
}

/** A tab named in the address, or none for the default. */
export function parseServerTab(v: unknown): ServerTab | undefined {
  return typeof v === 'string' && v !== 'overview' && (SERVER_TABS as readonly string[]).includes(v)
    ? (v as ServerTab)
    : undefined
}
