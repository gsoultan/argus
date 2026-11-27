import { useState } from 'react'
import {
  Alert, Badge, Button, Checkbox, Group, Modal, MultiSelect, Select, Skeleton, Stack, Switch, Table,
  Tabs, Text, TextInput,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconBrandTelegram, IconCheck, IconLock, IconPlus } from '@tabler/icons-react'
import { SectionCard } from '~/components/page'
import { Mono } from '~/components/primitives'
import { isConfigured } from '~/lib/live'
import { notifyOk, run } from '~/lib/notify'
import {
  ROUTE_HELP, ROUTE_LABEL, botLabel, botOptions, chatOption, chatsOf, checkToken, findChats, telegramQuery, testBot,
  testRoute, useAddBot, useChangeBot, useRemoveBot, useSetRoute,
  type TelegramBot, type TelegramChat, type TelegramRoute, type TelegramView,
} from '~/lib/telegram'
import { ago } from '~/lib/websites'
import { FS, INK } from '~/theme'
import { ChatHelp, FinderStatus, TokenHelp, useChatFinder } from '~/components/TelegramHelp'

/**
 * Settings › Notifications: which bot and chats each kind of alert goes to,
 * first, and the organization's own bots behind them. Admins and owners only; the server says the same.
 */
export function TelegramBots() {
  const { data: v, isLoading, error } = useQuery({ ...telegramQuery(), enabled: isConfigured() })
  const [tab, setTab] = useState<string | null>('routes')
  const [editing, setEditing] = useState<TelegramBot | 'new' | null>(null)
  const [removing, setRemoving] = useState<TelegramBot | null>(null)

  if (!isConfigured()) {
    return (
      <Alert color="slate">
        <Text size={FS.body}>Telegram bots are kept on a control plane. Connect this console to Argus to add them.</Text>
      </Alert>
    )
  }
  if (error) return <Alert color="rose"><Text size={FS.body}>{error.message}</Text></Alert>
  if (isLoading || !v) return <Skeleton h={320} />

  return (
    <Stack gap="sm">
      {!v.canSave && (
        <Alert color="amber" icon={<IconLock size={16} />} title="Bots cannot be added here yet">
          <Text size={FS.body}>{v.cannotSave}</Text>
        </Alert>
      )}
      <Tabs value={tab} onChange={setTab}>
        <Tabs.List>
          <Tabs.Tab value="routes">Where alerts go</Tabs.Tab>
          <Tabs.Tab value="bots">Bots · {v.bots.length}</Tabs.Tab>
        </Tabs.List>
        <Tabs.Panel value="bots" pt="sm">
          <BotsList v={v} onAdd={() => setEditing('new')} onEdit={setEditing} onRemove={setRemoving} />
        </Tabs.Panel>
        <Tabs.Panel value="routes" pt="sm">
          <Routes v={v} />
        </Tabs.Panel>
      </Tabs>
      {editing && <BotForm v={v} bot={editing === 'new' ? null : editing} onClose={() => setEditing(null)} />}
      {removing && <RemoveBot v={v} bot={removing} onClose={() => setRemoving(null)} />}
    </Stack>
  )
}

/* ── Bots ────────────────────────────────────────────────────────────────── */

function BotsList({ v, onAdd, onEdit, onRemove }: {
  v: TelegramView; onAdd: () => void; onEdit: (b: TelegramBot) => void; onRemove: (b: TelegramBot) => void
}) {
  const [testing, setTesting] = useState<string | null>(null)
  const test = (b: TelegramBot) => {
    setTesting(b.id)
    void run(async () => {
      const r = await testBot(b.id)
      notifyOk('Sent', `${r.bot} posted a test message to ${r.chats.length} chat${r.chats.length === 1 ? '' : 's'}.`)
    }, { failure: 'Test not sent' }).finally(() => setTesting(null))
  }
  return (
    <Stack gap="sm">
      <SectionCard
        title="Telegram bots"
        icon={IconBrandTelegram}
        description="Bots that send this organization's alerts. Each kind of alert picks a bot and some of its chats under Where alerts go. A bot only ever posts to the chats listed on it."
        action={<Button size="xs" leftSection={<IconPlus size={14} />} disabled={!v.canSave} onClick={onAdd}>Add bot</Button>}
        flush
      >
        {v.bots.length === 0 ? (
          <Text size={FS.body} c="dimmed" p="md">
            No bots yet. {v.platform ? 'Alerts use the platform bot until you add one and make it the default.' : 'Until you add one, alerts reach the bell and email only.'}
          </Text>
        ) : (
          <Table.ScrollContainer minWidth={760}>
            <Table verticalSpacing="sm" horizontalSpacing="md">
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>Bot</Table.Th>
                  <Table.Th>Chats it may post to</Table.Th>
                  <Table.Th>Used by</Table.Th>
                  <Table.Th>Last message</Table.Th>
                  <Table.Th />
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {v.bots.map((b) => (
                  <Table.Tr key={b.id}>
                    <Table.Td>
                      <Group gap={6}>
                        <Text size={FS.body} fw={600}>{b.name}</Text>
                        {b.default && <Badge size="xs" variant="light">Default</Badge>}
                      </Group>
                      <Text size="xs" c="dimmed"><Mono>@{b.username || '?'} · token ••••{b.tokenTail}</Mono></Text>
                    </Table.Td>
                    <Table.Td>
                      <Group gap={4}>
                        {b.chats.length === 0
                          ? <Text size="xs" c="amber.8">No chats yet</Text>
                          : b.chats.map((c) => <Badge key={c.id} size="sm" variant="default" tt="none">{chatOption(c).label}</Badge>)}
                      </Group>
                    </Table.Td>
                    <Table.Td><Text size={FS.body}>{b.usedBy.length ? b.usedBy.join(', ') : 'Nothing'}</Text></Table.Td>
                    <Table.Td><LastMessage b={b} /></Table.Td>
                    <Table.Td>
                      <Group gap={6} justify="flex-end" wrap="nowrap">
                        <Button size="xs" variant="default" loading={testing === b.id} disabled={b.chats.length === 0} onClick={() => test(b)}>Test</Button>
                        <Button size="xs" variant="default" onClick={() => onEdit(b)}>{b.lastError ? 'Fix' : 'Edit'}</Button>
                        <Button size="xs" variant="subtle" color="rose" onClick={() => onRemove(b)}>Remove</Button>
                      </Group>
                    </Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          </Table.ScrollContainer>
        )}
      </SectionCard>
      {v.platform && (
        <SectionCard title="Platform bot" description="The bot in the control plane's configuration file. This organization can use it as well as its own bots; it is changed under Advanced, Alerts.">
          <Group gap="xs">
            <Badge variant="default" tt="none">Read only</Badge>
            <Text size={FS.body}>
              {v.platform.chats.length} chat{v.platform.chats.length === 1 ? '' : 's'}
              {v.platform.usedBy.length ? `, used by ${v.platform.usedBy.join(', ')}` : ', used by nothing here'}
            </Text>
          </Group>
        </SectionCard>
      )}
      <Text size="xs" c="dimmed">
        Tokens are encrypted when saved and never shown again, only their last three characters.
        Removing a bot asks where what used it should go instead.
      </Text>
    </Stack>
  )
}

function LastMessage({ b }: { b: TelegramBot }) {
  const failedLast = b.lastErrorAt && (!b.lastOkAt || b.lastErrorAt > b.lastOkAt)
  if (failedLast) {
    return (
      <Stack gap={0}>
        <Text size={FS.body} c="rose.8" fw={500}>Failed</Text>
        <Text size="xs" c="dimmed" lineClamp={2} title={b.lastError}>{b.lastError} · {ago(b.lastErrorAt)}</Text>
      </Stack>
    )
  }
  if (b.lastOkAt) {
    return (
      <Stack gap={0}>
        <Text size={FS.body} c="teal.8" fw={500}>Sent</Text>
        <Text size="xs" c="dimmed">{ago(b.lastOkAt)}</Text>
      </Stack>
    )
  }
  return <Text size={FS.body} c="dimmed">Nothing sent yet</Text>
}

function BotForm({ v, bot, onClose }: { v: TelegramView; bot: TelegramBot | null; onClose: () => void }) {
  const add = useAddBot()
  const change = useChangeBot()
  const [name, setName] = useState(bot?.name ?? '')
  const [token, setToken] = useState('')
  const [who, setWho] = useState<string | null>(bot ? `@${bot.username}` : null)
  const [checking, setChecking] = useState(false)
  const [known, setKnown] = useState<TelegramChat[]>(bot?.chats ?? [])
  const [chosen, setChosen] = useState<string[]>(bot?.chats.map((c) => c.id) ?? [])
  const [typed, setTyped] = useState('')
  const [isDefault, setDefault] = useState(bot ? bot.default : !v.bots.some((b) => b.default))
  const current = v.bots.find((b) => b.default)

  const check = () => {
    setChecking(true)
    void run(async () => {
      const me = await checkToken(token.trim())
      setWho(`@${me.username}`)
      if (!name.trim()) setName(me.name || me.username)
    }, { failure: 'Token not accepted' }).finally(() => setChecking(false))
  }
  const finder = useChatFinder(
    async () => (await findChats(token.trim() ? { token: token.trim() } : { bot: bot?.id ?? '' })).chats,
    known.map((k) => k.id),
    (found, fresh) => {
      setKnown((prev) => {
        const merged = [...prev]
        for (const c of found) if (!merged.some((k) => k.id === c.id)) merged.push(c)
        return merged
      })
      // A chat that turned up while watching is the one the person just
      // messaged: tick it, unless it is someone's private chat.
      if (fresh) {
        const groups = found.filter((c) => c.kind !== 'private').map((c) => c.id)
        if (groups.length) setChosen((prev) => [...prev, ...groups.filter((id) => !prev.includes(id))])
        notifyOk('Found it', `${found.map((c) => chatOption(c).label).join(', ')} ${groups.length ? 'is ticked' : 'is listed'} below.`)
      }
    },
  )
  const addTyped = () => {
    const id = typed.trim()
    if (!id) return
    if (!known.some((k) => k.id === id)) setKnown([...known, { id, title: id }])
    if (!chosen.includes(id)) setChosen([...chosen, id])
    setTyped('')
  }
  const save = () => {
    const input = { name: name.trim(), token: token.trim(), default: isDefault, chats: known.filter((k) => chosen.includes(k.id)) }
    void run(async () => {
      if (bot) await change.mutateAsync({ id: bot.id, input })
      else await add.mutateAsync(input)
    }, { failure: bot ? 'Bot not saved' : 'Bot not added', success: [bot ? 'Saved' : 'Bot added', `${input.name} posts to ${input.chats.length} chat${input.chats.length === 1 ? '' : 's'}.`] })
      .then((ok) => { if (ok) onClose() })
  }
  const canFind = Boolean(token.trim() || bot)

  return (
    <Modal opened onClose={onClose} title={bot ? `Edit ${bot.name}` : 'Add a Telegram bot'} size="lg">
      <Stack gap="md">
        <TextInput label="Name" value={name} onChange={(e) => setName(e.currentTarget.value)} placeholder="Ops alerts" maxLength={80} />
        <Stack gap={4}>
          <Group gap="xs" align="flex-end" wrap="nowrap">
            <TextInput
              style={{ flex: 1 }}
              label="Bot token"
              description={bot ? `Saved token ends ${bot.tokenTail}. Leave empty to keep it.` : 'From @BotFather in Telegram. Argus checks it with Telegram before saving.'}
              placeholder={bot ? '••••••••' + bot.tokenTail : '123456789:AAH…'}
              value={token}
              onChange={(e) => { setToken(e.currentTarget.value); setWho(bot && !e.currentTarget.value ? `@${bot.username}` : null) }}
              autoComplete="off"
              styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)' } }}
            />
            <Button variant="default" onClick={check} loading={checking} disabled={!token.trim()}>Check</Button>
          </Group>
          {!bot && <TokenHelp initiallyOpen={v.bots.length === 0} check />}
          {who && token.trim() && (
            <Group gap={4}><IconCheck size={14} color={INK.verified} /><Text size="sm" c={INK.verified}>Works: this is {who}</Text></Group>
          )}
        </Stack>

        <Stack gap={6}>
          <Text size={FS.body} fw={600}>Chats it may post to</Text>
          <ChatHelp username={who && (token.trim() || bot) ? who : undefined} initiallyOpen={known.length === 0} findLabel="Find chats" />
          {known.length > 0 && (
            <Checkbox.Group value={chosen} onChange={setChosen}>
              <Stack gap={6}>
                {known.map((c) => (
                  <Checkbox key={c.id} value={c.id}
                    label={<Group gap={6}><Text size={FS.body}>{chatOption(c).label}</Text><Mono>{c.kind ? `${c.kind} · ` : ''}{c.id}</Mono></Group>} />
                ))}
              </Stack>
            </Checkbox.Group>
          )}
          <Group gap="xs" wrap="nowrap">
            <Button variant="default" size="xs" onClick={finder.find} loading={finder.busy} disabled={!canFind || finder.watching}>Find chats</Button>
            <TextInput size="xs" style={{ flex: 1 }} aria-label="Chat ID" placeholder="Or type a chat ID, such as -1001234567890"
              value={typed} onChange={(e) => setTyped(e.currentTarget.value)}
              onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); addTyped() } }} />
            <Button variant="default" size="xs" onClick={addTyped} disabled={!typed.trim()}>Add</Button>
          </Group>
          {!canFind && <Text size={FS.meta} c="dimmed">Paste the bot token first: Find chats asks Telegram with it.</Text>}
          <FinderStatus watching={finder.watching} missed={finder.missed} username={who && (token.trim() || bot) ? who : undefined} onStop={finder.stop} />
        </Stack>

        <Switch
          checked={isDefault}
          onChange={(e) => setDefault(e.currentTarget.checked)}
          label="Default bot for this organization"
          description={current && current.id !== bot?.id
            ? `Alerts set to "Default bot" use it. ${current.name} is the default now.`
            : 'Alerts set to "Default bot" use it.'}
        />
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button onClick={save} loading={add.isPending || change.isPending}
            disabled={!name.trim() || (!bot && !token.trim()) || !v.canSave}>
            {bot ? 'Save' : 'Add bot'}
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}

function RemoveBot({ v, bot, onClose }: { v: TelegramView; bot: TelegramBot; onClose: () => void }) {
  const remove = useRemoveBot()
  const options = botOptions(v).filter((o) => o.value !== bot.id)
  const [moveTo, setMoveTo] = useState(options.some((o) => o.value === '') ? '' : 'off')
  return (
    <Modal opened onClose={onClose} title={`Remove ${bot.name}?`}>
      <Stack gap="sm">
        <Text size={FS.body}>
          {bot.usedBy.length
            ? `${bot.usedBy.join(', ')} ${bot.usedBy.length === 1 ? 'uses' : 'use'} this bot. Choose where they go instead.`
            : 'Nothing uses this bot.'}
          {' '}Its token is deleted.
        </Text>
        {bot.usedBy.length > 0 && (
          <Select label="Send them through" allowDeselect={false} value={moveTo} onChange={(x) => setMoveTo(x ?? '')}
            data={options.map((o) => (o.value === '' && bot.default ? { ...o, label: 'Default bot (none after this)' } : o))} />
        )}
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button color="rose" loading={remove.isPending}
            onClick={() => void run(() => remove.mutateAsync({ id: bot.id, moveTo }), {
              failure: 'Bot not removed', success: ['Removed', `${bot.name} is gone.`],
            }).then((ok) => { if (ok) onClose() })}>
            Remove bot
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}

/* ── Where alerts go ─────────────────────────────────────────────────────── */

function Routes({ v }: { v: TelegramView }) {
  return (
    <SectionCard
      title="Where alerts go"
      icon={IconBrandTelegram}
      description="Choose a bot and its chats for each kind of alert. Leave chats empty for all of the bot's chats. The bell shows every alert either way."
      flush
    >
      <Table.ScrollContainer minWidth={760}>
        <Table verticalSpacing="sm" horizontalSpacing="md">
          <Table.Thead>
            <Table.Tr>
              <Table.Th w="30%">Alert</Table.Th>
              <Table.Th w="25%">Bot</Table.Th>
              <Table.Th>Chats</Table.Th>
              <Table.Th w={90} />
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {v.routes.map((r) => <RouteRow key={r.kind} v={v} r={r} />)}
          </Table.Tbody>
        </Table>
      </Table.ScrollContainer>
    </SectionCard>
  )
}

function RouteRow({ v, r }: { v: TelegramView; r: TelegramRoute }) {
  const set = useSetRoute()
  const [testing, setTesting] = useState(false)
  const backup = r.kind === 'backup'
  const chats = chatsOf(v, r.bot)
  const options = Array.from(new Map([...r.chats.map((id) => ({ value: id, label: id })), ...chats.map(chatOption)]
    .map((o) => [o.value, o] as const)).values())
  const save = (bot: string, list: string[]) =>
    void run(() => set.mutateAsync({ kind: r.kind, bot, chats: list }), { failure: 'Not saved' })
  const noDefault = r.bot === '' && !v.bots.some((b) => b.default) && (backup || !v.platform)
  return (
    <Table.Tr>
      <Table.Td>
        <Text size={FS.body} fw={600}>{ROUTE_LABEL[r.kind]}</Text>
        <Text size="xs" c="dimmed">{ROUTE_HELP[r.kind]}</Text>
      </Table.Td>
      <Table.Td>
        <Select aria-label={`Bot for ${ROUTE_LABEL[r.kind]}`} allowDeselect={false} value={r.bot}
          data={botOptions(v, { noPlatform: backup }).map((o) => (backup && o.value === '' && !v.bots.some((b) => b.default)
            ? { ...o, label: 'Default bot (none yet)' } : o))}
          onChange={(x) => save(x ?? '', [])} />
      </Table.Td>
      <Table.Td>
        {r.bot === 'off' ? (
          <Text size={FS.body} c="dimmed">No Telegram</Text>
        ) : noDefault ? (
          <Text size="xs" c="amber.8">No default bot yet: make one of the bots the default, or pick one here.</Text>
        ) : (
          <MultiSelect aria-label={`Chats for ${ROUTE_LABEL[r.kind]}`} data={options} value={r.chats}
            placeholder={r.chats.length ? undefined : `All of ${botLabel(v, r.bot).replace(/^Default bot \((.*)\)$/, '$1')}'s chats`}
            onChange={(list) => save(r.bot, list)} clearable />
        )}
      </Table.Td>
      <Table.Td>
        <Button size="xs" variant="default" disabled={r.bot === 'off' || noDefault} loading={testing}
          onClick={() => {
            setTesting(true)
            void run(async () => {
              const s = await testRoute(r.kind)
              notifyOk('Sent', `${s.bot} posted a test to ${s.chats.length} chat${s.chats.length === 1 ? '' : 's'}.`)
            }, { failure: 'Test not sent' }).finally(() => setTesting(false))
          }}>
          Test
        </Button>
      </Table.Td>
    </Table.Tr>
  )
}
