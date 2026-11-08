import { get, send } from '~/lib/live'

/*
 * Growing disks: the types the control plane sends and its calls. Here rather
 * than in live.ts, which every page loads, so only the pages that show disks
 * pay for them. The report keeps the agent's own snake_case.
 */

export type DiskAnswer = 'grow' | 'full' | 'blocked'

export interface DiskLayer {
  /** disk, partition, crypt, pv, vg, lv or fs; the disk first. */
  kind: 'disk' | 'partition' | 'crypt' | 'pv' | 'vg' | 'lv' | 'fs'
  name: string
  detail?: string
  size: number
  used?: number
  room?: number
  offset?: number
}

export interface GrowPath {
  disk?: string
  disk_size?: number
  disk_serial?: string
  part?: string
  part_num?: number
  part_start?: number
  part_size?: number
  part_room?: number
  table?: string
  crypt?: string
  crypt_room?: number
  pv?: string
  pv_room?: number
  vg?: string
  vg_uuid?: string
  vg_free?: number
  lv?: string
  lv_uuid?: string
  lv_size?: number
  fs_device?: string
  fs_type: string
  fs_size: number
  fs_room?: number
  mount: string
  disk_number?: number
  partition_number?: number
  size_max?: number
}

export interface DiskFS {
  key: string
  mount: string
  fs: string
  total: number
  used: number
  avail: number
  layers: DiskLayer[]
  answer: DiskAnswer
  why?: string
  room?: number
  lvm?: boolean
  rescan?: boolean
  unallocated?: number
  path?: GrowPath
}

export interface BlockDisk {
  name: string
  size: number
  bus?: string
  model?: string
  serial?: string
  table?: string
  unallocated?: number
  read_only?: boolean
}

export interface DiskReport {
  scanned_at: string
  os: string
  supported: boolean
  reason?: string
  error?: string
  hypervisor?: string
  filesystems: DiskFS[]
  disks: BlockDisk[]
  rescanned?: boolean
}

export interface DiskStep {
  title: string
  command?: string
  output?: string
  state: 'running' | 'done' | 'failed' | string
}

export type DiskRunState =
  | 'awaiting_approval' | 'queued' | 'running' | 'done' | 'failed' | 'interrupted' | 'cancelled' | 'denied'

export interface DiskRun {
  id: string
  hostname: string
  assetHostname: string
  assetId?: string
  trigger: 'manual' | 'automatic'
  state: DiskRunState
  key: string
  mount: string
  path: GrowPath
  growBytes: number
  plannedBytes: number
  summary: string
  reason: string
  requestedBy: string
  requestedAt: string
  decidedBy: string | null
  decidedAt: string | null
  sentAt: string | null
  startedAt: string | null
  finishedAt: string | null
  updatedAt: string
  step: string
  message: string
  steps: DiskStep[]
  tableBefore: string
  sizeBefore: number
  sizeAfter: number
}

export interface DiskGrowRule {
  id?: string
  position: number
  tag: string
  needsApproval: boolean
  /** Grow on its own past this percent; 0 never. */
  autoPercent: number
}

export interface DiskGrowPolicy {
  rules: DiskGrowRule[]
}

export interface DisksView {
  hostname: string
  enrolled: boolean
  reported: boolean
  supported: boolean
  reason: string
  scanning: boolean
  scanAsked: boolean
  report: DiskReport | null
  rule: DiskGrowRule
  runs: DiskRun[]
}

export const diskRunFinished = (s: DiskRunState) =>
  s === 'done' || s === 'failed' || s === 'interrupted' || s === 'cancelled' || s === 'denied'

export async function assetDisks(assetId: string): Promise<DisksView> {
  return get(`/api/v1/assets/${encodeURIComponent(assetId)}/disks`)
}

export async function scanDisks(assetId: string, rescan: boolean): Promise<{ scanId: string }> {
  return send('POST', `/api/v1/assets/${encodeURIComponent(assetId)}/disks/scan`, { rescan })
}

export async function startDiskGrow(assetId: string, body: { key: string; bytes: number }): Promise<DiskRun> {
  return send('POST', `/api/v1/assets/${encodeURIComponent(assetId)}/disks/grow`, body)
}

export async function diskRun(id: string): Promise<DiskRun> {
  return get(`/api/v1/disk-runs/${encodeURIComponent(id)}`)
}

export async function decideDiskRun(id: string, action: 'approve' | 'deny' | 'cancel'): Promise<DiskRun> {
  return send('POST', `/api/v1/disk-runs/${encodeURIComponent(id)}/${action}`)
}

export async function diskPolicy(): Promise<DiskGrowPolicy> {
  return get('/api/v1/settings/disk-grow')
}

export async function saveDiskPolicy(p: DiskGrowPolicy): Promise<DiskGrowPolicy> {
  return send('PUT', '/api/v1/settings/disk-grow', p)
}
