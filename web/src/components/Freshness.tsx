import { Text, Tooltip } from '@mantine/core'
import { useEffect, useState } from 'react'
import { FS } from '~/theme'
import './freshness.css'

/**
 * How fresh a live page's figures are: "Live · updated 8 s ago", counting
 * up, and amber with "Not updated for 2 min" once a read is overdue (the
 * page is reading every `every` ms, so twice that means reads are failing).
 * `at` is the query's dataUpdatedAt (wall clock, so it is compared with
 * Date.now(), not the sample data's fixed clock); 0 means not read yet.
 */
export function Freshness({ at, every, what }: { at: number; every: number; what: string }) {
  const [t, setT] = useState(() => Date.now())
  useEffect(() => {
    const id = window.setInterval(() => setT(Date.now()), 5_000)
    return () => window.clearInterval(id)
  }, [])
  if (!at) return null
  const age = Math.max(0, t - at)
  const stale = age > 2 * every + 5_000
  const words = age < 10_000 ? 'just now' : age < 60_000 ? `${Math.round(age / 10_000) * 10} s ago` : `${Math.round(age / 60_000)} min ago`
  return (
    <Tooltip label={`${what} Read again every ${Math.round(every / 1000)} seconds while this page is open.`} multiline w={260} withArrow>
      <span className="argus-fresh" data-stale={stale || undefined} data-testid="freshness" aria-live="off">
        <span className="argus-fresh-dot" aria-hidden />
        <Text span size={FS.meta} fw={550}>{stale ? `Not updated for ${words.replace(' ago', '')}` : `Live · updated ${words}`}</Text>
      </span>
    </Tooltip>
  )
}
