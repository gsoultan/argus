import type * as React from 'react'
import type * as JSXRuntime from 'react/jsx-runtime'
import type * as Query from '@tanstack/react-query'
import type * as Page from '~/components/page'
import type * as Primitives from '~/components/primitives'
import type * as Notify from '~/lib/notify'
import type * as Theme from '~/theme'
import type * as Live from '~/lib/live'
import type { LentMantine } from './mantine'

/*
 * The contract between the console (the shell) and a section that ships its
 * own screens, such as argus-secret's. A section is an ES module the shell
 * imports from /mf/<section>/<contract>/index.js, so it is served by the same
 * origin under the console's own Content-Security-Policy.
 *
 * It exports `contract` (the version of this file it was built against) and
 * `mount(element, context)`, which returns the function that unmounts it.
 *
 * React, Mantine, TanStack Query and the console's own page shapes are not in
 * the section's bundle: it is built with those imports pointed at the shell's
 * copies (src/mf/shared), which the shell puts on `globalThis.__argusShell`
 * before importing it. So they load once, and the section looks and behaves
 * like every other page because it is drawn with the same components.
 */

/** The contract this console speaks. A section built for another is refused. */
export const SHELL_CONTRACT = 1

/** What the shell lends a section's bundle: its own modules. */
export interface ShellModules {
  contract: number
  React: typeof React
  jsx: typeof JSXRuntime
  mantine: LentMantine
  query: typeof Query
  page: typeof Page
  primitives: Pick<typeof Primitives, 'Mono' | 'relTime' | 'absTime'>
  notify: typeof Notify
  theme: Pick<typeof Theme, 'FS' | 'SP' | 'INK'>
  live: Pick<typeof Live, 'get' | 'send'>
}

/** The signed-in person, as a section needs them. */
export interface SectionPerson {
  email: string
  name: string
  role: string
}

/** What a section is given when it is mounted. */
export interface SectionContext {
  contract: number
  person: SectionPerson
  /** The ?tab= the page is on, and a way to change it. */
  tab: string | undefined
  setTab: (tab: string | undefined) => void
  /** Opens a console page, such as /settings?section=secrets-servers. */
  navigate: (to: string) => void
  /** Draws React into the element, inside the console's providers. */
  render: (element: HTMLElement, node: React.ReactNode) => SectionRoot
}

/** Where a section draws: draw again with new props, or take it away. */
export interface SectionRoot {
  render: (node: React.ReactNode) => void
  unmount: () => void
}

/** A section's module, as the shell imports it. */
export interface SectionModule {
  contract: number
  mount: (element: HTMLElement, context: SectionContext) => () => void
  /** Called when the ?tab= changes, so the section draws again in place. */
  update?: (element: HTMLElement, context: SectionContext) => void
}

declare global {
  // eslint-disable-next-line no-var
  var __argusShell: ShellModules | undefined
}
