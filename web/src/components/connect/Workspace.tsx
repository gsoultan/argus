import '../ssh/ssh.css'
import './connect.css'
import { Suspense, lazy, useCallback, useEffect, useRef, useState } from 'react'
import { ActionIcon, Box, Button, Center, Group, Loader, Menu, Stack, Text, Tooltip, UnstyledButton } from '@mantine/core'
import { useMediaQuery } from '@mantine/hooks'
import { useQuery } from '@tanstack/react-query'
import { useNavigate, useRouterState } from '@tanstack/react-router'
import {
  IconArrowLeft, IconCheck, IconChevronDown, IconCircleCheck, IconClipboard, IconDeviceDesktop, IconDeviceMobile, IconEye, IconFolders, IconKeyboard,
  IconLayoutColumns, IconMaximize, IconMinimize, IconPin, IconPinFilled, IconPlugConnectedX, IconPlus,
  IconTerminal2, IconX,
} from '@tabler/icons-react'
import type { FilesState } from '~/components/FileBrowser'
import type { TerminalState } from '~/components/LiveTerminal'
import type { ClipboardNote, RDPControls, RDPStats } from '~/components/RDPScreen'
import type { ClipboardRule } from '~/lib/clipboard'
import { StateCard } from '~/components/connect/ConnectPanel'
import { chord, isMac, loadKeyboardPrefs, saveKeyboardPrefs, type KeyboardPrefs, type KeyOut } from '~/lib/keymap'
import { assetsQuery, meQuery } from '~/lib/queries'
import { desktopEndpoint, filesEndpoint, terminalEndpoint } from '~/lib/live'
import { seesWholeFleet } from '~/lib/roles'
import { useSessions, type OpenSession, type SessionView } from '~/lib/sessions'
import { FS } from '~/theme'

const LiveTerminal = lazy(() => import('~/components/LiveTerminal').then((m) => ({ default: m.LiveTerminal })))
const RDPScreen = lazy(() => import('~/components/RDPScreen').then((m) => ({ default: m.RDPScreen })))
const FileBrowser = lazy(() => import('~/components/FileBrowser').then((m) => ({ default: m.FileBrowser })))

/** One state for a whole session, whichever view reported it. */
export type PaneState = TerminalState

const WAY_ICON = {
  terminal: IconTerminal2, files: IconFolders, split: IconLayoutColumns,
  desktop: IconDeviceDesktop, screen: IconEye, device: IconDeviceMobile,
} as const

const WAY_LABEL = {
  terminal: 'Terminal', files: 'Files', split: 'Terminal', desktop: 'Desktop', screen: 'Screen', device: 'Device',
} as const

/**
 * Every open session, as tabs, over the whole window.
 *
 * Mounted by the shell for as long as any session is open, and shown only on
 * /connect: on any other page it is still there, hidden, so a desktop keeps
 * its connection and a terminal its shell while you look something up. The
 * console's own header and page chrome give way to a slim tab bar while it is
 * in front, so the remote screen gets the window.
 */
export function Workspace() {
  const { sessions, activeId, focus, close, ask } = useSessions()
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  const navigate = useNavigate()
  const [states, setStates] = useState<Record<string, PaneState>>({})
  const ends = useRef(new Map<string, () => void>())
  const rootRef = useRef<HTMLDivElement>(null)
  const shown = pathname === '/connect' && sessions.length > 0

  const report = useCallback((id: string, s: PaneState) => {
    setStates((m) => (m[id] === s ? m : { ...m, [id]: s }))
  }, [])

  // The console's page scroll would otherwise sit under the workspace.
  useEffect(() => {
    if (!shown) return
    const prev = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    return () => {
      document.body.style.overflow = prev
    }
  }, [shown])

  const endTab = (s: OpenSession) => {
    const st = states[s.id]
    const running = st === 'live' || st === 'reconnecting'
    if (running && !window.confirm(
      `End the session on ${short(s.target)}? Anything still running in it is stopped. To keep it running, switch to another tab or page instead.`,
    )) return
    ends.current.get(s.id)?.()
    ends.current.delete(s.id)
    close(s.id)
  }

  if (sessions.length === 0) return null
  return (
    <div ref={rootRef} className="argus-workspace" hidden={!shown} data-testid="session-workspace">
      <div className="argus-ws-bar" role="tablist" aria-label="Open sessions">
        <UnstyledButton className="argus-ws-back" onClick={() => void navigate({ to: '/assets' })}
          aria-label="Back to assets, sessions keep running">
          <IconArrowLeft size={15} />
          <span>Assets</span>
        </UnstyledButton>
        <div className="argus-ws-tabs">
          {sessions.map((s) => {
            const st = states[s.id] ?? 'connecting'
            const Icon = WAY_ICON[s.way]
            return (
              <div key={s.id} className="argus-ws-tab" data-on={s.id === activeId || undefined}>
                <UnstyledButton role="tab" aria-selected={s.id === activeId} className="argus-ws-tabmain"
                  onClick={() => focus(s.id)} title={`${s.principal} on ${s.target}`}>
                  <span className="argus-ws-dot" data-state={st} />
                  <Icon size={14} />
                  <b>{short(s.target)}</b>
                  <span className="argus-ws-tabway">{WAY_LABEL[s.way]}</span>
                  {s.protocol === 'ssh' && <span className="argus-ws-acct">{s.principal}</span>}
                </UnstyledButton>
                <ActionIcon variant="subtle" color="gray" size="sm" aria-label={`End session on ${short(s.target)}`}
                  onClick={() => endTab(s)}>
                  <IconX size={13} />
                </ActionIcon>
              </div>
            )
          })}
          <Tooltip label="Connect to another">
            <ActionIcon variant="subtle" color="gray" aria-label="New connection" onClick={() => ask()}>
              <IconPlus size={16} />
            </ActionIcon>
          </Tooltip>
        </div>
        <Group gap={6} wrap="nowrap" className="argus-ws-rec" visibleFrom="xs">
          <span className="argus-ws-recdot" />
          <Text size={FS.meta}>Recording</Text>
        </Group>
      </div>
      <div className="argus-ws-stage">
        {sessions.map((s) => (
          <div key={s.id} className="argus-ws-pane" hidden={s.id !== activeId}>
            <SessionPane session={s} active={shown && s.id === activeId} rootRef={rootRef}
              onState={(st) => report(s.id, st)}
              registerEnd={(fn) => {
                if (fn) ends.current.set(s.id, fn)
                else ends.current.delete(s.id)
              }}
              onClose={() => endTab(s)} />
          </div>
        ))}
      </div>
    </div>
  )
}

function short(host: string) {
  return host.split('.')[0] ?? host
}

/** One session: its views, and what to do when it ends or drops. */
function SessionPane({ session, active, rootRef, onState, registerEnd, onClose }: {
  session: OpenSession
  active: boolean
  rootRef: React.RefObject<HTMLDivElement | null>
  onState: (s: PaneState) => void
  registerEnd: (fn: (() => void) | null) => void
  onClose: () => void
}) {
  const { ticketFor, takePassword, reopen } = useSessions()
  const { data: assets } = useQuery(assetsQuery({}))
  const { data: me } = useQuery(meQuery())
  const asset = assets?.find((a) => a.hostname === session.target)
  const phone = useMediaQuery('(max-width: 36em)') ?? false
  const first: SessionView = session.way === 'files' ? 'files' : session.way === 'split' && !phone ? 'split' : 'terminal'
  const [view, setView] = useState<SessionView>(first)
  const [opened, setOpened] = useState<Set<'terminal' | 'files'>>(
    () => new Set(first === 'split' ? ['terminal', 'files'] : [first === 'files' ? 'files' : 'terminal']),
  )
  const [cwd, setCwd] = useState<string | null>(null)
  const [termState, setTermState] = useState<TerminalState>('connecting')
  const [termDetail, setTermDetail] = useState<string>()
  const [filesState, setFilesState] = useState<FilesState>('connecting')
  const [filesDetail, setFilesDetail] = useState<string>()
  const [reopenError, setReopenError] = useState<string>()
  const [reopening, setReopening] = useState(false)
  const endTerminal = useRef<(() => void) | null>(null)

  const desktop = session.protocol === 'rdp'
  const filesFront = !desktop && view === 'files'
  const state: PaneState = filesFront ? filesState : termState
  const detail = filesFront ? filesDetail : termDetail

  useEffect(() => onState(state), [state, onState])
  useEffect(() => {
    registerEnd(() => endTerminal.current?.())
    return () => registerEnd(null)
  }, [registerEnd])

  const getTicket = useCallback(() => ticketFor(session.id), [ticketFor, session.id])

  const show = (v: SessionView) => {
    setView(v)
    setOpened((o) => {
      const next = new Set(o)
      if (v !== 'files') next.add('terminal')
      if (v !== 'terminal') next.add('files')
      return next.size === o.size ? o : next
    })
  }

  const again = async () => {
    setReopening(true)
    setReopenError(undefined)
    const res = await reopen(session.id)
    setReopening(false)
    if (res.error) {
      setReopenError(res.error)
      return
    }
    setTermState('connecting')
    setFilesState('connecting')
  }

  const ended = state === 'closed' || state === 'lost' || state === 'error'
  const whole = seesWholeFleet(me?.role)

  return (
    <Box className="argus-ws-session" data-desktop={desktop || undefined}>
      {!desktop && (
        <TerminalStrip view={view} show={show} phone={phone} onEnd={onClose} />
      )}
      <Box className="argus-ws-body" key={session.attempt}>
        <Suspense fallback={<Loading />}>
          {desktop ? (
            <DesktopView session={session} active={active} rootRef={rootRef}
              getTicket={getTicket} takePassword={() => takePassword(session.id)}
              onEnd={onClose}
              onState={(s, d) => {
                setTermState(s === 'live' ? 'live' : s === 'error' ? 'error' : s === 'connecting' ? 'connecting' : 'closed')
                setTermDetail(d)
              }} />
          ) : (
            <div className={view === 'split' ? 'argus-session-split' : 'argus-session-pane'}>
              {opened.has('terminal') && (
                <div className="argus-session-pane" hidden={view === 'files'}>
                  <LiveTerminal bare endRef={endTerminal} endpoint={terminalEndpoint()} getTicket={getTicket}
                    target={session.target} principal={session.principal} onCwd={setCwd}
                    onStateChange={(s, d) => {
                      setTermState(s)
                      setTermDetail(d)
                    }} />
                </div>
              )}
              {opened.has('files') && (
                <div className="argus-session-pane" hidden={view === 'terminal'}>
                  <FileBrowser endpoint={filesEndpoint()} getTicket={getTicket} target={session.target}
                    principal={session.principal} compact={view === 'split'} follow={view === 'split' ? cwd : null}
                    onStateChange={(s, d) => {
                      setFilesState(s)
                      setFilesDetail(d)
                    }} />
                </div>
              )}
            </div>
          )}
        </Suspense>
        {ended && (
          <div className="argus-ws-over">
            {state === 'closed' ? (
              <StateCard icon={<IconCircleCheck size={26} />} tone="teal" title="Session ended"
                body={`It was recorded. You can find it under ${whole ? 'Activity' : 'My activity'}.`}>
                <Button loading={reopening} onClick={() => void again()}>Connect again</Button>
                <Button variant="default" onClick={onClose}>Close tab</Button>
              </StateCard>
            ) : (
              <StateCard icon={state === 'lost' ? <IconPlugConnectedX size={26} /> : <IconX size={26} />}
                tone={state === 'lost' ? 'amber' : 'rose'}
                title={state === 'lost' ? 'Connection lost' : 'It could not open'}
                body={reopenError ?? `${detail ?? (state === 'lost' ? 'The connection to the server dropped.' : 'The session did not start.')}${state === 'lost' ? ' What ran until then was recorded.' : ''}`}>
                <Button loading={reopening} onClick={() => void again()}>Connect again</Button>
                <Button variant="default" onClick={onClose}>Close tab</Button>
              </StateCard>
            )}
          </div>
        )}
      </Box>
      {!desktop && (
        <div className="argus-ws-status">
          <span data-state={state}>{STATE_WORD[state]}{state !== 'live' && detail ? ` · ${detail}` : ''}</span>
          <span>{session.principal}@{short(session.target)}</span>
          {asset?.os && <span className="argus-ws-hide-phone">{asset.os}</span>}
          <span className="argus-ws-status-r"><Since at={session.openedAt} /></span>
        </div>
      )}
    </Box>
  )
}

const STATE_WORD: Record<PaneState, string> = {
  connecting: 'Connecting', live: 'Live · recorded', reconnecting: 'Reconnecting', lost: 'Connection lost',
  closed: 'Ended', error: 'Failed',
}

function Since({ at }: { at: number }) {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const t = window.setInterval(() => setNow(Date.now()), 30_000)
    return () => window.clearInterval(t)
  }, [])
  const m = Math.max(0, Math.floor((now - at) / 60_000))
  return <>{m < 60 ? `${m} min` : `${Math.floor(m / 60)} h ${m % 60} min`}</>
}

function TerminalStrip({ view, show, phone, onEnd }: {
  view: SessionView
  show: (v: SessionView) => void
  phone: boolean
  onEnd: () => void
}) {
  const tab = (v: SessionView, label: string, Icon: typeof IconTerminal2) => (
    <button type="button" role="tab" className="argus-ws-view" aria-selected={view === v} onClick={() => show(v)}>
      <Icon size={15} />{label}
    </button>
  )
  return (
    <div className="argus-ws-strip" role="tablist" aria-label="Session view">
      {tab('terminal', 'Terminal', IconTerminal2)}
      {tab('files', 'Files', IconFolders)}
      {!phone && tab('split', 'Both', IconLayoutColumns)}
      <Button size="compact-sm" color="rose" ml="auto" leftSection={<IconX size={13} />} onClick={onEnd}>End</Button>
    </div>
  )
}

/** A desktop, a shared screen or a device, with the floating toolbar. */
function DesktopView({ session, active, rootRef, getTicket, takePassword, onState, onEnd }: {
  session: OpenSession
  active: boolean
  rootRef: React.RefObject<HTMLDivElement | null>
  getTicket: () => Promise<string | null>
  takePassword: () => string
  onState: (s: 'connecting' | 'live' | 'closed' | 'error', d?: string) => void
  onEnd: () => void
}) {
  const device = session.way === 'device'
  const readOnly = device && session.principal === 'view'
  const controls = useRef<RDPControls | null>(null)
  const [stats, setStats] = useState<RDPStats | null>(null)
  const [live, setLive] = useState(false)
  const [full, setFull] = useState(false)
  const [pinned, setPinned] = useState(false)
  const [visible, setVisible] = useState(true)
  const [menuOpen, setMenuOpen] = useState(false)
  const hideTimer = useRef<number | undefined>(undefined)

  // The toolbar steps out of the way after a few seconds and comes back when
  // the pointer goes near the top, as in a desktop remote-control app.
  const wake = useCallback(() => {
    setVisible(true)
    window.clearTimeout(hideTimer.current)
    hideTimer.current = window.setTimeout(() => setVisible(false), 3000)
  }, [])
  useEffect(() => {
    wake()
    return () => window.clearTimeout(hideTimer.current)
  }, [wake])

  useEffect(() => {
    const onFs = () => setFull(document.fullscreenElement === rootRef.current && !!rootRef.current)
    document.addEventListener('fullscreenchange', onFs)
    return () => document.removeEventListener('fullscreenchange', onFs)
  }, [rootRef])

  // In full screen the keyboard is the remote's whole: Esc, Alt+Tab, the
  // Windows key and ⌘Tab reach it instead of the browser, where the browser
  // allows (Chrome and Edge). Holding Esc still leaves full screen.
  const toggleFull = () => {
    const kb = (navigator as Navigator & { keyboard?: { lock?: () => Promise<void>; unlock?: () => void } }).keyboard
    if (document.fullscreenElement) {
      kb?.unlock?.()
      void document.exitFullscreen()
    } else {
      void rootRef.current?.requestFullscreen?.()
        .then(() => (!device && !readOnly ? kb?.lock?.() : undefined))
        .catch(() => undefined)
    }
    controls.current?.focus()
  }

  const mac = isMac()
  const [keyboard, setKeyboard] = useState<KeyboardPrefs>(loadKeyboardPrefs)
  const setPref = (p: Partial<KeyboardPrefs>) => {
    const next = { ...keyboard, ...p }
    setKeyboard(next)
    saveKeyboardPrefs(next)
    controls.current?.focus()
  }
  const send = (keys: KeyOut[]) => controls.current?.sendKeys(keys)

  // The clipboard's rule for this server, and a few words when something
  // crossed, shown on the toolbar for a moment.
  const [clipRule, setClipRule] = useState<ClipboardRule | null>(null)
  const [clipSaid, setClipSaid] = useState<string | null>(null)
  const clipTimer = useRef<number | undefined>(undefined)
  useEffect(() => () => window.clearTimeout(clipTimer.current), [])
  const host = session.target.split('.')[0]
  const onClipboard = (n: ClipboardNote) => {
    if (n.kind === 'rule') {
      setClipRule(n.rule)
      return
    }
    const said = {
      copied: n.kind === 'copied' && n.written ? `Copied from ${host}` : `Copied on ${host}: click the screen to take it`,
      sent: `Clipboard sent to ${host}`,
      'too-long': 'Too long to paste here; use Files',
      unreadable: 'Allow clipboard access in the browser to paste',
    }[n.kind]
    setClipSaid(said)
    wake()
    window.clearTimeout(clipTimer.current)
    clipTimer.current = window.setTimeout(() => setClipSaid(null), 3000)
  }
  const CLIP_LABEL: Record<ClipboardRule, string> = {
    both: 'Copy and paste work both ways.',
    to: 'You can paste into this server. What is copied there stays there.',
    from: 'You can copy out of this server. Pasting into it is off.',
    off: 'Copy and paste are off for this server.',
  }

  const rtt = stats?.rttMs
  const quality = rtt === undefined ? 'unknown' : rtt > 300 ? 'poor' : rtt > 150 ? 'fair' : 'good'

  return (
    <Box className="argus-ws-desktop"
      onMouseMove={(e) => {
        const top = (e.currentTarget as HTMLElement).getBoundingClientRect().top
        if (e.clientY - top < 72) wake()
      }}>
      <div className="argus-ws-toolbar" data-hidden={(!visible && !pinned && !menuOpen && live) || undefined}
        role="toolbar" aria-label="Session controls" onMouseEnter={wake}>
        <Tooltip label={stats ? `${stats.fps} fps · ${stats.mbit.toFixed(1)} Mbit/s · ${stats.video ? 'H.264' : 'exact pixels'}` : 'Measuring'}>
          <span className="argus-ws-signal" data-q={quality} data-testid="desktop-signal">
            <i /><i /><i /><i />
            {rtt !== undefined ? `${rtt} ms` : live ? 'Live' : 'Connecting'}
          </span>
        </Tooltip>
        <span className="argus-ws-sep" />
        {!readOnly && !device && (
          <UnstyledButton className="argus-ws-tb" disabled={!live} onClick={() => controls.current?.ctrlAltDel()}>
            Ctrl+Alt+Del
          </UnstyledButton>
        )}
        {!readOnly && !device && (
          // Not in a portal: in full screen only the workspace is on screen.
          <Menu withinPortal={false} position="bottom" shadow="md" width={250}
            onOpen={() => setMenuOpen(true)} onClose={() => { setMenuOpen(false); wake() }}>
            <Menu.Target>
              <UnstyledButton className="argus-ws-tb" aria-label="Keys and keyboard">
                <IconKeyboard size={15} />Keys<IconChevronDown size={12} />
              </UnstyledButton>
            </Menu.Target>
            <Menu.Dropdown>
              <Menu.Label>Send to the server</Menu.Label>
              <Menu.Item disabled={!live} onClick={() => send(chord('MetaLeft'))}>Windows key</Menu.Item>
              <Menu.Item disabled={!live} onClick={() => send(chord('AltLeft', 'Tab'))}>Alt+Tab</Menu.Item>
              <Menu.Item disabled={!live} onClick={() => send(chord('ControlLeft', 'ShiftLeft', 'Escape'))}>
                Task Manager (Ctrl+Shift+Esc)
              </Menu.Item>
              <Menu.Item disabled={!live} onClick={() => send(chord('PrintScreen'))}>Print Screen</Menu.Item>
              <Menu.Item disabled={!live} onClick={() => send(chord('MetaLeft', 'KeyL'))}>Lock (Windows+L)</Menu.Item>
              <Menu.Divider />
              <Menu.Label>This keyboard</Menu.Label>
              {mac && (
                <Menu.Item closeMenuOnClick={false} role="menuitemcheckbox" aria-checked={keyboard.macShortcuts}
                  rightSection={keyboard.macShortcuts ? <IconCheck size={14} /> : null}
                  onClick={() => setPref({ macShortcuts: !keyboard.macShortcuts })}>
                  Mac shortcuts (⌘C copies)
                </Menu.Item>
              )}
              <Menu.Item closeMenuOnClick={false} role="menuitemcheckbox" aria-checked={keyboard.chars}
                rightSection={keyboard.chars ? <IconCheck size={14} /> : null}
                onClick={() => setPref({ chars: !keyboard.chars })}>
                Type characters, not key positions
              </Menu.Item>
              <Text size="xs" c="dimmed" px="sm" pb={6}>
                {keyboard.chars
                  ? 'What is printed on your keys arrives, whatever layout the server has.'
                  : 'Keys go by position, and the server\'s own layout applies, as on a PC there.'}
              </Text>
            </Menu.Dropdown>
          </Menu>
        )}
        {readOnly && <span className="argus-ws-tb argus-ws-ro">Watch only</span>}
        {!readOnly && !device && (
          <Menu withinPortal={false} position="bottom" shadow="md" width={270}
            onOpen={() => setMenuOpen(true)} onClose={() => { setMenuOpen(false); wake() }}>
            <Menu.Target>
              <UnstyledButton className="argus-ws-tb" aria-label="Clipboard">
                <IconClipboard size={15} />{clipSaid ?? 'Clipboard'}<IconChevronDown size={12} />
              </UnstyledButton>
            </Menu.Target>
            <Menu.Dropdown>
              <Text size="xs" c="dimmed" px="sm" py={6}>
                {!clipRule ? 'Waiting for the server.'
                  : session.way === 'screen' && clipRule !== 'off' && clipRule !== 'from'
                    ? 'Screen sharing has no shared clipboard yet. Paste as typing types what you copied.'
                    : session.way === 'screen' ? 'Pasting into this computer is off.' : CLIP_LABEL[clipRule]}
              </Text>
              {session.way !== 'screen' && (clipRule === 'both' || clipRule === 'to') && (
                <Menu.Item disabled={!live} onClick={() => void controls.current?.sendClipboard()}>
                  Send my clipboard to {host}
                </Menu.Item>
              )}
              {(clipRule === 'both' || clipRule === 'to') && (
                <Menu.Item disabled={!live} onClick={() => void controls.current?.pasteAsTyping()}>
                  Paste as typing
                </Menu.Item>
              )}
              {(clipRule === 'both' || clipRule === 'to') && (
                <Text size="xs" c="dimmed" px="sm" pb={6}>
                  {session.way === 'screen'
                    ? 'For a sign-in box or anywhere paste does not work.'
                    : `${mac ? '⌘V' : 'Ctrl+V'} on the screen pastes what you copied. Paste as typing is for boxes that refuse paste, such as the Windows sign-in.`}
                </Text>
              )}
            </Menu.Dropdown>
          </Menu>
        )}
        <UnstyledButton className="argus-ws-tb" onClick={toggleFull}>
          {full ? <IconMinimize size={15} /> : <IconMaximize size={15} />}{full ? 'Exit full screen' : 'Full screen'}
        </UnstyledButton>
        <UnstyledButton className="argus-ws-tb argus-ws-end" onClick={onEnd}>
          <IconX size={14} />End
        </UnstyledButton>
        <Tooltip label={pinned ? 'Let it hide' : 'Keep it showing'}>
          <ActionIcon variant="subtle" color="gray" size="sm" aria-label={pinned ? 'Unpin toolbar' : 'Pin toolbar'}
            aria-pressed={pinned} onClick={() => setPinned((p) => !p)}>
            {pinned ? <IconPinFilled size={14} /> : <IconPin size={14} />}
          </ActionIcon>
        </Tooltip>
      </div>
      <RDPScreen
        bare
        endpoint={desktopEndpoint()}
        target={session.target}
        principal={session.principal}
        device={device}
        readOnly={readOnly}
        getTicket={getTicket}
        takePassword={takePassword}
        controls={controls}
        keyboard={keyboard}
        onClipboard={onClipboard}
        clipboardSync={session.way !== 'screen'}
        onStats={setStats}
        onStateChange={(s, d) => {
          setLive(s === 'live')
          onState(s, d)
        }}
      />
      {/* Only the tab in front takes the keyboard. */}
      {active && <FocusOnShow controls={controls} />}
    </Box>
  )
}

function FocusOnShow({ controls }: { controls: React.RefObject<RDPControls | null> }) {
  useEffect(() => {
    controls.current?.focus()
  }, [controls])
  return null
}

function Loading() {
  return (
    <Center h="100%" mih={220}>
      <Stack align="center" gap="xs">
        <Loader size="sm" color="azure" />
        <Text size={FS.meta} c="dimmed">Loading…</Text>
      </Stack>
    </Center>
  )
}
