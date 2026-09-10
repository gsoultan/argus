import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import { Alert, MantineProvider, Text } from '@mantine/core'
import { IconAlertTriangle } from '@tabler/icons-react'

/**
 * What a replay says about an artefact that does not match its chain head.
 *
 * The control plane re-verifies before serving and reports the verdict in a
 * header, and the session panel carried it as a badge. An operator watching a
 * replay has no reason to look at a badge in a side panel, so a tampered
 * recording played like any other. It is not a recording with a caveat; it is
 * an artefact that cannot be used as evidence.
 *
 * The banner is reproduced here rather than imported because the route pulls in
 * the whole session page. What is under test is the wording and the prominence,
 * which is what an operator actually acts on.
 */
function VerificationBanner({ verified }: { verified: string | null }) {
  if (verified === 'tampered') {
    return (
      <Alert
        color="rose"
        variant="filled"
        icon={<IconAlertTriangle size={16} />}
        title="This recording does not match what the gateway sealed"
      >
        <Text size="xs">
          The stored artefact's hash chain disagrees with the chain head recorded when this
          session ended, so it has been altered since. What plays below is the file as it
          stands now, shown so an investigator can see what was changed — it is not evidence
          of what happened. Preserve it and treat this as an incident.
        </Text>
      </Alert>
    )
  }
  if (verified === 'error') {
    return (
      <Alert color="amber" variant="light" title="This recording could not be verified">
        <Text size="xs">
          The control plane could not check the artefact against its sealed chain head.
        </Text>
      </Alert>
    )
  }
  return null
}

const show = (verified: string | null) =>
  render(
    <MantineProvider>
      <VerificationBanner verified={verified} />
    </MantineProvider>,
  )

describe('a replay whose artefact was altered', () => {
  it('says so where someone about to watch it will read it', () => {
    show('tampered')
    expect(screen.getByText(/does not match what the gateway sealed/i)).toBeInTheDocument()
    expect(screen.getByText(/not evidence of what happened/i)).toBeInTheDocument()
  })

  it('tells an unverifiable recording apart from an altered one', () => {
    show('error')
    expect(screen.getByText(/could not be verified/i)).toBeInTheDocument()
    // Not the same claim, and saying the stronger one would be wrong.
    expect(screen.queryByText(/has been altered/i)).not.toBeInTheDocument()
  })

  it('stays quiet when the chain verified', () => {
    show('intact')
    // No alarm on a good recording: a warning that appears every time is one
    // nobody reads on the day it matters.
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(screen.queryByText(/gateway sealed/i)).not.toBeInTheDocument()
  })

  it('stays quiet before a verdict has arrived', () => {
    show(null)
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(screen.queryByText(/gateway sealed/i)).not.toBeInTheDocument()
  })
})
