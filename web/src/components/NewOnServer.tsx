import { Button, Modal, Select, Stack, Text } from '@mantine/core'
import { useMediaQuery } from '@mantine/hooks'
import { useQuery } from '@tanstack/react-query'
import { IconPlus } from '@tabler/icons-react'
import { lazy, Suspense, useState } from 'react'
import { assetsQuery } from '~/lib/queries'
import { FS } from '~/theme'
import type { DBEngine } from '~/types/domain'

const AddDatabase = lazy(() => import('~/components/DatabasesOnServer').then((m) => ({ default: m.AddDatabase })))

/**
 * New database (or broker) from the list pages: which server first, then the
 * same Create new dialog a server's page opens. The agent on that server does
 * the creating, so only servers with an agent are offered.
 */
export function NewOnServer({ label, engines }: { label: string; engines: DBEngine[] }) {
  const narrow = useMediaQuery('(max-width: 48em)')
  const [picking, setPicking] = useState(false)
  const [assetId, setAssetId] = useState<string | null>(null)
  const [open, setOpen] = useState(false)
  const { data: assets } = useQuery({ ...assetsQuery({}), enabled: picking || open })
  const withAgent = (assets ?? []).filter((a) => a.agentState !== 'absent')
  const asset = withAgent.find((a) => a.id === assetId)
  return (
    <>
      <Button size="sm" leftSection={<IconPlus size={16} />} onClick={() => setPicking(true)}>{label}</Button>
      <Modal opened={picking} onClose={() => setPicking(false)} title={label} centered={!narrow} fullScreen={narrow}>
        <Stack gap="sm">
          <Select label="On which server" searchable data={withAgent.map((a) => ({
            value: a.id, label: `${a.hostname}${a.agentState === 'stale' ? ' (agent not heard from lately)' : ''}`,
          }))} value={assetId} onChange={setAssetId} placeholder="Pick a server" data-autofocus />
          <Text size={FS.micro} c="dimmed">
            Only servers with the Argus agent are listed: it creates it there as the engine’s admin, so no password is typed in.
          </Text>
          <Button disabled={!asset} onClick={() => { setPicking(false); setOpen(true) }}>Next</Button>
        </Stack>
      </Modal>
      {asset && open && (
        <Suspense fallback={null}>
          <AddDatabase asset={asset} opened onClose={() => setOpen(false)} initialMode="create"
            initialEngine={engines[0]} onlyEngines={engines} />
        </Suspense>
      )}
    </>
  )
}
