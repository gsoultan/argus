import {
  get,
  send,
  type LogCleanupPolicy,
  type LogPick,
  type LogRun,
  type LogRunState,
  type LogsView,
} from '~/lib/live'

/*
 * Log cleanup's calls. Here rather than in live.ts, which every page loads,
 * so only the pages that show logs pay for them.
 */
export const logRunFinished = (s: LogRunState) =>
  s === 'done' || s === 'failed' || s === 'interrupted' || s === 'cancelled' || s === 'denied'

export async function assetLogs(assetId: string): Promise<LogsView> {
  return get(`/api/v1/assets/${encodeURIComponent(assetId)}/logs`)
}

export async function scanLogs(assetId: string): Promise<{ scanId: string }> {
  return send('POST', `/api/v1/assets/${encodeURIComponent(assetId)}/logs/scan`)
}

export async function startLogRun(assetId: string, body: LogPick & { confirm: string }): Promise<LogRun> {
  return send('POST', `/api/v1/assets/${encodeURIComponent(assetId)}/logs/runs`, body)
}

export async function logRun(id: string): Promise<LogRun> {
  return get(`/api/v1/log-runs/${encodeURIComponent(id)}`)
}

export async function decideLogRun(id: string, action: 'approve' | 'deny' | 'cancel'): Promise<LogRun> {
  return send('POST', `/api/v1/log-runs/${encodeURIComponent(id)}/${action}`)
}

export async function logPolicy(): Promise<LogCleanupPolicy> {
  return get('/api/v1/settings/log-cleanup')
}

export async function saveLogPolicy(p: LogCleanupPolicy): Promise<LogCleanupPolicy> {
  return send('PUT', '/api/v1/settings/log-cleanup', p)
}
