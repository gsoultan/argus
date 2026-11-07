import { useMemo, useState } from 'react'
import { Alert, Badge, Button, Group, Modal, MultiSelect, SegmentedControl, Stack, Text } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { clean, isProductionAsset, ParamInput } from '~/components/server/ScriptInstall'
import { run as notifyRun } from '~/lib/notify'
import { assetsQuery } from '~/lib/queries'
import type { BatchResult, SoftwareFile, SoftwareScript } from '~/lib/softwareApi'
import { useSoftwareBatch } from '~/lib/softwareQueries'
import { FS, SP } from '~/theme'
import type { Asset } from '~/types/domain'

export type BatchTarget = { kind: 'script'; script: SoftwareScript } | { kind: 'file'; file: SoftwareFile }

/** Which servers a library item can go on, by what it is for. */
export function fitsBatch(t: BatchTarget, a: Asset): boolean {
  const platform = t.kind === 'script' ? t.script.platform : t.file.platform
  if (a.kind === 'android') return platform === 'android'
  if (platform === 'windows') return a.protocol === 'rdp'
  if (platform === 'linux') return (a.protocol ?? 'ssh') === 'ssh'
  return false
}

/** The order a batch goes in: outside production first, as the control plane does. */
export function batchOrder(assets: Asset[]): Asset[] {
  return [...assets].sort((x, y) => Number(isProductionAsset(x)) - Number(isProductionAsset(y)))
}

/**
 * One library item on many servers. One server at a time, outside
 * production first, and a failure stops the servers after it; the ones that
 * cannot take it are named and left out.
 */
export function BatchInstall({ target, onClose }: { target: BatchTarget; onClose: () => void }) {
  const { data: assets } = useQuery(assetsQuery())
  const start = useSoftwareBatch()
  const [ids, setIds] = useState<string[]>([])
  const [action, setAction] = useState<'install' | 'uninstall'>('install')
  const [values, setValues] = useState<Record<string, string>>({})
  const [result, setResult] = useState<BatchResult | null>(null)
  const v = target.kind === 'script' ? target.script.reviewed : undefined
  const name = target.kind === 'script' ? `${target.script.name} v${v?.version ?? ''}` : target.file.name
  const options = useMemo(() => (assets ?? []).filter((a) => fitsBatch(target, a)), [assets, target])
  const picked = batchOrder(options.filter((a) => ids.includes(a.id)))
  const missing = (v?.params ?? []).filter((p) => p.required && !(values[p.name] ?? p.default ?? '').trim())
  const prodOnly = picked.length > 0 && picked.every(isProductionAsset)
  const untried = target.kind === 'script' ? !v?.triedOn : false

  const send = () => void notifyRun(async () => {
    setResult(await start.mutateAsync(target.kind === 'script'
      ? { kind: 'script', versionId: v!.id, action, params: clean(v!.params, values), assetIds: picked.map((a) => a.id) }
      : { kind: 'file', fileId: target.file.id, assetIds: picked.map((a) => a.id) }))
  }, { failure: 'Not started' })

  return (
    <Modal opened onClose={onClose} size="lg" title={`Put ${name} on several ${target.kind === 'file' && target.file.platform === 'android' ? 'devices' : 'servers'}`}>
      {result ? (
        <Stack gap={SP.cozy}>
          <Alert color="teal" title={`${result.runs.length} asked for, one at a time`}>
            <Text size={FS.body}>
              {result.runs.map((r, i) => `${i + 1}. ${r.assetHostname}`).join('  ')}
            </Text>
            <Text size={FS.meta} c="dimmed" mt={4}>
              Each starts when the one before it has finished; a failure stops the rest. Follow them in Activity or on each server's Apps tab.
            </Text>
          </Alert>
          {result.skipped.length > 0 && (
            <Alert color="amber" title="Left out">
              {result.skipped.map((s) => <Text key={s.assetId} size={FS.body}><b>{s.hostname || s.assetId}</b>: {s.reason}</Text>)}
            </Alert>
          )}
          <Group justify="flex-end"><Button onClick={onClose}>Done</Button></Group>
        </Stack>
      ) : (
        <Stack gap={SP.cozy}>
          {target.kind === 'script' && v?.uninstall && (
            <SegmentedControl size="xs" value={action} onChange={(x) => setAction(x as 'install' | 'uninstall')}
              data={[{ value: 'install', label: 'Install' }, { value: 'uninstall', label: 'Uninstall' }]} />
          )}
          <MultiSelect label="Servers" placeholder={options.length ? 'Pick servers' : 'No server can take this'} searchable
            data={options.map((a) => ({ value: a.id, label: `${a.hostname}${isProductionAsset(a) ? ' (production)' : ''}` }))}
            value={ids} onChange={setIds} maxValues={100} />
          {v?.params.map((p) => (
            <ParamInput key={p.name} p={p} value={values[p.name]} onChange={(val) => setValues((x) => ({ ...x, [p.name]: val }))} />
          ))}
          {picked.length > 1 && (
            <Text size={FS.meta}>
              In this order: {picked.map((a, i) => (
                <Badge key={a.id} size="sm" variant="light" color={isProductionAsset(a) ? 'amber' : 'slate'} mr={4}>{i + 1}. {a.hostname}</Badge>
              ))}
            </Text>
          )}
          {prodOnly && untried && (
            <Alert color="amber" p="xs">
              <Text size={FS.body}>Every server picked is production, and this has not worked outside production yet. Add a test server; it goes first.</Text>
            </Alert>
          )}
          <Text size={FS.meta} c="dimmed">
            One at a time, servers outside production first. A failure stops the servers after it, so a bad change breaks one server, not all of them.
            Where the update rule needs approval, each waits for another admin.
          </Text>
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <Button disabled={picked.length === 0 || missing.length > 0 || (prodOnly && untried)} loading={start.isPending} onClick={send}>
              {action === 'uninstall' ? 'Uninstall' : 'Install'} on {picked.length || ''} {picked.length === 1 ? 'server' : 'servers'}
            </Button>
          </Group>
        </Stack>
      )}
    </Modal>
  )
}
