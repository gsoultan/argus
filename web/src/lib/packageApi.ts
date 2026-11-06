import {
  get,
  send,
  type PackageFleetRow,
  type PackagePolicy,
  type PackageReboot,
  type PackageRun,
  type PackageRunState,
  type PackagesView,
} from '~/lib/live'

/*
 * Operating system updates' calls. Here rather than in live.ts, which every
 * page loads, so only the pages that show updates pay for them.
 */
export const runFinished = (s: PackageRunState) =>
  s === 'done' || s === 'failed' || s === 'interrupted' || s === 'cancelled' || s === 'denied'

export async function assetPackages(assetId: string): Promise<PackagesView> {
  return get(`/api/v1/assets/${encodeURIComponent(assetId)}/packages`)
}

export async function checkPackages(assetId: string): Promise<{ checkId: string }> {
  return send('POST', `/api/v1/assets/${encodeURIComponent(assetId)}/packages/check`)
}

export async function startPackageRun(
  assetId: string,
  body: { mode: 'all' | 'security' | 'selected'; packages?: string[]; reboot: PackageReboot },
): Promise<PackageRun> {
  return send('POST', `/api/v1/assets/${encodeURIComponent(assetId)}/packages/runs`, body)
}

export async function startPackageRuns(body: {
  assetIds: string[]
  mode: 'all' | 'security'
  reboot: PackageReboot
}): Promise<PackageRun[]> {
  return send('POST', '/api/v1/package-runs', body)
}

export async function packageRun(id: string): Promise<PackageRun> {
  return get(`/api/v1/package-runs/${encodeURIComponent(id)}`)
}

export async function decidePackageRun(id: string, action: 'approve' | 'deny' | 'cancel'): Promise<PackageRun> {
  return send('POST', `/api/v1/package-runs/${encodeURIComponent(id)}/${action}`)
}

export async function packageFleet(): Promise<PackageFleetRow[]> {
  return get('/api/v1/package-updates')
}

export async function packagePolicy(): Promise<PackagePolicy> {
  return get('/api/v1/settings/package-updates')
}

export async function savePackagePolicy(p: PackagePolicy): Promise<PackagePolicy> {
  return send('PUT', '/api/v1/settings/package-updates', p)
}

/*
 * Operating system updates. Live only: there is no sample of a server's
 * package list worth showing as if it were real, so against no control plane
 * these queries do not run and the views say they need one.
 */
