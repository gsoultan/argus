import { useState } from 'react'
import { Button, Tabs } from '@mantine/core'
import { IconClipboardCheck, IconKey, IconPlugConnected, IconPlus, IconShieldLock } from '@tabler/icons-react'
import { PageBody, PageHeader } from '@argus/shell'
import type { SectionContext, SectionModule, SectionRoot } from '~/mf/contract'
import { SecretsTab } from './SecretsTab'
import { RequestsTab } from './RequestsTab'
import { RulesTab } from './RulesTab'
import { AppsTab } from './AppsTab'

/*
 * argus-secret's screens: the micro-frontend the console mounts at /secrets.
 * Built on its own (vite.mf.config.ts) into /mf/secrets/<contract>/index.js;
 * see src/mf/contract.ts for what it is given and what it may import.
 */

export const contract = 1

type Tab = 'secrets' | 'requests' | 'rules' | 'apps'

const ADMINS = new Set(['admin', 'owner'])

const DESCRIBE: Record<Tab, string> = {
  secrets: 'Passwords, keys and tokens, kept on your organization’s secrets servers. Servers and apps read them directly; every read is in the audit log.',
  requests: 'Reads that wait for an approval.',
  rules: 'Who may see, read and change which secrets.',
  apps: 'Tokens for programs that read secrets without an Argus agent.',
}

const ADD: Partial<Record<Tab, string>> = { secrets: 'New secret', rules: 'New rule', apps: 'New token' }

function Secrets({ ctx }: { ctx: SectionContext }) {
  const admin = ADMINS.has(ctx.person.role)
  const tabs: Tab[] = admin ? ['secrets', 'requests', 'rules', 'apps'] : ['secrets', 'requests']
  const tab: Tab = tabs.find((t) => t === ctx.tab) ?? 'secrets'
  const [adding, setAdding] = useState<Tab | null>(null)
  const add = ADD[tab]
  const done = () => setAdding(null)

  return (
    <>
      <PageHeader
        title="Secrets"
        description={DESCRIBE[tab]}
        actions={add && <Button leftSection={<IconPlus size={16} />} onClick={() => setAdding(tab)}>{add}</Button>}
      />
      <PageBody>
        <Tabs value={tab} onChange={(v) => ctx.setTab(v === 'secrets' || !v ? undefined : v)}>
          <Tabs.List>
            <Tabs.Tab value="secrets" leftSection={<IconKey size={16} />}>Secrets</Tabs.Tab>
            <Tabs.Tab value="requests" leftSection={<IconClipboardCheck size={16} />}>Requests</Tabs.Tab>
            {admin && <Tabs.Tab value="rules" leftSection={<IconShieldLock size={16} />}>Rules</Tabs.Tab>}
            {admin && <Tabs.Tab value="apps" leftSection={<IconPlugConnected size={16} />}>App tokens</Tabs.Tab>}
          </Tabs.List>
        </Tabs>
        {tab === 'secrets' && (
          <SecretsTab creating={adding === 'secrets'} onCreated={done}
            openSettings={() => ctx.navigate('/settings?section=secrets-servers')} />
        )}
        {tab === 'requests' && <RequestsTab me={ctx.person.email} role={ctx.person.role} />}
        {tab === 'rules' && <RulesTab adding={adding === 'rules'} onAdded={done} />}
        {tab === 'apps' && <AppsTab adding={adding === 'apps'} onAdded={done} />}
      </PageBody>
    </>
  )
}

/** Mounts the screens; the console calls `update` when the tab changes. */
export function mount(element: HTMLElement, ctx: SectionContext): () => void {
  const root = ctx.render(element, <Secrets ctx={ctx} />)
  roots.set(element, root)
  return () => { root.unmount(); roots.delete(element) }
}

const roots = new Map<HTMLElement, SectionRoot>()

export function update(element: HTMLElement, ctx: SectionContext): void {
  roots.get(element)?.render(<Secrets ctx={ctx} />)
}

// Checked by the compiler: this module is what the shell expects.
const _module: SectionModule = { contract, mount, update }
void _module
