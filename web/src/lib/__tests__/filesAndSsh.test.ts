import { describe, expect, it } from 'vitest'
import { baseName, crumbs, freeName, joinPath, parentPath, validName } from '~/lib/files/paths'
import { crc32, makeZip } from '~/lib/files/zip'
import { directCommand, shortCommand, sshConfig } from '~/lib/sshConfig'
import { cwdFromOsc7, cwdFromTitle } from '~/components/LiveTerminal'

describe('paths on the server', () => {
  it('joins, climbs and names', () => {
    expect(joinPath('/var/log', 'syslog')).toBe('/var/log/syslog')
    expect(joinPath('/', 'etc')).toBe('/etc')
    expect(parentPath('/var/log')).toBe('/var')
    expect(parentPath('/var')).toBe('/')
    expect(parentPath('/')).toBe('/')
    expect(parentPath('~/app')).toBe('~')
    expect(baseName('/var/log/nginx/')).toBe('nginx')
  })

  it('breaks a path into crumbs from the root or home', () => {
    expect(crumbs('/var/log').map((c) => c.path)).toEqual(['/', '/var', '/var/log'])
    expect(crumbs('~/app/logs').map((c) => c.path)).toEqual(['~', '~/app', '~/app/logs'])
  })

  it('finds a free name for Keep both, leaving a dotfile whole', () => {
    expect(freeName('report.csv', ['report.csv'])).toBe('report (1).csv')
    expect(freeName('report.csv', ['report.csv', 'report (1).csv'])).toBe('report (2).csv')
    expect(freeName('.env', ['.env'])).toBe('.env (1)')
    expect(freeName('new.txt', ['other'])).toBe('new.txt')
  })

  it('refuses names that are not one segment', () => {
    expect(validName('a/b')).not.toBeNull()
    expect(validName('..')).not.toBeNull()
    expect(validName('  ')).not.toBeNull()
    expect(validName('logs')).toBeNull()
  })
})

describe('zipping downloads in the browser', () => {
  it('computes the standard CRC-32', () => {
    expect(crc32(new TextEncoder().encode('123456789')) >>> 0).toBe(0xcbf43926)
  })

  it('writes a zip with a local header per file and an end record', async () => {
    const enc = new TextEncoder()
    const blob = makeZip([
      { name: 'a.txt', data: enc.encode('hello') as Uint8Array<ArrayBuffer> },
      { name: 'b.log', data: enc.encode('world!') as Uint8Array<ArrayBuffer> },
    ])
    const b = new Uint8Array(await blob.arrayBuffer())
    const view = new DataView(b.buffer)
    expect(view.getUint32(0, true)).toBe(0x04034b50)
    const eocd = b.length - 22
    expect(view.getUint32(eocd, true)).toBe(0x06054b50)
    expect(view.getUint16(eocd + 10, true)).toBe(2)
    expect(new TextDecoder().decode(b)).toContain('a.txt')
  })
})

describe('reaching your own account with ssh', () => {
  const web = { hostname: 'web-01.prod.northwind.id', address: '10.20.4.17' }

  it('gives a short command and one that needs no config', () => {
    expect(shortCommand(web)).toBe('ssh web-01')
    expect(directCommand('gembit', web)).toBe('ssh -i ~/.ssh/argus_ed25519 gembit@10.20.4.17')
    expect(directCommand('gembit', { ...web, address: '' })).toBe('ssh -i ~/.ssh/argus_ed25519 gembit@web-01.prod.northwind.id')
  })

  it('writes a config entry per server, keeping clashing short names apart', () => {
    const cfg = sshConfig('gembit', [
      web,
      { hostname: 'web-01.stage.northwind.id', address: '10.30.4.17' },
    ], new Date('2026-11-07T00:00:00Z'))
    expect(cfg).toContain('Host web-01\n    HostName 10.20.4.17\n    User gembit\n    IdentityFile ~/.ssh/argus_ed25519')
    expect(cfg).toContain('Host web-01.stage.northwind.id')
    expect(cfg).toContain('2026-11-07')
  })
})

describe('the folder the terminal is in', () => {
  it('reads OSC 7', () => {
    expect(cwdFromOsc7('file://web-01/var/log/my%20app')).toBe('/var/log/my app')
    expect(cwdFromOsc7('garbage')).toBeNull()
  })

  it('reads a user@host:path window title', () => {
    expect(cwdFromTitle('deploy@web-01: /srv/app')).toBe('/srv/app')
    expect(cwdFromTitle('deploy@web-01: ~/releases')).toBe('~/releases')
    expect(cwdFromTitle('vim notes.txt')).toBeNull()
  })
})
