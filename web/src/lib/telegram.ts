import { queryOptions, useMutation, useQueryClient } from '@tanstack/react-query'
import { get, send } from '~/lib/live'

/*
 * An organization's own Telegram bots, and which bot each kind of alert
 * uses (internal/control/telegram_bots.go). Admins and owners only. A
 * token goes up once and never comes back: the console sees its last three
 * characters.
 */

export interface TelegramChat {
  id: string
  title: string
  /** private, group, supergroup or channel, when Telegram said. */
  kind?: string
}

export interface TelegramBot {
  id: string
  name: string
  username: string
  tokenTail: string
  chats: TelegramChat[]
  default: boolean
  lastOkAt?: string
  lastError?: string
  lastErrorAt?: string
  createdBy: string
  createdAt: string
  updatedAt: string
  /** What sends through it, in words. */
  usedBy: string[]
}

export type RouteKind = 'signin' | 'server' | 'website' | 'backup'

/** '' is the default bot, 'off' no Telegram, 'platform' the config file's bot, else a bot's id. */
export interface TelegramRoute {
  kind: RouteKind
  bot: string
  chats: string[]
  updatedBy?: string
  updatedAt?: string
}

export interface TelegramView {
  bots: TelegramBot[]
  routes: TelegramRoute[]
  /** The config file's bot, when this organization may use it. */
  platform?: { chats: TelegramChat[]; usedBy: string[] }
  canSave: boolean
  cannotSave?: string
}

export interface TelegramBotInput {
  name: string
  /** Empty on a change keeps the saved token. */
  token: string
  chats: TelegramChat[]
  default: boolean
}

export const ROUTE_LABEL: Record<RouteKind, string> = {
  signin: 'Sign-in alerts',
  server: 'Server connected',
  website: 'Websites',
  backup: 'Backups',
}

export const ROUTE_HELP: Record<RouteKind, string> = {
  signin: 'A new device or place for an administrator, and sign-ins reported as not theirs.',
  server: "A server's agent connects for the first time.",
  website: 'A website goes down or comes back. A website can choose its own bot.',
  backup: 'A backup failed or is overdue. The platform bot already gets these.',
}

const key = ['telegram'] as const

export const telegramQuery = () =>
  queryOptions({ queryKey: key, queryFn: () => get<TelegramView>('/api/v1/telegram') })

function useTelegramMutation<A, R>(fn: (a: A) => Promise<R>) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: fn,
    onSettled: () => {
      void qc.invalidateQueries({ queryKey: key })
      void qc.invalidateQueries({ queryKey: ['websites'] })
    },
  })
}

const enc = encodeURIComponent

export const useAddBot = () =>
  useTelegramMutation((in_: TelegramBotInput) => send<TelegramBot>('POST', '/api/v1/telegram/bots', in_))

export const useChangeBot = () =>
  useTelegramMutation(({ id, input }: { id: string; input: TelegramBotInput }) =>
    send<TelegramBot>('PUT', `/api/v1/telegram/bots/${enc(id)}`, input))

export const useRemoveBot = () =>
  useTelegramMutation(({ id, moveTo }: { id: string; moveTo: string }) =>
    send<void>('DELETE', `/api/v1/telegram/bots/${enc(id)}?moveTo=${enc(moveTo)}`))

export const useSetRoute = () =>
  useTelegramMutation(({ kind, bot, chats }: { kind: RouteKind; bot: string; chats: string[] }) =>
    send<TelegramView>('PUT', `/api/v1/telegram/routes/${kind}`, { bot, chats }))

export interface Sent { bot: string; chats: string[] }

export const testBot = (id: string) => send<Sent>('POST', `/api/v1/telegram/bots/${enc(id)}/test`)
export const testRoute = (kind: RouteKind) => send<Sent>('POST', `/api/v1/telegram/routes/${kind}/test`)
export const checkToken = (token: string) =>
  send<{ username: string; name: string }>('POST', '/api/v1/telegram/check', { token })
export const findChats = (by: { token?: string; bot?: string }) =>
  send<{ chats: TelegramChat[] }>('POST', '/api/v1/telegram/chats', by)

/** A choice of bot, in words. */
export function botLabel(v: TelegramView | undefined, bot: string): string {
  if (bot === 'off') return 'Off'
  if (bot === 'platform') return 'Platform bot'
  if (bot === '') {
    const d = v?.bots.find((b) => b.default)
    if (d) return `Default bot (${d.name})`
    return v?.platform ? 'Default bot (Platform bot)' : 'Default bot (none yet)'
  }
  return v?.bots.find((b) => b.id === bot)?.name ?? 'A removed bot'
}

/** The chats a choice of bot can post to. */
export function chatsOf(v: TelegramView | undefined, bot: string): TelegramChat[] {
  if (!v || bot === 'off') return []
  if (bot === 'platform') return v.platform?.chats ?? []
  if (bot === '') {
    const d = v.bots.find((b) => b.default)
    return d ? d.chats : (v.platform?.chats ?? [])
  }
  return v.bots.find((b) => b.id === bot)?.chats ?? []
}

/** The bots an alert can pick, as Select options. noPlatform leaves the platform bot out. */
export function botOptions(v: TelegramView | undefined, opts: { noPlatform?: boolean; withOff?: boolean } = {}) {
  const out: { value: string; label: string; disabled?: boolean }[] = [{ value: '', label: botLabel(v, '') }]
  for (const b of v?.bots ?? []) {
    out.push({ value: b.id, label: b.lastError ? `${b.name} (last message failed)` : b.name })
  }
  if (v?.platform && !opts.noPlatform) out.push({ value: 'platform', label: 'Platform bot' })
  if (opts.withOff !== false) out.push({ value: 'off', label: 'Off: no Telegram' })
  return out
}

/** A chat for a MultiSelect: its title, and its ID when that differs. */
export const chatOption = (c: TelegramChat) => ({ value: c.id, label: c.title && c.title !== c.id ? c.title : c.id })
