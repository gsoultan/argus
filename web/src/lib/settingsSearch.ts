/**
 * The tab a database's or broker's settings page opens on, from its address.
 * Its own module, not the page's: the route definitions that use it are in
 * the entry bundle, and importing the page from there would put the whole
 * settings page in every first visit.
 */
export type SettingsTab = 'who' | 'safety' | 'permissions' | 'connection' | 'backups'

export function settingsSearch(s: Record<string, unknown>): { tab?: SettingsTab } {
  return s.tab === 'safety' || s.tab === 'permissions' || s.tab === 'connection' || s.tab === 'backups' ? { tab: s.tab } : {}
}
