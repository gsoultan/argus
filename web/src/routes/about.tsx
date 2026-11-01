import { Anchor, Badge, Card, Group, Stack, Text } from '@mantine/core'
import { createFileRoute } from '@tanstack/react-router'
import { PageBody, PageHeader } from '~/components/page'
import { LogoMark } from '~/components/Logo'
import { argusVersion, controlPlaneHost, isConfigured } from '~/lib/live'
import { FS, SP } from '~/theme'

const COPYRIGHT_HOLDER = 'Gembit Soultan Shirazi'
const COPYRIGHT_EMAIL = 'gembit.soultan@gmail.com'
const COPYRIGHT_YEAR = 2026

/**
 * What Argus is, which release is running, and who holds the copyright.
 *
 * Open to every role: anyone who can sign in may want to know which version
 * they are on when they report a problem. The version is the control plane's
 * own, stamped from the release tag when it was built (see `argusVersion`).
 */
export const Route = createFileRoute('/about')({
  // In the main bundle rather than a chunk of its own. Split, this one small
  // page made Rollup carve live.ts, react-dom and ScrollArea out of the entry
  // into shared chunks, and the first visit grew 1.4 kB past its budget
  // (e2e/weight.spec.ts). Inline it costs 0.7 kB.
  codeSplitGroupings: [],
  component: AboutPage,
})

function AboutPage() {
  const version = argusVersion()
  return (
    <>
      <PageHeader title="About Argus" description="What this is and which release is running." />
      <PageBody>
        <Card padding="lg" maw={720}>
          <Stack gap="md">
            <Group gap={SP.cozy} wrap="nowrap">
              <LogoMark size={40} />
              <Stack gap={2}>
                <Text fw={700} size="lg">Argus</Text>
                <Group gap={SP.tight}>
                  <Text size={FS.meta} c="dimmed">Version</Text>
                  <Badge variant="default" color="slate" className="argus-digest" data-testid="argus-version">
                    {version}
                  </Badge>
                </Group>
              </Stack>
            </Group>

            <Text size={FS.body} lh={1.6}>
              Argus is privileged access management that knows who reached which server, as
              whom, and what they did there, and can prove it later. Every session opens from
              this console and runs through an agent on the server, which records it there and
              streams the record back as it happens. The audit trail is a hash chain, so
              tampering shows.
            </Text>

            {isConfigured() && (
              <Text size={FS.meta} c="dimmed">
                Served by the control plane at {controlPlaneHost()}.
              </Text>
            )}

            <Text size={FS.meta} c="dimmed">
              Copyright © {COPYRIGHT_YEAR} {COPYRIGHT_HOLDER}{' '}
              &lt;<Anchor href={`mailto:${COPYRIGHT_EMAIL}`} size={FS.meta}>{COPYRIGHT_EMAIL}</Anchor>&gt;.
              All rights reserved.
            </Text>
          </Stack>
        </Card>
      </PageBody>
    </>
  )
}
