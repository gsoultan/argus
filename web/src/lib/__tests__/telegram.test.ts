import { describe, expect, it } from 'vitest'
import { botLabel, botOptions, chatOption, chatsOf, type TelegramView } from '~/lib/telegram'

const bot = (id: string, name: string, def: boolean, chats: string[]) => ({
  id, name, username: id, tokenTail: 'abc', default: def, createdBy: '', createdAt: '', updatedAt: '', usedBy: [],
  chats: chats.map((c) => ({ id: c, title: c.toUpperCase() })),
})

const v: TelegramView = {
  canSave: true,
  bots: [bot('ops', 'Ops', true, ['-1']), bot('pay', 'Pay', false, ['-2', '-3'])],
  routes: [],
  platform: { chats: [{ id: '-9', title: '-9' }], usedBy: [] },
}

describe('telegram choices', () => {
  it('names the default bot after the bot it stands for', () => {
    expect(botLabel(v, '')).toBe('Default bot (Ops)')
    expect(botLabel({ ...v, bots: [] }, '')).toBe('Default bot (Platform bot)')
    expect(botLabel({ ...v, bots: [], platform: undefined }, '')).toBe('Default bot (none yet)')
    expect(botLabel(v, 'gone')).toBe('A removed bot')
  })
  it("offers only the chosen bot's chats", () => {
    expect(chatsOf(v, '').map((c) => c.id)).toEqual(['-1'])
    expect(chatsOf(v, 'pay').map((c) => c.id)).toEqual(['-2', '-3'])
    expect(chatsOf(v, 'platform').map((c) => c.id)).toEqual(['-9'])
    expect(chatsOf(v, 'off')).toEqual([])
  })
  it('leaves the platform bot out where asked', () => {
    expect(botOptions(v).map((o) => o.value)).toEqual(['', 'ops', 'pay', 'platform', 'off'])
    expect(botOptions(v, { noPlatform: true, withOff: false }).map((o) => o.value)).toEqual(['', 'ops', 'pay'])
  })
  it('shows a chat by its title', () => {
    expect(chatOption({ id: '-1', title: 'Ops' })).toEqual({ value: '-1', label: 'Ops' })
    expect(chatOption({ id: '-1', title: '-1' })).toEqual({ value: '-1', label: '-1' })
  })
})
