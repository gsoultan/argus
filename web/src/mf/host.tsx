import {
  Children, Component, Fragment, PureComponent, StrictMode, Suspense, cloneElement, createContext, createElement, forwardRef, isValidElement, lazy, memo, startTransition, use, useCallback, useContext, useDebugValue, useDeferredValue, useEffect, useId, useImperativeHandle, useInsertionEffect, useLayoutEffect, useMemo, useReducer, useRef, useState, useSyncExternalStore, useTransition, version, type ReactNode,
} from 'react'
import { Fragment as JSXFragment, jsx, jsxs } from 'react/jsx-runtime'
import { createRoot } from 'react-dom/client'
import { MantineProvider, localStorageColorSchemeManager } from '@mantine/core'
import { LENT_MANTINE } from './mantine'
import { QueryClientProvider, queryOptions, useMutation, useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query'
import { DataTable, EmptyState, PageBody, PageHeader, SectionCard, TableSkeleton, Toolbar } from '~/components/page'
import { Mono, absTime, relTime } from '~/components/primitives'
import { notifyError, notifyOk, notifyWarn, run } from '~/lib/notify'
import { get, send } from '~/lib/live'
import { COLOR_SCHEME_KEY, FS, INK, SP, cssVariablesResolver, theme } from '~/theme'
import { SHELL_CONTRACT, type SectionContext, type SectionModule, type ShellModules } from './contract'

/*
 * Loading a section that ships its own screens (see contract.ts), and drawing
 * it inside the console's providers. A section's failure stays in its slot:
 * the shell says what went wrong and where to look.
 */

function shellModules(): ShellModules {
  return {
    // Named, never whole modules: a namespace keeps every export of it in
    // the console's chunks, and the overview pays for them (e2e/weight.spec.ts).
    contract: SHELL_CONTRACT, mantine: LENT_MANTINE,
    React: { Children, Component, Fragment, PureComponent, StrictMode, Suspense, cloneElement, createContext, createElement, forwardRef, isValidElement, lazy, memo, startTransition, use, useCallback, useContext, useDebugValue, useDeferredValue, useEffect, useId, useImperativeHandle, useInsertionEffect, useLayoutEffect, useMemo, useReducer, useRef, useState, useSyncExternalStore, useTransition, version },
    jsx: { Fragment: JSXFragment, jsx, jsxs },
    query: { queryOptions, useMutation, useQuery, useQueryClient },
    page: { PageHeader, PageBody, Toolbar, SectionCard, EmptyState, DataTable, TableSkeleton },
    primitives: { Mono, relTime, absTime }, notify: { notifyOk, notifyWarn, notifyError, run }, theme: { FS, SP, INK }, live: { get, send },
  }
}

/** Where each section's bundle is, by the contract this console speaks. */
const SECTIONS = {
  secrets: () => import.meta.env.DEV
    ? import('../mf-secrets/index') as Promise<SectionModule>
    : import(/* @vite-ignore */ `/mf/secrets/${SHELL_CONTRACT}/index.js`) as Promise<SectionModule>,
} as const

export type SectionName = keyof typeof SECTIONS

const loaded = new Map<SectionName, Promise<SectionModule>>()

/** Imports a section once. Exported for tests. */
export function loadSection(name: SectionName): Promise<SectionModule> {
  let p = loaded.get(name)
  if (!p) {
    globalThis.__argusShell ??= shellModules()
    p = SECTIONS[name]().then((m) => {
      if (typeof m.mount !== 'function') throw new SectionError('broken', 'it does not export mount')
      if (m.contract !== SHELL_CONTRACT) {
        throw new SectionError('version', `it was built for console contract ${String(m.contract)}, and this console speaks ${SHELL_CONTRACT}`)
      }
      return m
    }).catch((err: unknown) => {
      loaded.delete(name)
      throw err instanceof SectionError ? err : new SectionError('missing', err instanceof Error ? err.message : String(err))
    })
    loaded.set(name, p)
  }
  return p
}

export class SectionError extends Error {
  readonly kind: 'missing' | 'version' | 'broken'
  constructor(kind: 'missing' | 'version' | 'broken', message: string) {
    super(message)
    this.kind = kind
  }
}

const colorSchemeManager = localStorageColorSchemeManager({ key: COLOR_SCHEME_KEY })

function providers(qc: QueryClient) {
  return (node: ReactNode) => (
    <StrictMode>
      <MantineProvider theme={theme} cssVariablesResolver={cssVariablesResolver} colorSchemeManager={colorSchemeManager} defaultColorScheme="auto">
        <QueryClientProvider client={qc}>{node}</QueryClientProvider>
      </MantineProvider>
    </StrictMode>
  )
}

/**
 * The slot a section draws in. `fallback` is what to show when it cannot be
 * loaded: the section is still there, with a card naming the problem.
 */
export function SectionSlot({ name, context, fallback }: {
  name: SectionName
  context: Omit<SectionContext, 'contract' | 'render'>
  fallback: (err: SectionError) => ReactNode
}) {
  const qc = useQueryClient()
  const el = useRef<HTMLDivElement>(null)
  const mod = useRef<SectionModule | null>(null)
  const [err, setErr] = useState<SectionError | null>(null)
  const ctx = useRef<SectionContext | null>(null)
  const wrap = providers(qc)
  ctx.current = {
    ...context, contract: SHELL_CONTRACT,
    render: (element, node) => {
      const root = createRoot(element)
      root.render(wrap(node))
      return { render: (n) => root.render(wrap(n)), unmount: () => root.unmount() }
    },
  }

  useEffect(() => {
    let unmount: (() => void) | null = null
    let gone = false
    loadSection(name).then((m) => {
      if (gone || !el.current || !ctx.current) return
      mod.current = m
      try {
        unmount = m.mount(el.current, ctx.current)
      } catch (e) {
        setErr(new SectionError('broken', e instanceof Error ? e.message : String(e)))
      }
    }, (e: unknown) => { if (!gone) setErr(e as SectionError) })
    return () => {
      gone = true
      // After React has finished with this tree, so the section's own root
      // is not unmounted in the middle of a render.
      const u = unmount
      if (u) queueMicrotask(u)
    }
  }, [name])

  // A new tab is drawn in place, keeping what the section holds.
  useEffect(() => {
    if (mod.current && el.current && ctx.current) mod.current.update?.(el.current, ctx.current)
  }, [context.tab])

  if (err) return <>{fallback(err)}</>
  return <div ref={el} data-section={name} />
}
