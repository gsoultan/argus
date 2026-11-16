import { queryOptions } from '@tanstack/react-query'
import { get, isConfigured } from '~/lib/live'
import type { CloudItem } from '~/lib/cloud'

/* ── Suggestions from Argus's own metrics ──────────────────────────────── */

export type SuggestionKind = 'add-server' | 'resize-up' | 'resize-down'

/** A busy or idle server, and what would help; it never acts by itself. */
export interface ServerSuggestion {
  kind: SuggestionKind
  assetId: string
  hostname: string
  title: string
  detail: string
  instance: CloudItem
  /** What to resize to. */
  size?: string
  /** What an added server goes behind. */
  balancer?: string
  targetGroup?: string
  /** A month, negative when it saves; absent when the estimate does not know the size. */
  monthly?: number
  load: { busyHours: number; hours: number; peakCpu: number; peakMem: number; days?: number }
}

export const assetSuggestionQuery = (assetId: string, enabled = true) =>
  queryOptions({
    queryKey: ['asset', assetId, 'suggestion'] as const,
    queryFn: () => get<{ suggestion: ServerSuggestion | null }>(`/api/v1/assets/${encodeURIComponent(assetId)}/suggestion`).then((r) => r.suggestion),
    enabled: enabled && isConfigured(),
    refetchInterval: 5 * 60_000,
    retry: false,
  })

export const cloudSuggestionsQuery = (enabled = true) =>
  queryOptions({
    queryKey: ['cloud', 'suggestions'] as const,
    queryFn: () => get<{ suggestions: ServerSuggestion[] }>('/api/v1/cloud/suggestions').then((r) => r.suggestions),
    enabled: enabled && isConfigured(),
    refetchInterval: 5 * 60_000,
  })

/** What pressing a suggestion does, in words. */
export function suggestionAction(s: ServerSuggestion): string {
  if (s.kind === 'add-server') return `Add a server behind ${s.balancer}`
  return `Resize to ${s.size}`
}
