import { useState } from 'react'
import { Alert, Badge, Box, Button, Group, PasswordInput, ScrollArea, Select, Stack, Text, TextInput, UnstyledButton } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { Findings, VersionState } from '~/components/SoftwareLibrary'
import { run as notifyRun } from '~/lib/notify'
import type { ScriptParam, SoftwareScript } from '~/lib/softwareApi'
import { scriptsQuery, useRunScript } from '~/lib/softwareQueries'
import { FS, SP } from '~/theme'
import type { Asset } from '~/types/domain'

const PRODUCTION = ['production', 'prod', 'prd']

/** Whether Argus treats this server as production: the same tags the control plane checks. */
export function isProductionAsset(asset: Asset): boolean {
  return (asset.tags ?? []).some((t) => PRODUCTION.includes(t.toLowerCase()))
}

/**
 * Install from the library: a reviewed script instead of a repository
 * package. There is no simulation to show first: a script can do anything
 * its text says, so the review stands in for the preview, and the server
 * reports what changed (packages, services, ports, folders) afterwards.
 */
export function ScriptInstall({ asset, onClose }: { asset: Asset; onClose: () => void }) {
  const { data, error } = useQuery(scriptsQuery())
  const platform = asset.protocol === 'rdp' ? 'windows' : 'linux'
  const usable = (data ?? []).filter((s) => s.platform === platform && s.reviewed)
  const [picked, setPicked] = useState<SoftwareScript | null>(null)
  const [values, setValues] = useState<Record<string, string>>({})
  const go = useRunScript(asset.id)
  const v = picked?.reviewed
  const prod = isProductionAsset(asset)
  const untried = Boolean(v && !v.triedOn)
  const missing = (v?.params ?? []).filter((p) => p.required && !(values[p.name] ?? p.default ?? '').trim())
  const refused = prod && untried

  return (
    <>
      <Group align="stretch" gap={0} wrap="nowrap" style={{ minHeight: 480 }}>
        <Box w={300} style={{ borderRight: '1px solid var(--mantine-color-default-border)' }}>
          <ScrollArea h={540} type="auto">
            {error ? <Text size={FS.meta} c="rose" p="sm">{error instanceof Error ? error.message : String(error)}</Text>
              : usable.length === 0 ? (
                <Text size={FS.meta} c="dimmed" p="sm">
                  No reviewed Linux scripts yet. Add one in Settings › Software; it can run once someone other than its author reviews it.
                </Text>
              ) : usable.map((s) => (
                <UnstyledButton key={s.id} w="100%" px="sm" py={SP.cozy}
                  onClick={() => { setPicked(s); setValues({}) }}
                  bg={picked?.id === s.id ? 'var(--mantine-color-default-hover)' : undefined}
                  style={{ borderBottom: '1px solid var(--mantine-color-default-border)' }}>
                  <Text size="sm" fw={600}>{s.name}</Text>
                  {s.description && <Text size={FS.micro} c="dimmed" lineClamp={1}>{s.description}</Text>}
                  <Text size={FS.micro} c="dimmed">v{s.reviewed!.version}{s.reviewed!.triedOn ? ` · ran on ${s.reviewed!.triedOn}` : ' · not tried yet'}</Text>
                </UnstyledButton>
              ))}
          </ScrollArea>
        </Box>
        <Box p="md" style={{ flex: 1, minWidth: 0 }}>
          {!picked || !v ? (
            <Text size={FS.body} c="dimmed">Pick a script. Only versions that passed the checks and were reviewed are listed.</Text>
          ) : (
            <Stack gap="sm">
              <Group gap={SP.snug}><Text fw={600}>{picked.name}</Text><VersionState v={v} /></Group>
              <Text size={FS.micro} c="dimmed">
                Runs as {v.runAs || 'root'}, stopped after {Math.round(v.timeoutSeconds / 60)} minutes. The server runs only
                the reviewed text (install {v.installSha.slice(0, 12)}) and refuses anything else.
              </Text>
              {v.params.map((p) => (
                <ParamInput key={p.name} p={p} value={values[p.name]}
                  onChange={(val) => setValues((x) => ({ ...x, [p.name]: val }))} />
              ))}
              <Alert color="slate" p="xs">
                <Text size={FS.body}>No preview for a script. When it ends, this server reports what changed: packages,
                  services, listening ports and folders under /opt and /usr/local.{v.check ? ' Then the check script confirms it is installed.' : ''}</Text>
              </Alert>
              {refused && (
                <Alert color="amber" p="xs">
                  <Text size={FS.body}>{asset.hostname} is production. v{v.version} runs here once it has finished successfully on a server that isn't.</Text>
                </Alert>
              )}
              {v.findings.some((f) => f.level !== 'note') && <Findings findings={v.findings.filter((f) => f.level !== 'note')} />}
            </Stack>
          )}
        </Box>
      </Group>
      <Group justify="space-between" p="md" style={{ borderTop: '1px solid var(--mantine-color-default-border)' }} wrap="wrap">
        <Text size={FS.micro} c="dimmed">Secret values are sealed, hidden in the log, and deleted when the run ends.</Text>
        <Group gap="sm">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button disabled={!v || refused || missing.length > 0} loading={go.isPending}
            onClick={() => v && void notifyRun(() => go.mutateAsync({ versionId: v.id, action: 'install', params: clean(v.params, values) }), {
              failure: 'Not started',
              success: ['Script asked for', 'The server starts it at its next check-in, or after approval if updates need one.'],
            }).then((ok) => ok && onClose())}>
            Run install
          </Button>
        </Group>
      </Group>
    </>
  )
}

export function clean(params: ScriptParam[], values: Record<string, string>): Record<string, string> {
  const out: Record<string, string> = {}
  for (const p of params) {
    const val = values[p.name]
    if (val !== undefined && val !== '') out[p.name] = val
  }
  return out
}

export function ParamInput({ p, value, onChange }: { p: ScriptParam; value?: string; onChange: (v: string) => void }) {
  const label = <>{p.label || p.name} <Badge size="xs" variant="light" color="slate" ml={4}>{p.name}</Badge></>
  if (p.kind === 'choice') {
    return <Select size="xs" w={320} label={label} required={p.required} value={value ?? p.default ?? null}
      data={p.choices ?? []} onChange={(v) => onChange(v ?? '')} />
  }
  if (p.kind === 'secret') {
    return <PasswordInput size="xs" w={320} label={label} required={p.required} value={value ?? ''}
      onChange={(e) => onChange(e.currentTarget.value)} autoComplete="off" />
  }
  return <TextInput size="xs" w={320} label={label} required={p.required} placeholder={p.default}
    value={value ?? ''} onChange={(e) => onChange(e.currentTarget.value)} />
}
