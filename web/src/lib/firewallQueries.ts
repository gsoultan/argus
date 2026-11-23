/*
 * The firewall on each server. Live only: a sample server's firewall shown
 * as if real would mislead, so against no control plane these queries do
 * not run and the views say they need one.
 */
import { queryOptions } from '@tanstack/react-query'
import { assetFirewall, firewallFleet } from '~/lib/firewallApi'
import { isConfigured } from '~/lib/live'

export const assetFirewallQuery = (assetId: string) =>
  queryOptions({
    queryKey: ['firewall', 'asset', assetId] as const,
    queryFn: () => assetFirewall(assetId),
    enabled: isConfigured(),
    refetchInterval: 60_000,
  })

export const firewallFleetQuery = () =>
  queryOptions({
    queryKey: ['firewall', 'fleet'] as const,
    queryFn: firewallFleet,
    enabled: isConfigured(),
    refetchInterval: 60_000,
  })
