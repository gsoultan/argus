import { useState } from 'react'
import { ActionIcon, Button, Group, Tooltip } from '@mantine/core'
import { IconChevronDown, IconPlugConnected } from '@tabler/icons-react'
import { lastConnect, useSessions, type Way } from '~/lib/sessions'
import type { Asset } from '~/types/domain'

const LABEL: Record<Way, string> = {
  terminal: 'Terminal', files: 'Files', split: 'Terminal and files', desktop: 'Desktop', screen: 'Screen', device: 'Screen',
}

/**
 * Connect, for one asset.
 *
 * Opens the Connect drawer. Once someone has ticked "open this way with one
 * click", it becomes a split button: the main part opens their last way and
 * account at once (still through a fresh ticket, which is the permission),
 * and the arrow opens the drawer. A refusal falls back to the drawer, which
 * says why and offers the next step.
 */
export function ConnectButton({ asset, size = 'sm' }: { asset: Asset; size?: 'sm' | 'md' }) {
  const { ask, open } = useSessions()
  const [busy, setBusy] = useState(false)
  const pref = lastConnect(asset.hostname)
  const quick = pref?.oneClick && pref.way !== 'desktop' && asset.principals.includes(pref.principal)
    ? pref : undefined
  const disabled = asset.principals.length === 0

  if (!quick) {
    return (
      <Button size={size} leftSection={<IconPlugConnected size={18} />} disabled={disabled}
        onClick={() => ask(asset.hostname)}>
        Connect
      </Button>
    )
  }
  return (
    <Group gap={0} wrap="nowrap" className="argus-splitbtn">
      <Button size={size} loading={busy} disabled={disabled} leftSection={<IconPlugConnected size={18} />}
        data-split aria-label={`Connect to ${asset.hostname}: ${LABEL[quick.way]} as ${quick.principal}`}
        onClick={async () => {
          setBusy(true)
          const res = await open({ target: asset.hostname, principal: quick.principal, way: quick.way })
          setBusy(false)
          if (res.error) ask(asset.hostname)
        }}>
        {LABEL[quick.way]} as {quick.principal}
      </Button>
      <Tooltip label="Other ways to connect">
        <ActionIcon size={size === 'md' ? 42 : 36} variant="filled" className="argus-splitbtn-more"
          aria-label={`Other ways onto ${asset.hostname}`} onClick={() => ask(asset.hostname)}>
          <IconChevronDown size={16} />
        </ActionIcon>
      </Tooltip>
    </Group>
  )
}
