import { Badge, Box, Group, Radio, Stack, Text } from '@mantine/core'
import type { CloudBase } from '~/lib/cloud'
import { FS, SP } from '~/theme'

/**
 * Which of argus-control's identities a new account is connected as. One
 * that does not answer cannot be picked: its roles would trust a principal
 * Argus cannot be.
 */
export function IdentityPicker({ bases, value, onChange }: { bases: CloudBase[]; value: string; onChange: (v: string) => void }) {
  return (
    <Box>
      <Text size="sm" fw={500}>Argus connects as</Text>
      <Text size={FS.meta} c="dimmed" mb={6}>
        The roles in the account trust only this identity, together with an external id made for this organization and account.
        You can move the account to another one later.
      </Text>
      <Radio.Group value={value} onChange={onChange}>
        <Stack gap={SP.snug}>
          {bases.map((b) => (
            <Radio.Card key={b.identity} value={b.identity} radius="md" p="sm" disabled={!b.principal}>
              <Group wrap="nowrap" align="flex-start" gap="sm">
                <Radio.Indicator mt={2} disabled={!b.principal} />
                <Box style={{ minWidth: 0 }}>
                  <Group gap={6}>
                    <Text fw={600} size={FS.body}>{b.name}</Text>
                    {b.default && <Badge size="sm" variant="light" color="azure" tt="none">Default</Badge>}
                    {b.principal ? <Badge size="sm" variant="light" color="teal" tt="none">Working</Badge>
                      : <Badge size="sm" variant="light" color="rose" tt="none">No answer</Badge>}
                  </Group>
                  <Text size={FS.micro} c="dimmed" ff="monospace" style={{ overflowWrap: 'anywhere' }}>{b.principal ?? b.error}</Text>
                </Box>
              </Group>
            </Radio.Card>
          ))}
        </Stack>
      </Radio.Group>
    </Box>
  )
}
