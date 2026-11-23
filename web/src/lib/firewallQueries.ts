/*
 * The firewall on each server. Live only: a sample server's firewall shown
 * as if real would mislead, so against no control plane these queries do
 * not run and the views say they need one.
 */
import { queryOptions, useMutation, useQueryClient } from '@tanstack/react-query'
import {
  actOnFirewallChange, actOnFirewallRollout, addFirewallTarget, askFirewallChange, assetFirewall,
  createFirewallPolicy, deleteFirewallPolicy, firewallFleet, firewallPolicies, firewallPolicy, firewallStarters,
  removeFirewallTarget, saveFirewallPolicy, setFirewallManagers,
  type FirewallAsk, type FirewallPolicyInput, type FWChangeAction, type FWRolloutAction,
} from '~/lib/firewallApi'
import { isConfigured } from '~/lib/live'

export const assetFirewallQuery = (assetId: string) =>
  queryOptions({
    queryKey: ['firewall', 'asset', assetId] as const,
    queryFn: () => assetFirewall(assetId),
    enabled: isConfigured(),
    // Faster while a change is on its way: the trial is two minutes.
    refetchInterval: (q) => (q.state.data?.manage?.open ? 5_000 : 60_000),
  })

export const firewallFleetQuery = () =>
  queryOptions({
    queryKey: ['firewall', 'fleet'] as const,
    queryFn: firewallFleet,
    enabled: isConfigured(),
    refetchInterval: 60_000,
  })

export const firewallPoliciesQuery = () =>
  queryOptions({
    queryKey: ['firewall', 'policies'] as const,
    queryFn: firewallPolicies,
    enabled: isConfigured(),
  })

export const firewallStartersQuery = () =>
  queryOptions({
    queryKey: ['firewall', 'starters'] as const,
    queryFn: firewallStarters,
    enabled: isConfigured(),
    staleTime: Infinity,
  })

export const firewallPolicyQuery = (id: string) =>
  queryOptions({
    queryKey: ['firewall', 'policy', id] as const,
    queryFn: () => firewallPolicy(id),
    enabled: isConfigured(),
    refetchInterval: (q) => (q.state.data?.rollout?.state === 'running' ? 5_000 : 60_000),
  })

function useInvalidate() {
  const qc = useQueryClient()
  return () => void qc.invalidateQueries({ queryKey: ['firewall'] })
}

export function useAskFirewallChange(assetId: string) {
  const done = useInvalidate()
  return useMutation({ mutationFn: (ask: FirewallAsk) => askFirewallChange(assetId, ask), onSettled: done })
}

export function useActOnFirewallChange() {
  const done = useInvalidate()
  return useMutation({
    mutationFn: (v: { id: string; action: FWChangeAction; confirmSelf?: string }) => actOnFirewallChange(v.id, v.action, v.confirmSelf),
    onSettled: done,
  })
}

export function useSetFirewallManagers(assetId: string) {
  const done = useInvalidate()
  return useMutation({ mutationFn: (emails: string[]) => setFirewallManagers(assetId, emails), onSettled: done })
}

export function useSaveFirewallPolicy(id?: string) {
  const done = useInvalidate()
  return useMutation({
    mutationFn: (p: FirewallPolicyInput) => (id ? saveFirewallPolicy(id, p) : createFirewallPolicy(p)),
    onSettled: done,
  })
}

export function useDeleteFirewallPolicy() {
  const done = useInvalidate()
  return useMutation({ mutationFn: (id: string) => deleteFirewallPolicy(id), onSettled: done })
}

export function useFirewallTarget(id: string) {
  const done = useInvalidate()
  return useMutation({
    mutationFn: (v: { add?: { assetId?: string; tag?: string }; remove?: string }) =>
      v.remove ? removeFirewallTarget(id, v.remove) : addFirewallTarget(id, v.add ?? {}),
    onSettled: done,
  })
}

export function useActOnFirewallRollout() {
  const done = useInvalidate()
  return useMutation({
    mutationFn: (v: { id: string; action: FWRolloutAction; confirmSelf?: string }) => actOnFirewallRollout(v.id, v.action, v.confirmSelf),
    onSettled: done,
  })
}
