import { describe, expect, it } from 'vitest'
import { archiveClassesFor, serviceOf, switchService, type StorageFields } from '~/lib/objectStorage'

const s = (patch: Partial<StorageFields>): StorageFields => ({ endpoint: '', region: '', useSsl: true, ...patch })

describe('object storage services', () => {
  it('reads which service a saved configuration is', () => {
    expect(serviceOf(s({ provider: 'azure' }))).toBe('azure')
    expect(serviceOf(s({ endpoint: 'abc123.r2.cloudflarestorage.com' }))).toBe('r2')
    expect(serviceOf(s({ endpoint: 'https://abc123.r2.cloudflarestorage.com/' }))).toBe('r2')
    expect(serviceOf(s({ endpoint: 's3.amazonaws.com' }))).toBe('aws')
    expect(serviceOf(s({ endpoint: '', region: 'auto' }))).toBe('r2')
    expect(serviceOf(s({ endpoint: 's3.eu-west-1.amazonaws.com' }))).toBe('aws')
    expect(serviceOf(s({ endpoint: 'minio.internal:9000' }))).toBe('other')
    // A configuration from before the provider field is S3.
    expect(serviceOf(s({ provider: undefined, endpoint: 'minio.internal:9000' }))).toBe('other')
  })

  it('fills in what a service always has, and drops an endpoint meant for another', () => {
    const aws = s({ provider: 's3', endpoint: 's3.amazonaws.com', region: 'eu-west-1' })
    expect(switchService(aws, 'r2')).toEqual({ provider: 's3', endpoint: '', region: 'auto', useSsl: true })
    expect(switchService(aws, 'azure')).toEqual({ provider: 'azure', endpoint: '', region: '', useSsl: true })
    expect(switchService(aws, 'aws')).toEqual({})
    // What the card says it is wins over what the fields look like.
    expect(switchService(s({ endpoint: '' }), 'aws', 'other'))
      .toEqual({ provider: 's3', endpoint: 's3.amazonaws.com', region: '', useSsl: true })
    expect(switchService(s({ provider: 'azure' }), 'aws'))
      .toEqual({ provider: 's3', endpoint: 's3.amazonaws.com', region: '', useSsl: true })
  })

  it('offers only archive classes a recording replays from at once', () => {
    const values = (svc: Parameters<typeof archiveClassesFor>[0]) => archiveClassesFor(svc).map((c) => c.value)
    expect(values('azure')).toEqual(['', 'Hot', 'Cool', 'Cold'])
    expect(values('r2')).toEqual(['', 'STANDARD_IA'])
    expect(values('aws')).not.toContain('GLACIER')
    expect(values('aws')).not.toContain('DEEP_ARCHIVE')
  })
})
