import { useEffect, useMemo, useState } from 'react'
import {
  ActionIcon, Alert, Badge, Box, Button, Card, Code, Drawer, FileButton, Group, Modal, NumberInput, ScrollArea, Select,
  SegmentedControl, SimpleGrid, Skeleton, Stack, Table, Tabs, Text, TextInput, Textarea,
} from '@mantine/core'
import { useDebouncedValue, useMediaQuery } from '@mantine/hooks'
import { useQuery } from '@tanstack/react-query'
import { IconBrandAndroid, IconCode, IconPackage, IconPlus, IconTrash, IconUpload } from '@tabler/icons-react'
import { EmptyState, SectionCard } from '~/components/page'
import { relTime } from '~/components/primitives'
import { run as notifyRun } from '~/lib/notify'
import { meQuery } from '~/lib/queries'
import {
  fileBase64, findingKey,
  type ScriptFinding, type ScriptParam, type ScriptPartInput, type ScriptPlatform, type ScriptVersion,
  signerShort, sizeOf, type ScriptVersionInput, type SoftwareFile, type SoftwareScript,
} from '~/lib/softwareApi'
import {
  scriptQuery, scriptRulesQuery, scriptsQuery, softwareFilesQuery, useCheckScript, useCreateScript, useRemoveScript,
  useRemoveSoftwareFile, useReviewScript, useSaveScriptVersion, useUploadSoftwareFile,
} from '~/lib/softwareQueries'
import { FS, SP } from '~/theme'

/**
 * Settings › Software: the install scripts Argus may run on servers.
 *
 * A script is what someone would otherwise paste into a root shell. Here it
 * is text that came in through two doors (pasted, or a .sh/.ps1 uploaded),
 * checked automatically on every save, reviewed by someone other than its
 * author, and signed: the agent runs exactly the version reviewed, and a new
 * version must run successfully outside production before it runs there.
 */
export function SoftwareLibrary({ canEdit }: { canEdit: boolean }) {
  const { data, isLoading, error } = useQuery(scriptsQuery())
  const [editing, setEditing] = useState<SoftwareScript | 'new' | null>(null)
  const [open, setOpen] = useState<string | null>(null)
  return (
    <Stack gap="md">
      <SectionCard title="Install scripts" icon={IconCode} flush
        description="Scripts for software that is in no repository. Each version is checked automatically, reviewed by someone other than its author, and tried outside production before it runs there."
        action={canEdit ? (
          <Button size="compact-sm" leftSection={<IconPlus size={14} />} onClick={() => setEditing('new')}>New script</Button>
        ) : undefined}>
        {error ? (
          <Alert color="rose" m="md"><Text size={FS.body}>{error instanceof Error ? error.message : String(error)}</Text></Alert>
        ) : isLoading || !data ? <Skeleton h={120} m="md" /> : data.length === 0 ? (
          <EmptyState compact icon={IconCode} title="No scripts yet."
            description="Add one for a vendor's installer, an agent with an enrolment token, or a tarball unpacked into /opt." />
        ) : (
          <Table verticalSpacing={8} fz="sm">
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Script</Table.Th>
                <Table.Th>Platform</Table.Th>
                <Table.Th>Runs</Table.Th>
                <Table.Th>Newest</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {data.map((s) => (
                <Table.Tr key={s.id} style={{ cursor: 'pointer' }} onClick={() => setOpen(s.id)}>
                  <Table.Td>
                    <Text size="sm" fw={600}>{s.name}</Text>
                    {s.description && <Text size={FS.micro} c="dimmed" lineClamp={1}>{s.description}</Text>}
                  </Table.Td>
                  <Table.Td><PlatformBadge p={s.platform} /></Table.Td>
                  <Table.Td>
                    {s.reviewed ? (
                      <Text size={FS.meta}>v{s.reviewed.version}{s.reviewed.triedOn ? '' : ' · not tried yet'}</Text>
                    ) : <Text size={FS.meta} c="dimmed">nothing reviewed</Text>}
                  </Table.Td>
                  <Table.Td>{s.latest && <VersionState v={s.latest} />}</Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        )}
      </SectionCard>
      <FilesCard canEdit={canEdit} />
      <RulesCard />
      {open && <ScriptDetail id={open} canEdit={canEdit} onClose={() => setOpen(null)}
        onEdit={(s) => { setOpen(null); setEditing(s) }} />}
      {editing && <ScriptEditor script={editing === 'new' ? null : editing} onClose={() => setEditing(null)} />}
    </Stack>
  )
}

function PlatformBadge({ p }: { p: ScriptPlatform }) {
  return <Badge size="xs" variant="light" color="slate">{p === 'windows' ? 'PowerShell' : 'Linux shell'}</Badge>
}

export function VersionState({ v }: { v: ScriptVersion }) {
  if (v.blocked) return <Badge size="xs" variant="light" color="rose">v{v.version} · blocked</Badge>
  if (!v.reviewedBy) return <Badge size="xs" variant="light" color="amber">v{v.version} · needs review</Badge>
  return (
    <Badge size="xs" variant="light" color="teal">
      v{v.version} · reviewed{v.selfReviewed ? ' by its author' : ` by ${v.reviewedBy}`}
    </Badge>
  )
}

const LEVEL: Record<ScriptFinding['level'], { color: string; label: string }> = {
  blocked: { color: 'rose', label: 'Blocked' },
  warning: { color: 'amber', label: 'Warning' },
  note: { color: 'slate', label: 'Note' },
}

export function Findings({ findings, empty }: { findings: ScriptFinding[]; empty?: string }) {
  const blocked = findings.filter((f) => f.level === 'blocked').length
  const warnings = findings.filter((f) => f.level === 'warning').length
  return (
    <Stack gap={SP.cozy}>
      <Group gap={SP.tight}>
        <Text size={FS.body} fw={600}>Safety checks</Text>
        {blocked > 0 && <Badge size="xs" variant="light" color="rose">{blocked} blocked</Badge>}
        {warnings > 0 && <Badge size="xs" variant="light" color="amber">{warnings} warning{warnings === 1 ? '' : 's'}</Badge>}
      </Group>
      {findings.length === 0 && <Text size={FS.meta} c="dimmed">{empty ?? 'Nothing found.'}</Text>}
      {findings.map((f) => (
        <Box key={findingKey(f) + f.title}>
          <Group gap={SP.snug} wrap="nowrap" align="flex-start">
            <Badge size="xs" variant="light" color={LEVEL[f.level].color}>{LEVEL[f.level].label}</Badge>
            <Text size={FS.body} fw={600}>{f.title}</Text>
          </Group>
          {f.line ? <Text size={FS.micro} c="dimmed">{f.part} · line {f.line}{f.excerpt ? `: ${f.excerpt}` : ''}</Text> : null}
          {f.why && <Text size={FS.micro} c="dimmed">{f.why}</Text>}
        </Box>
      ))}
    </Stack>
  )
}

/* ── Writing a version ───────────────────────────────────────────────────── */

type Part = 'install' | 'uninstall' | 'check'

function ScriptEditor({ script, onClose }: { script: SoftwareScript | null; onClose: () => void }) {
  const base = script?.versions?.[0] ?? script?.latest
  const [name, setName] = useState(script?.name ?? '')
  const [description, setDescription] = useState(script?.description ?? '')
  const [platform, setPlatform] = useState<ScriptPlatform>(script?.platform ?? 'linux')
  const [texts, setTexts] = useState<Record<Part, string>>({
    install: base?.install ?? '', uninstall: base?.uninstall ?? '', check: base?.check ?? '',
  })
  const [files, setFiles] = useState<Partial<Record<Part, ScriptPartInput>>>({})
  const [part, setPart] = useState<Part>('install')
  const [params, setParams] = useState<ScriptParam[]>(base?.params ?? [])
  const [runAs, setRunAs] = useState(base?.runAs ?? '')
  const [timeout, setTimeoutMin] = useState<number>(base ? Math.round(base.timeoutSeconds / 60) : 30)
  const [reboot, setReboot] = useState<'never' | 'if_asked'>(base?.reboot ?? 'never')
  const [note, setNote] = useState('')
  const create = useCreateScript()
  const save = useSaveScriptVersion()
  const check = useCheckScript()
  const [findings, setFindings] = useState<Record<Part, ScriptFinding[]>>({ install: [], uninstall: [], check: [] })
  const [debounced] = useDebouncedValue(texts, 800)
  const narrow = useMediaQuery('(max-width: 62em)') ?? false

  // Checked as it is written: the same checks a save runs.
  useEffect(() => {
    for (const p of ['install', 'uninstall', 'check'] as Part[]) {
      const text = debounced[p]
      if (!text.trim()) {
        setFindings((f) => ({ ...f, [p]: [] }))
        continue
      }
      check.mutateAsync({ platform, part: p, text }).then((r) => setFindings((f) => ({ ...f, [p]: r.findings })),
        () => undefined)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [debounced, platform])

  const upload = async (file: File | null) => {
    if (!file) return
    const input = { fileName: file.name, fileBase64: await fileBase64(file) }
    // The control plane decides what a file is: it reads it back as text.
    const ok = await notifyRun(async () => {
      const r = await check.mutateAsync({ platform, part, ...input })
      setTexts((t) => ({ ...t, [part]: r.text }))
      setFindings((f) => ({ ...f, [part]: r.findings }))
      setFiles((fs) => ({ ...fs, [part]: input }))
    }, { failure: `${file.name} was not accepted` })
    if (!ok) setFiles((fs) => ({ ...fs, [part]: undefined }))
  }
  const partInput = (p: Part): ScriptPartInput => files[p] && texts[p] !== '' ? files[p]! : { text: texts[p] }
  const all = [...findings.install, ...findings.uninstall, ...findings.check]
  const blocked = all.some((f) => f.level === 'blocked')
  const body: ScriptVersionInput = {
    install: partInput('install'), uninstall: partInput('uninstall'), check: partInput('check'),
    params, runAs, timeoutMinutes: timeout, reboot, note,
  }
  const submit = () => void notifyRun(async () => {
    if (script) await save.mutateAsync({ id: script.id, body })
    else await create.mutateAsync({ name, platform, description, version: body })
  }, {
    failure: 'Not saved',
    success: ['Saved', blocked ? 'It cannot be reviewed until what is blocked is changed.' : 'It runs nowhere until someone other than you reviews it.'],
  }).then((ok) => ok && onClose())

  return (
    <Modal opened onClose={onClose} size="90%" title={<Box>
      <Text fw={600}>{script ? `New version of ${script.name}` : 'New script'}</Text>
      <Text size={FS.micro} c="dimmed">Saved as a new version. It cannot run until the checks pass and someone other than you reviews it.</Text>
    </Box>}>
      <Stack gap="sm">
        <Group gap="sm" wrap="wrap" align="flex-end">
          {!script && (
            <>
              <TextInput label="Name" value={name} onChange={(e) => setName(e.currentTarget.value)} w={260} required />
              <Select label="Platform" w={200} value={platform} allowDeselect={false}
                onChange={(v) => setPlatform((v ?? 'linux') as ScriptPlatform)}
                data={[{ value: 'linux', label: 'Linux · bash' }, { value: 'windows', label: 'Windows · PowerShell' }]} />
            </>
          )}
          <TextInput label="Run as" placeholder={platform === 'windows' ? 'SYSTEM' : 'root'} w={160} value={runAs}
            onChange={(e) => setRunAs(e.currentTarget.value)} description="A local account; empty is root" />
          <NumberInput label="Timeout, minutes" w={140} min={1} max={120} value={timeout}
            onChange={(v) => setTimeoutMin(Number(v) || 30)} />
          <Select label="Reboot" w={220} value={reboot} allowDeselect={false}
            onChange={(v) => setReboot((v ?? 'never') as 'never' | 'if_asked')}
            data={[{ value: 'never', label: 'Never' }, { value: 'if_asked', label: 'If the script asks for one' }]} />
        </Group>
        {!script && (
          <TextInput label="What it installs" value={description} onChange={(e) => setDescription(e.currentTarget.value)} />
        )}
        <SimpleGrid cols={narrow ? 1 : 2} spacing="md" style={{ gridTemplateColumns: narrow ? undefined : '3fr 2fr' }}>
          <Card padding={0} withBorder>
            <Group justify="space-between" px="sm" py={SP.cozy}>
              <SegmentedControl size="xs" value={part} onChange={(v) => setPart(v as Part)} data={[
                { value: 'install', label: 'Install' }, { value: 'uninstall', label: 'Uninstall' }, { value: 'check', label: 'Check' },
              ]} />
              <FileButton onChange={(f) => void upload(f)} accept={platform === 'windows' ? '.ps1,.psm1' : '.sh,.bash'}>
                {(props) => <Button {...props} size="compact-xs" variant="default" leftSection={<IconUpload size={12} />}>Upload a file</Button>}
              </FileButton>
            </Group>
            <Textarea aria-label={`${part} script`} value={texts[part]} autosize minRows={16} maxRows={28}
              styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)', fontSize: 12, borderRadius: 0 } }}
              placeholder={PLACEHOLDER[part][platform]}
              onChange={(e) => {
                const v = e.currentTarget.value
                setTexts((t) => ({ ...t, [part]: v }))
                setFiles((fs) => ({ ...fs, [part]: undefined }))
              }} />
            <Text size={FS.micro} c="dimmed" px="sm" py={SP.tight}>
              {part === 'install' ? 'Required.' : part === 'uninstall'
                ? 'Optional. Without it there is no Uninstall for what this script installs.'
                : 'Optional. Prints the installed version and exits 0, or exits 1 when it is not installed.'}{' '}
              Parameters arrive as {platform === 'windows' ? '$env:ARGUS_PARAM_NAME' : '$ARGUS_PARAM_NAME'}, never in the text.
            </Text>
          </Card>
          <Stack gap="md">
            <Card withBorder padding="sm"><Findings findings={all} empty="Write the install script to check it." /></Card>
            <Card withBorder padding="sm"><ParamsEditor params={params} onChange={setParams} /></Card>
          </Stack>
        </SimpleGrid>
        <TextInput label="What changed in this version" value={note} onChange={(e) => setNote(e.currentTarget.value)} />
        <Group justify="space-between" wrap="wrap">
          <Text size={FS.micro} c="dimmed">Next: fix what is blocked → review by another admin → a first run on a server that isn't production.</Text>
          <Group gap="sm">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <Button onClick={submit} loading={create.isPending || save.isPending}
              disabled={!texts.install.trim() || (!script && !name.trim())}>
              Save version
            </Button>
          </Group>
        </Group>
      </Stack>
    </Modal>
  )
}

const PLACEHOLDER: Record<Part, Record<ScriptPlatform, string>> = {
  install: {
    linux: '#!/bin/bash\ncurl -fsSL "https://vendor.example/agent-${ARGUS_PARAM_VERSION}.deb" -o /tmp/agent.deb\necho "${ARGUS_PARAM_SHA256}  /tmp/agent.deb" | sha256sum -c -\ndpkg -i /tmp/agent.deb',
    windows: '$ErrorActionPreference = \'Stop\'\nInvoke-WebRequest -Uri $url -OutFile $msi\nif ((Get-FileHash $msi).Hash -ne $env:ARGUS_PARAM_SHA256) { throw "hash" }\nStart-Process msiexec -ArgumentList "/i $msi /qn" -Wait',
  },
  uninstall: { linux: 'apt-get remove -y vendor-agent', windows: 'Start-Process msiexec -ArgumentList "/x {GUID} /qn" -Wait' },
  check: { linux: "dpkg-query -W -f='${Version}' vendor-agent", windows: '(Get-Package "Vendor Agent").Version' },
}

function ParamsEditor({ params, onChange }: { params: ScriptParam[]; onChange: (p: ScriptParam[]) => void }) {
  const set = (i: number, p: Partial<ScriptParam>) => onChange(params.map((x, j) => (j === i ? { ...x, ...p } : x)))
  return (
    <Stack gap={SP.cozy}>
      <Group justify="space-between">
        <Text size={FS.body} fw={600}>Parameters</Text>
        <Button size="compact-xs" variant="subtle" leftSection={<IconPlus size={12} />}
          onClick={() => onChange([...params, { name: '', label: '', kind: 'text' }])}>Add</Button>
      </Group>
      {params.length === 0 && <Text size={FS.micro} c="dimmed">None. A version, a URL or a token the person running it fills in.</Text>}
      {params.map((p, i) => (
        <Group key={i} gap={SP.tight} wrap="nowrap" align="flex-end">
          <TextInput size="xs" aria-label="Parameter name" placeholder="NAME" value={p.name} w={120}
            onChange={(e) => set(i, { name: e.currentTarget.value.toUpperCase() })} />
          <Select size="xs" aria-label="Parameter kind" w={96} value={p.kind} allowDeselect={false}
            onChange={(v) => set(i, { kind: (v ?? 'text') as ScriptParam['kind'] })}
            data={[{ value: 'text', label: 'Text' }, { value: 'choice', label: 'Choice' }, { value: 'secret', label: 'Secret' }]} />
          {p.kind === 'choice' ? (
            <TextInput size="xs" aria-label="Options" placeholder="8.15.3, 8.14.3" style={{ flex: 1 }}
              value={(p.choices ?? []).join(', ')}
              onChange={(e) => set(i, { choices: e.currentTarget.value.split(',').map((c) => c.trim()).filter(Boolean) })} />
          ) : p.kind === 'secret' ? (
            <Text size={FS.micro} c="dimmed" style={{ flex: 1 }}>masked everywhere</Text>
          ) : (
            <TextInput size="xs" aria-label="Default" placeholder="default" style={{ flex: 1 }} value={p.default ?? ''}
              onChange={(e) => set(i, { default: e.currentTarget.value })} />
          )}
          <ActionIcon variant="subtle" color="slate" aria-label="Remove parameter"
            onClick={() => onChange(params.filter((_, j) => j !== i))}><IconTrash size={14} /></ActionIcon>
        </Group>
      ))}
    </Stack>
  )
}

/* ── A script, its versions, and reviewing one ───────────────────────────── */

function ScriptDetail({ id, canEdit, onClose, onEdit }: {
  id: string; canEdit: boolean; onClose: () => void; onEdit: (s: SoftwareScript) => void
}) {
  const { data: s } = useQuery(scriptQuery(id))
  const [picked, setPicked] = useState<string | null>(null)
  const remove = useRemoveScript()
  const v = s?.versions?.find((x) => x.id === picked) ?? s?.versions?.[0]
  return (
    <Drawer opened onClose={onClose} position="right" size={760} title={s ? (
      <Group gap={SP.snug}><Text fw={600}>{s.name}</Text><PlatformBadge p={s.platform} /></Group>
    ) : 'Script'}>
      {!s || !v ? <Skeleton h={300} /> : (
        <Stack gap="md">
          {s.description && <Text size={FS.body} c="dimmed">{s.description}</Text>}
          <Group justify="space-between">
            <Select size="xs" w={260} aria-label="Version" value={v.id} allowDeselect={false} onChange={setPicked}
              data={(s.versions ?? []).map((x) => ({ value: x.id, label: `v${x.version} · ${x.author} · ${relTime(x.createdAt)}` }))} />
            {canEdit && (
              <Group gap="xs">
                <Button size="compact-sm" variant="default" onClick={() => onEdit(s)}>New version</Button>
                <Button size="compact-sm" variant="subtle" color="rose" loading={remove.isPending}
                  onClick={() => void notifyRun(() => remove.mutateAsync(s.id), {
                    failure: 'Not removed', success: ['Removed from the library', 'What it installed stays installed.'],
                  }).then((ok) => ok && onClose())}>
                  Remove
                </Button>
              </Group>
            )}
          </Group>
          <Group gap="xs">
            <VersionState v={v} />
            <Text size={FS.micro} c="dimmed">
              {v.runAs || 'root'} · {Math.round(v.timeoutSeconds / 60)} min · install {v.installSha.slice(0, 12)}
              {v.triedOn ? ` · tried on ${v.triedOn}` : v.reviewedBy ? ' · not yet tried outside production' : ''}
            </Text>
          </Group>
          {v.note && <Text size={FS.body}>{v.note}</Text>}
          {canEdit && !v.reviewedBy && !v.blocked && <Review v={v} />}
          <Findings findings={v.findings} />
          {v.accepted.length > 0 && (
            <Stack gap={SP.tight}>
              <Text size={FS.body} fw={600}>Accepted by the reviewer</Text>
              {v.accepted.map((a) => <Text key={a.key} size={FS.micro} c="dimmed">{a.key}: {a.reason}</Text>)}
            </Stack>
          )}
          <Tabs defaultValue="install" variant="pills" radius="sm">
            <Tabs.List>
              <Tabs.Tab value="install">Install</Tabs.Tab>
              <Tabs.Tab value="uninstall" disabled={!v.uninstall}>Uninstall</Tabs.Tab>
              <Tabs.Tab value="check" disabled={!v.check}>Check</Tabs.Tab>
            </Tabs.List>
            {(['install', 'uninstall', 'check'] as Part[]).map((p) => (
              <Tabs.Panel key={p} value={p} pt="xs">
                <ScrollArea h={320} type="auto"><Code block fz={FS.micro}>{v[p] ?? ''}</Code></ScrollArea>
              </Tabs.Panel>
            ))}
          </Tabs>
          {v.params.length > 0 && (
            <Text size={FS.micro} c="dimmed">
              Parameters: {v.params.map((p) => `${p.name} (${p.kind}${p.required ? ', required' : ''})`).join(', ')}
            </Text>
          )}
        </Stack>
      )}
    </Drawer>
  )
}

function Review({ v }: { v: ScriptVersion }) {
  const { data: me } = useQuery(meQuery())
  const review = useReviewScript()
  const warnings = useMemo(() => v.findings.filter((f) => f.level === 'warning'), [v])
  const [reasons, setReasons] = useState<Record<string, string>>({})
  const [confirm, setConfirm] = useState('')
  const mine = me?.email?.toLowerCase() === v.author.toLowerCase()
  const ready = warnings.every((f) => (reasons[findingKey(f)] ?? '').trim().length >= 3)
  return (
    <Card withBorder padding="sm">
      <Stack gap="sm">
        <Text size={FS.body} fw={600}>Review v{v.version}</Text>
        <Text size={FS.micro} c="dimmed">
          Read the whole script below. Approving signs exactly this text: the agent refuses anything else. What this
          cannot protect against: something it downloads that changes later, and approving without reading.
        </Text>
        {mine && (
          <Alert color="amber" p="xs">
            <Text size={FS.body}>You wrote this version. Someone else reviews it, unless you are this Argus's only admin:
              then type "review my own script" and Activity says it was self-reviewed.</Text>
          </Alert>
        )}
        {warnings.map((f) => (
          <TextInput key={findingKey(f)} size="xs" label={`${f.title} (${f.part}${f.line ? ` line ${f.line}` : ''})`}
            placeholder="Why this is acceptable" value={reasons[findingKey(f)] ?? ''}
            onChange={(e) => {
              const val = e.currentTarget.value
              setReasons((r) => ({ ...r, [findingKey(f)]: val }))
            }} />
        ))}
        {mine && <TextInput size="xs" aria-label="Confirm self-review" placeholder="review my own script" value={confirm}
          onChange={(e) => setConfirm(e.currentTarget.value)} />}
        <Group justify="flex-end">
          <Button size="compact-sm" color="teal" disabled={!ready} loading={review.isPending}
            onClick={() => void notifyRun(() => review.mutateAsync({
              versionId: v.id,
              accepted: warnings.map((f) => ({ key: findingKey(f), reason: reasons[findingKey(f)] ?? '' })),
              ...(mine ? { confirmSelf: confirm } : {}),
            }), { failure: 'Not approved', success: ['Approved', `v${v.version} can now run, first on a server outside production.`] })}>
            Approve v{v.version}
          </Button>
        </Group>
      </Stack>
    </Card>
  )
}

function RulesCard() {
  const [platform, setPlatform] = useState<ScriptPlatform>('linux')
  const [open, setOpen] = useState(false)
  const { data } = useQuery({ ...scriptRulesQuery(platform), enabled: open })
  return (
    <SectionCard title="What the checks look for" flush
      description="Run on the text of every version, never by executing it. Blocked cannot be waived; a reviewer accepts each warning with a reason."
      action={<Button size="compact-sm" variant="default" onClick={() => setOpen(!open)}>{open ? 'Hide' : 'Show'} rules</Button>}>
      {open && (
        <Box px="md" pb="md">
          <SegmentedControl size="xs" mb="xs" value={platform} onChange={(v) => setPlatform(v as ScriptPlatform)}
            data={[{ value: 'linux', label: 'Linux shell' }, { value: 'windows', label: 'PowerShell' }]} />
          <Table verticalSpacing={4} fz="sm">
            <Table.Tbody>
              {(data ?? []).map((r) => (
                <Table.Tr key={r.id}>
                  <Table.Td w={100}><Badge size="xs" variant="light" color={LEVEL[r.level].color}>{LEVEL[r.level].label}</Badge></Table.Td>
                  <Table.Td><Text size={FS.meta}>{r.title}</Text><Text size={FS.micro} c="dimmed">{r.group}</Text></Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Box>
      )}
    </SectionCard>
  )
}

/* ── Files: APKs for devices ─────────────────────────────────────────────── */

function FilesCard({ canEdit }: { canEdit: boolean }) {
  const { data, error } = useQuery(softwareFilesQuery())
  const add = useUploadSoftwareFile()
  const remove = useRemoveSoftwareFile()
  const [added, setAdded] = useState<SoftwareFile | null>(null)
  const [winFile, setWinFile] = useState<File | null>(null)
  const pick = (file: File | null) => {
    if (!file) return
    setAdded(null)
    if (/\.(msi|exe)$/i.test(file.name)) {
      setWinFile(file)
      return
    }
    void notifyRun(async () => setAdded(await add.mutateAsync({ file, note: '' })), { failure: `${file.name} was not added` })
  }
  return (
    <SectionCard title="Installers" icon={IconPackage} flush
      description="Android packages for devices, and MSI or EXE installers for Windows servers, installed from their Apps tab. Each is read before it is kept: what it is and who signed it, pinned by its SHA-256. Removing one removes the file only, never what was installed with it."
      action={canEdit ? (
        <FileButton onChange={pick} accept=".apk,.msi,.exe">
          {(props) => <Button {...props} size="compact-sm" loading={add.isPending} leftSection={<IconUpload size={14} />}>Add a file</Button>}
        </FileButton>
      ) : undefined}>
      {winFile && (
        <WindowsFileForm file={winFile} onClose={() => setWinFile(null)}
          onAdded={(f) => { setAdded(f); setWinFile(null) }} />
      )}
      {added && (
        <Alert color={added.warnings?.length ? 'amber' : 'teal'} m="md" withCloseButton onClose={() => setAdded(null)}>
          <Text size={FS.body} fw={600}>Added {added.name} {added.app?.versionName}</Text>
          {added.app ? (
            <Text size={FS.micro} c="dimmed" ff="monospace">
              {added.app.package} · version code {added.app.versionCode} · signed {added.app.scheme} by {signerShort(added.app.signer)}
            </Text>
          ) : (
            <Text size={FS.micro} c="dimmed" ff="monospace">
              {added.fileName} · runs with {added.windows?.args.join(' ') || 'no arguments'}{added.windows?.subject ? ` · signed by ${added.windows.subject}` : ''}
            </Text>
          )}
          {added.warnings?.map((w) => <Text key={w} size={FS.body} mt={4}>{w}</Text>)}
        </Alert>
      )}
      {error ? (
        <Alert color="rose" m="md"><Text size={FS.body}>{error instanceof Error ? error.message : String(error)}</Text></Alert>
      ) : !data ? <Skeleton h={80} m="md" /> : data.length === 0 ? (
        <EmptyState compact icon={IconBrandAndroid} title="No installers yet."
          description="Add a company app's APK for devices, or an MSI or EXE for Windows servers, with the switches that install it quietly." />
      ) : (
        <Table verticalSpacing={8} fz="sm">
          <Table.Thead>
            <Table.Tr>
              <Table.Th>App</Table.Th>
              <Table.Th>Version or switches</Table.Th>
              <Table.Th>Signed by</Table.Th>
              <Table.Th>Size</Table.Th>
              <Table.Th />
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {data.map((f) => (
              <Table.Tr key={f.id}>
                <Table.Td>
                  <Text size="sm" fw={600}>{f.name}</Text>
                  <Text size={FS.micro} c="dimmed" ff="monospace">{f.app?.package ?? f.fileName}</Text>
                  <Badge size="xs" variant="light" color="slate" mt={2}>{f.platform === 'windows' ? `Windows · ${f.kind.toUpperCase()}` : 'Android'}</Badge>
                </Table.Td>
                <Table.Td>
                  {f.app
                    ? <Text size={FS.meta}>{f.app.versionName} <Text span c="dimmed" size={FS.micro}>({f.app.versionCode})</Text></Text>
                    : <Text size={FS.micro} ff="monospace">{f.windows?.args.join(' ') || '—'}</Text>}
                </Table.Td>
                <Table.Td>
                  {f.app
                    ? <Text size={FS.micro} ff="monospace" title={f.app.signer}>{signerShort(f.app.signer)}</Text>
                    : <Text size={FS.micro} title={f.windows?.signer}>{f.windows?.subject || (f.kind === 'msi' ? 'not read' : 'not signed')}</Text>}
                </Table.Td>
                <Table.Td><Text size={FS.meta}>{sizeOf(f.size)}</Text></Table.Td>
                <Table.Td ta="right">
                  {canEdit && (
                    <ActionIcon variant="subtle" color="slate" aria-label={`Remove ${f.name}`} loading={remove.isPending}
                      onClick={() => void notifyRun(() => remove.mutateAsync(f.id),
                        { failure: 'Not removed', success: ['Removed from the library', 'Devices that installed it keep the app.'] })}>
                      <IconTrash size={14} />
                    </ActionIcon>
                  )}
                </Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      )}
    </SectionCard>
  )
}

/** The name and quiet switches an MSI or EXE is kept with. */
function WindowsFileForm({ file, onClose, onAdded }: { file: File; onClose: () => void; onAdded: (f: SoftwareFile) => void }) {
  const add = useUploadSoftwareFile()
  const exe = /\.exe$/i.test(file.name)
  const [name, setName] = useState(file.name.replace(/\.(msi|exe)$/i, ''))
  const [args, setArgs] = useState('')
  const [note, setNote] = useState('')
  const save = () => void notifyRun(async () => onAdded(await add.mutateAsync({ file, note, name, args })),
    { failure: `${file.name} was not added` })
  return (
    <Modal opened onClose={onClose} title={`Add ${file.name}`}>
      <Stack gap={SP.cozy}>
        <TextInput label="Name" value={name} onChange={(e) => setName(e.currentTarget.value)} maxLength={120} />
        <TextInput label={exe ? 'Silent switches' : 'MSI properties (optional)'} ff="monospace"
          placeholder={exe ? '/S  or  /quiet /norestart  or  /VERYSILENT' : 'ALLUSERS=1 SERVER=reports.internal'}
          description={exe
            ? "Required: nobody is there to click Next. The vendor's documentation names them."
            : 'Argus runs msiexec /i with /qn /norestart; these are added, NAME=value each.'}
          value={args} onChange={(e) => setArgs(e.currentTarget.value)} />
        <TextInput label="Note (optional)" value={note} onChange={(e) => setNote(e.currentTarget.value)} maxLength={500} />
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button loading={add.isPending} disabled={!name.trim() || (exe && !args.trim())} onClick={save}>Add</Button>
        </Group>
      </Stack>
    </Modal>
  )
}
