/** Telegram bots as the control plane serves them, for the e2e specs. */
const ago = (m: number) => new Date(Date.now() - m * 60_000).toISOString()

const bot = (id: string, name: string, username: string, tail: string, chats: { id: string; title: string }[], def = false) => ({
  id, name, username, tokenTail: tail, chats, default: def, createdBy: 'dev@northwind.id',
  createdAt: ago(60 * 24 * 9), updatedAt: ago(60 * 24 * 9), usedBy: [] as string[], lastOkAt: ago(4),
})
export const TELEGRAM = {
  canSave: true,
  bots: [
    { ...bot('b-ops', 'Ops alerts', 'northwind_ops_bot', '7Qa', [{ id: '-100ops', title: 'Ops on-call' }, { id: '-100infra', title: 'Infra team' }], true),
      usedBy: ['Sign-in alerts', 'Server connected', 'Websites', 'Backups'] },
    { ...bot('b-pay', 'Payments team', 'nw_payments_bot', 'c2M', [{ id: '-100payments', title: 'Payments' }]), usedBy: ['Website Payments API'] },
  ],
  routes: [
    { kind: 'signin', bot: '', chats: [] },
    { kind: 'server', bot: 'b-ops', chats: ['-100infra'] },
    { kind: 'website', bot: 'b-ops', chats: ['-100ops'] },
    { kind: 'backup', bot: '', chats: [] },
  ],
}

