import { useEffect, useRef, useState } from 'react'
import { Anchor, Box, Button, Code, CopyButton, Group, List, Loader, Stack, Text } from '@mantine/core'
import { IconCheck, IconChevronDown, IconChevronRight, IconCopy, IconExternalLink } from '@tabler/icons-react'
import { run } from '~/lib/notify'
import type { TelegramChat } from '~/lib/telegram'
import { FS, INK } from '~/theme'

/*
 * How to get what a Telegram bot needs, for the two forms that take one:
 * Settings › Notifications (an organization's bots) and Settings › Alert
 * channels (the platform bot). The token is the easy half; a group's chat ID
 * is the half people get stuck on, so it is spelled out, and the form can
 * watch for the group's first message and fill the ID in by itself.
 */

/** How long the form keeps looking for a group's message, and how often. */
export const WATCH_MS = 120_000
const WATCH_EVERY_MS = 3_000

function Toggle({ open, onClick, children }: { open: boolean; onClick: () => void; children: React.ReactNode }) {
  return (
    <Anchor component="button" type="button" size={FS.meta} onClick={onClick}>
      <Group gap={4} wrap="nowrap">
        {open ? <IconChevronDown size={13} /> : <IconChevronRight size={13} />}
        {children}
      </Group>
    </Anchor>
  )
}

function CopyCode({ value }: { value: string }) {
  return (
    <Group gap={6} wrap="nowrap" component="span" style={{ display: 'inline-flex', verticalAlign: 'middle' }}>
      <Code>{value}</Code>
      <CopyButton value={value} timeout={1500}>
        {({ copied, copy }) => (
          <Button size="compact-xs" variant="default" onClick={copy} aria-label={`Copy ${value}`}
            leftSection={copied ? <IconCheck size={12} /> : <IconCopy size={12} />}>
            {copied ? 'Copied' : 'Copy'}
          </Button>
        )}
      </CopyButton>
    </Group>
  )
}

const ext = <IconExternalLink size={12} style={{ display: 'inline-block', verticalAlign: 'middle' }} />

/** Making a bot with @BotFather, folded under the token field. */
export function TokenHelp({ initiallyOpen = false, check = false }: {
  initiallyOpen?: boolean
  /** The form has a Check button beside the token. */
  check?: boolean
}) {
  const [open, setOpen] = useState(initiallyOpen)
  return (
    <Box>
      <Toggle open={open} onClick={() => setOpen((v) => !v)}>How do I get a bot token?</Toggle>
      {open && (
        <List type="ordered" spacing={6} size="sm" mt={6} data-testid="telegram-token-help">
          <List.Item>
            In Telegram, open{' '}
            <Anchor href="https://t.me/BotFather" target="_blank" rel="noreferrer">@BotFather {ext}</Anchor>
            {' '}(the one with the blue tick) and press <b>Start</b>.
          </List.Item>
          <List.Item>Send <CopyCode value="/newbot" /></List.Item>
          <List.Item>
            Give it a name people will see, such as <i>Argus alerts</i>, then a username that ends in <Code>bot</Code>,
            such as <Code>acme_argus_bot</Code>.
          </List.Item>
          <List.Item>
            BotFather answers with a token that looks like <Code>123456789:AAH…</Code>. Copy all of it and paste it here{check ? <>, then press <b>Check</b></> : ''}.
          </List.Item>
          <Text size={FS.meta} c="dimmed" mt={4}>
            Anyone with the token can post as the bot. If it leaks, send <Code>/revoke</Code> to BotFather and paste the new one.
          </Text>
        </List>
      )}
    </Box>
  )
}

/**
 * Putting the bot in a group and getting the group's chat ID. Open by default
 * until the form has a chat, since that is where people get stuck.
 */
export function ChatHelp({ username, initiallyOpen, findLabel }: {
  /** The bot's @username once the token is checked; a placeholder before. */
  username?: string
  initiallyOpen: boolean
  /** The button the steps tell people to press. */
  findLabel: string
}) {
  const [open, setOpen] = useState(initiallyOpen)
  const [more, setMore] = useState(false)
  const name = username ? username.replace(/^@/, '') : ''
  const at = name ? `@${name}` : 'your bot'
  const start = name ? `/start@${name}` : '/start@your_bot'
  return (
    <Box>
      <Toggle open={open} onClick={() => setOpen((v) => !v)}>How do I get a group's chat ID?</Toggle>
      {open && (
        <Stack gap={6} mt={6} data-testid="telegram-chat-help">
          <List type="ordered" spacing={6} size="sm">
            <List.Item>
              In Telegram, open the group, tap its name, then <b>Add members</b>. Search for <b>{at}</b> and add it.
            </List.Item>
            <List.Item>
              In the group, send <CopyCode value={start} />
              <Text size={FS.meta} c="dimmed" lh={1.4} mt={2}>
                Bots in groups only see messages meant for them, so a plain "hello" may never reach Argus. This one always does.
              </Text>
            </List.Item>
            <List.Item>
              Press <b>{findLabel}</b>. Argus asks Telegram which groups the bot heard from and ticks yours as soon as
              your message arrives. You never have to copy the ID yourself.
            </List.Item>
          </List>
          <Text size={FS.meta} c="dimmed">
            A group's chat ID is a negative number: <Code>-123456789</Code>, or <Code>-1001234567890</Code> for a
            supergroup or channel. If you type one, keep the minus sign, and the <Code>-100</Code> when there is one.
          </Text>
          <Toggle open={more} onClick={() => setMore((v) => !v)}>Channels, private chats, and when it is not found</Toggle>
          {more && (
            <List spacing={6} size="sm" data-testid="telegram-chat-help-more">
              <List.Item>
                <b>A channel:</b> add {at} as an administrator that may post messages, post anything in the channel,
                then press {findLabel}.
              </List.Item>
              <List.Item>
                <b>Just you:</b> open {at} in Telegram, press <b>Start</b>, then press {findLabel}. A person's chat ID is a
                positive number.
              </List.Item>
              <List.Item>
                <b>Not found:</b> Telegram only keeps a bot's messages for 24 hours, so send {start} again. If another
                program already uses this bot (it has a webhook), Telegram will not tell Argus about its chats: make a
                new bot for Argus, or type the ID.
              </List.Item>
              <List.Item>
                <b>It worked once and now fails:</b> a group that is upgraded to a supergroup (for example by making it
                public or turning on topics) gets a new ID starting with <Code>-100</Code>. Send {start} again and pick the
                new one.
              </List.Item>
              <List.Item>
                <b>By hand:</b> after sending {start}, open{' '}
                <Code style={{ overflowWrap: 'anywhere' }}>https://api.telegram.org/bot&lt;token&gt;/getUpdates</Code>{' '}
                in a browser, with your token in place of &lt;token&gt;, and look for <Code>"chat":{'{'}"id":-100…</Code>.
              </List.Item>
            </List>
          )}
        </Stack>
      )}
    </Box>
  )
}

/**
 * Find chats, and when nothing new turns up, keep asking for a while so the
 * person can go and send the message: the group is ticked the moment it
 * arrives. `onFound` gets the chats from the first look (`fresh` false) and
 * then, while watching, only ones not seen before (`fresh` true).
 */
export function useChatFinder(lookup: () => Promise<TelegramChat[]>, known: string[],
  onFound: (chats: TelegramChat[], fresh: boolean) => void,
  /** What lookup asks with (the token or bot): a change stops a watch, so another bot's groups are never ticked. */
  key: string) {
  const [busy, setBusy] = useState(false)
  const [watching, setWatching] = useState(false)
  const [missed, setMissed] = useState(false)
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null)
  // Every find, stop, key change and unmount starts a new generation; a
  // lookup that answers for an older one is ignored, so a slow answer can
  // never start a second loop or act on a closed form.
  const gen = useRef(0)
  const until = useRef(0)
  const seen = useRef<Set<string>>(new Set())
  const latest = useRef({ lookup, onFound })
  latest.current = { lookup, onFound }

  const stop = () => {
    gen.current++
    if (timer.current) clearTimeout(timer.current)
    timer.current = null
    setWatching(false)
    setBusy(false)
  }
  useEffect(() => () => {
    gen.current++
    if (timer.current) clearTimeout(timer.current)
  }, [])
  const lastKey = useRef(key)
  useEffect(() => {
    if (lastKey.current === key) return
    lastKey.current = key
    stop()
    setMissed(false)
  }, [key])

  const tick = (g: number) => {
    timer.current = setTimeout(() => {
      timer.current = null
      const again = () => {
        if (g !== gen.current) return
        if (Date.now() >= until.current) { stop(); setMissed(true); return }
        tick(g)
      }
      latest.current.lookup().then((chats) => {
        if (g !== gen.current) return
        const fresh = chats.filter((c) => !seen.current.has(c.id))
        if (fresh.length > 0) {
          for (const c of fresh) seen.current.add(c.id)
          stop()
          latest.current.onFound(fresh, true)
          return
        }
        again()
      }, again) // a failed look while watching is retried until the time is up
    }, WATCH_EVERY_MS)
  }

  const find = () => {
    stop()
    const g = gen.current
    setMissed(false)
    setBusy(true)
    void run(async () => {
      const chats = await latest.current.lookup()
      if (g !== gen.current) return
      const before = new Set(known)
      seen.current = new Set([...known, ...chats.map((c) => c.id)])
      latest.current.onFound(chats, false)
      if (!chats.some((c) => !before.has(c.id))) {
        until.current = Date.now() + WATCH_MS
        setWatching(true)
        tick(g)
      }
    }, { failure: 'Chats not found' }).finally(() => { if (g === gen.current) setBusy(false) })
  }
  return { find, stop, busy, watching, missed }
}

/** What the finder is doing, under its button. */
export function FinderStatus({ watching, missed, username, onStop }: {
  watching: boolean; missed: boolean; username?: string; onStop: () => void
}) {
  const start = username ? `/start@${username.replace(/^@/, '')}` : '/start@your_bot'
  if (watching) {
    return (
      <Group gap={8} wrap="nowrap" data-testid="telegram-watching">
        <Loader size={14} />
        <Text size={FS.meta} c={INK.live} style={{ flex: 1 }}>
          Watching for a new chat for 2 minutes. Send <Code>{start}</Code> in your group now; it is ticked here when it arrives.
        </Text>
        <Button size="compact-xs" variant="subtle" onClick={onStop}>Stop</Button>
      </Group>
    )
  }
  if (missed) {
    return (
      <Text size={FS.meta} c={INK.pending} data-testid="telegram-missed">
        No new chat arrived. Check that the bot is in the group and that you sent <Code>{start}</Code> there (not a plain
        message), then press it again. Or type the chat ID.
      </Text>
    )
  }
  return null
}
