/**
 * The object storage services recordings can be copied to, as the Storage
 * settings card offers them. The control plane knows two APIs, S3 and Azure
 * Blob Storage; Amazon S3, Cloudflare R2 and every other S3-compatible
 * service are the S3 API with a different endpoint, so the card offers them
 * by name and fills in what tells them apart.
 */

export type StorageProvider = 's3' | 'azure'
export type StorageService = 'aws' | 'r2' | 'other' | 'azure'

export interface StorageFields {
  provider?: StorageProvider
  endpoint: string
  region: string
  useSsl: boolean
}

export const STORAGE_SERVICES: { value: StorageService; label: string }[] = [
  { value: 'aws', label: 'Amazon S3' },
  { value: 'r2', label: 'Cloudflare R2' },
  { value: 'azure', label: 'Azure Blob Storage' },
  { value: 'other', label: 'Other S3-compatible' },
]

const R2_SUFFIX = '.r2.cloudflarestorage.com'

/** Which service a saved configuration is, read from its endpoint. */
export function serviceOf(s: StorageFields): StorageService {
  if (s.provider === 'azure') return 'azure'
  const host = s.endpoint.trim().toLowerCase().replace(/^https?:\/\//, '')
  if (host.endsWith(R2_SUFFIX) || host.endsWith(R2_SUFFIX + '/')) return 'r2'
  // R2 chosen and its endpoint not typed yet: "auto" is R2's region alone.
  if (host === '' && s.region === 'auto') return 'r2'
  if (host === '' || /(^|\.)amazonaws\.com\/?$/.test(host)) return 'aws'
  return 'other'
}

/**
 * The fields to change when the operator picks a service: the provider, and
 * the endpoint, region and TLS that service always has. What the operator
 * typed for a field the new service shares is kept.
 */
export function switchService<T extends StorageFields>(s: T, to: StorageService,
  from: StorageService = serviceOf(s)): Partial<T> {
  if (from === to) return {}
  // An endpoint made for the service being left means nothing to the new one.
  const endpoint = from === 'other' && to === 'other' ? s.endpoint : ''
  switch (to) {
    case 'aws':
      return { provider: 's3', endpoint: 's3.amazonaws.com', region: from === 'aws' ? s.region : '', useSsl: true } as Partial<T>
    case 'r2':
      return { provider: 's3', endpoint: '', region: 'auto', useSsl: true } as Partial<T>
    case 'azure':
      return { provider: 'azure', endpoint: '', region: '', useSsl: true } as Partial<T>
    case 'other':
      return { provider: 's3', endpoint, region: '', useSsl: true } as Partial<T>
  }
}

/**
 * Storage classes a recording can be read back from at once. Glacier, Deep
 * Archive and Azure's Archive tier are left out on purpose: a recording there
 * cannot be replayed until a restore that takes hours, and the control plane
 * refuses them for the same reason.
 */
export const S3_ARCHIVE_CLASSES = [
  { value: '', label: "Bucket's default" },
  { value: 'STANDARD_IA', label: 'Standard-IA' },
  { value: 'ONEZONE_IA', label: 'One Zone-IA' },
  { value: 'INTELLIGENT_TIERING', label: 'Intelligent-Tiering' },
  { value: 'GLACIER_IR', label: 'Glacier Instant Retrieval' },
]

export const AZURE_ARCHIVE_TIERS = [
  { value: '', label: "Account's default tier" },
  { value: 'Hot', label: 'Hot' },
  { value: 'Cool', label: 'Cool' },
  { value: 'Cold', label: 'Cold' },
]

/** The archive classes the service offers. R2 has only Infrequent Access. */
export function archiveClassesFor(service: StorageService) {
  if (service === 'azure') return AZURE_ARCHIVE_TIERS
  if (service === 'r2') return S3_ARCHIVE_CLASSES.filter((c) => c.value === '' || c.value === 'STANDARD_IA')
  return S3_ARCHIVE_CLASSES
}
