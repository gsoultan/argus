// The console's own modules, as a section's bundle reaches them. Read when the
// section's module is evaluated: the shell sets globalThis.__argusShell first.
import type { ShellModules } from '~/mf/contract'

const s = globalThis.__argusShell
if (!s) throw new Error('this section is loaded by the Argus console, not on its own')

export const shell: ShellModules = s
export const { PageHeader, PageBody, Toolbar, SectionCard, EmptyState, DataTable, TableSkeleton } = s.page
export const { Mono, relTime, absTime } = s.primitives
export const { notifyOk, notifyWarn, notifyError, run } = s.notify
export const { FS, SP, INK } = s.theme
export const { get, send } = s.live
