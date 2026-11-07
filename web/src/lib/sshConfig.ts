import { shortName } from '~/lib/reach'

/**
 * The commands and the ~/.ssh/config a person needs to reach their own
 * account on servers with their own ssh client. Built in the browser from
 * what the console already holds -- each server's address and the person's
 * login name -- so there is nothing to fetch and nothing secret in it.
 */

/** Where "Make a key for me" saves the private key, by name. */
export const KEY_FILE = 'argus_ed25519'

export interface SshTarget {
  hostname: string
  address: string
}

/** The address ssh should dial: the recorded address, else the hostname. */
export function dialAddress(t: SshTarget): string {
  return t.address?.trim() || t.hostname
}

/** The one command that always works, without any config. */
export function directCommand(login: string, t: SshTarget, keyFile = `~/.ssh/${KEY_FILE}`): string {
  return `ssh -i ${keyFile} ${login}@${dialAddress(t)}`
}

/** The short command, once the config below is in place. */
export function shortCommand(t: SshTarget): string {
  return `ssh ${shortName(t.hostname)}`
}

/** A config entry per server, under the name people say out loud. */
export function sshConfig(login: string, targets: SshTarget[], now = new Date()): string {
  const seen = new Set<string>()
  const blocks: string[] = []
  for (const t of [...targets].sort((a, b) => a.hostname.localeCompare(b.hostname))) {
    let alias = shortName(t.hostname)
    if (seen.has(alias)) alias = t.hostname
    seen.add(alias)
    blocks.push([
      `Host ${alias}`,
      `    HostName ${dialAddress(t)}`,
      `    User ${login}`,
      `    IdentityFile ~/.ssh/${KEY_FILE}`,
      `    IdentitiesOnly yes`,
    ].join('\n'))
  }
  return [
    `# Argus: servers you may SSH to as ${login}. Made ${now.toISOString().slice(0, 10)}.`,
    '# Add this to ~/.ssh/config. Every session is recorded, as in the console.',
    `# Using a key of your own? Change IdentityFile to where it is.`,
    '',
    blocks.join('\n\n'),
    '',
  ].join('\n')
}
