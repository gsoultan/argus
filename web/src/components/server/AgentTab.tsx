import { Badge, Card, Group, Stack, Text } from '@mantine/core'
import { AgentUpdateCard } from '~/components/AgentUpdate'
import { absTime, relTime } from '~/components/primitives'
import { SettingsSection, SettingsSections } from '~/components/server/layout'
import { DeviceSecurity, agentStateLabel } from '~/components/server/OverviewTab'
import { assetNoun, assetTypeOf } from '~/lib/assetType'
import { isConfigured } from '~/lib/live'
import { FS } from '~/theme'
import type { Asset } from '~/types/domain'

const TONE = { healthy: 'teal', stale: 'amber', absent: 'slate' } as const

/**
 * The Argus agent on this asset, or the Argus app on a device: whether it
 * is there, when it last checked in, and its version and updates. It was a
 * card at the bottom of Settings; everything a session here depends on
 * runs through it, so it has a tab of its own.
 */
export function AgentTab({ asset, canEdit }: { asset: Asset; canEdit: boolean }) {
  const device = assetTypeOf(asset) === 'device'
  const noun = assetNoun(asset)
  return (
    <SettingsSections>
      <SettingsSection
        title={device ? 'The app' : 'Agent'}
        description={device
          ? 'The Argus app on this device, which shares its screen and records every session on it.'
          : `The Argus agent on this ${noun}, which opens and records every session here.`}
      >
        <Card padding="md">
          <Stack gap="sm">
            <Row label="State">
              <Badge color={TONE[asset.agentState]} variant="light" size="sm" radius="sm" tt="none">
                {agentStateLabel(asset)}
              </Badge>
            </Row>
            <Row label="Checked in">
              <Text size={FS.meta} title={asset.agentLastSeenAt ? absTime(asset.agentLastSeenAt) : undefined}>
                {asset.agentLastSeenAt ? relTime(asset.agentLastSeenAt) : 'Never'}
              </Text>
            </Row>
            {asset.tunnelConnected !== undefined && asset.agentState !== 'absent' && (
              <Row label="Tunnel">
                <Text size={FS.meta}>
                  {asset.tunnelConnected ? 'Connected: sessions can open now' : `Not connected: nothing opens until the ${device ? 'app' : 'agent'} reconnects`}
                </Text>
              </Row>
            )}
            {device && <DeviceSecurity asset={asset} />}
          </Stack>
        </Card>
      </SettingsSection>

      {isConfigured() && (
        <SettingsSection
          title={device ? 'App version' : 'Version and updates'}
          description="Updates wait until nobody is connected, and roll back on their own if the new version does not come back."
        >
          <AgentUpdateCard asset={asset} canEdit={canEdit} bare />
        </SettingsSection>
      )}
    </SettingsSections>
  )
}

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <Group gap="sm" wrap="nowrap" align="flex-start">
      <Text size={FS.meta} c="dimmed" w={110} style={{ flexShrink: 0 }}>{label}</Text>
      {children}
    </Group>
  )
}
