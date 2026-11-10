import type { Asset, AssetAddress } from '~/types/domain'

/**
 * Which of a server's addresses to show first, and the rest.
 *
 * The agent reports every address its interfaces hold, in interface order,
 * which puts Docker's bridge beside the address anyone would use. One rule
 * picks the one to show: a public address before a private one (on the
 * machine itself it is the one that names it; private ranges repeat from one
 * network to the next), IPv4 before IPv6, and a container or VM bridge last.
 * The rest stay one press away rather than being dropped.
 */

/** Interfaces a container runtime or hypervisor makes, never how people reach the host. */
const BRIDGE = /^(docker\d*|br-|veth|virbr|cni|flannel|cali|cilium|lxcbr|lxdbr|podman|kube-|vnet)/i

/** Whether s is an IPv4 or IPv6 literal, bracketed or not. */
export function isIpLiteral(s: string): boolean {
  const v = s.replace(/^\[|\]$/g, '')
  if (/^(25[0-5]|2[0-4]\d|1?\d?\d)(\.(25[0-5]|2[0-4]\d|1?\d?\d)){3}$/.test(v)) return true
  return v.includes(':') && /^[0-9a-f:.]+$/i.test(v)
}

function rank(a: AssetAddress): number {
  return (BRIDGE.test(a.interface ?? '') ? 4 : 0) + (a.family === 'ipv6' ? 2 : 0) + (a.public ? 0 : 1)
}

export interface ShownAddress extends AssetAddress {
  /** Typed into the asset by an administrator rather than reported by its agent. */
  entered?: boolean
}

/**
 * The addresses to show for an asset, best first.
 *
 * `withEntered` puts the address an administrator entered first when it is
 * not one the agent reported (a DNS name, or an IP the machine does not hold
 * itself, such as a cloud provider's NAT address): the detail page says what
 * Argus was told as well as what it saw. The list shows only IPs, so without
 * it the entered address counts only when it is an IP and nothing was
 * reported, which is what an operator, who is never sent the agent's list,
 * sees.
 */
export function addressesOf(a: Pick<Asset, 'address' | 'addresses' | 'hostname'>, withEntered = false): ShownAddress[] {
  const reported = [...(a.addresses ?? [])]
    .map((x, i) => ({ x, i }))
    .sort((p, q) => rank(p.x) - rank(q.x) || p.i - q.i)
    .map(({ x }) => x as ShownAddress)
  const entered = a.address?.trim()
  if (!entered || reported.some((r) => r.address === entered.replace(/^\[|\]$/g, ''))) return reported
  const ip = isIpLiteral(entered)
  if (withEntered ? entered !== a.hostname : ip && reported.length === 0) {
    return [{ address: entered, family: entered.includes(':') ? 'ipv6' : 'ipv4', public: false, entered: true }, ...reported]
  }
  return reported
}
