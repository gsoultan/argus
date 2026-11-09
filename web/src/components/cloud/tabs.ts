export const CLOUD_TABS = ['overview', 'instances', 'disks', 'network', 'schedules', 'access', 'changes'] as const
export type CloudTab = (typeof CLOUD_TABS)[number]

export const CLOUD_TAB_LABEL: Record<CloudTab, string> = {
  overview: 'Overview', instances: 'Instances', disks: 'Disks', network: 'Network', schedules: 'Schedules', access: 'Console access', changes: 'Changes',
}

export function parseCloudTab(v: unknown): CloudTab | undefined {
  return CLOUD_TABS.includes(v as CloudTab) && v !== 'overview' ? (v as CloudTab) : undefined
}
