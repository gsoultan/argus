/*
 * The Networks page's tabs, apart from the page itself: the route's
 * validateSearch is not code-split, so whatever it imports lands in every
 * first visit. Importing the page for this pulled all of Networks into it.
 */

/** The views of the page, each a ?tab= value. The first is the page itself. */
export const NETWORK_TABS = ['overview', 'wireguard', 'links', 'staff', 'cloud', 'cloudflare', 'software'] as const
export type NetworkTab = (typeof NETWORK_TABS)[number]

export const NETWORK_TAB_LABEL: Record<NetworkTab, string> = {
  overview: 'Overview',
  wireguard: 'WireGuard',
  links: 'Clouds and offices',
  staff: 'Staff VPN',
  cloud: 'Cloud VPNs',
  cloudflare: 'Cloudflare',
  software: 'Other VPN software',
}

export function parseNetworkTab(v: unknown): NetworkTab | undefined {
  return NETWORK_TABS.includes(v as NetworkTab) && v !== 'overview' ? (v as NetworkTab) : undefined
}
