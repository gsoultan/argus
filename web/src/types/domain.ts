/**
 * Argus domain model.
 *
 * This is the wire contract the Go control plane will implement. Everything the
 * UI renders is derived from these shapes — if a field isn't here, the UI can't
 * claim it.
 */

export type UUID = string
export type ISOTime = string

/* ── Identity ────────────────────────────────────────────────────────────── */

export type UserRole = 'owner' | 'admin' | 'approver' | 'operator' | 'auditor'

export interface User {
  id: UUID
  email: string
  displayName: string
  role: UserRole
  mfaEnrolled: boolean
  lastSeenAt: ISOTime | null
  /** The organization the session is in; absent on the fixture. */
  org?: { id: string; slug: string; name: string; default: boolean }
  /** Whether this account may manage organizations; see Identity.platformOwner. */
  platformOwner?: boolean
  /** Organizations the signed-in person can switch to; see Identity.orgs. */
  orgs?: Array<{ id: string; slug: string; name: string; default: boolean; role: string; home: boolean }>
  /** True when more than one organization shares this control plane. */
  multiOrg?: boolean
  /** They sign in through another organization; their password is not held here. */
  shared?: boolean
  /** One of ours whom another organization may add without a new password. */
  shareable?: boolean
  /**
   * Revoked rather than deleted. The audit log refers to people by address, so
   * an account is disabled and kept — and a disabled one must not be offered as
   * somewhere to assign a host.
   */
  disabled?: boolean
  /** When they were removed, and which admin removed them. */
  disabledAt?: ISOTime | null
  disabledBy?: string
  /** Their personal account on servers; empty until a team first needs one. */
  loginName?: string
  /** Whether they have added a public key to SSH directly with. */
  hasSshKey?: boolean
  /** They have not chosen a password of their own yet; the one they were sent is temporary. */
  temporaryPassword?: boolean
  /** When the temporary password they were sent stops working. */
  temporaryPasswordExpiresAt?: ISOTime | null
  /** Whether a person added now can be emailed their first password (signed-in user only). */
  canEmailPeople?: boolean
}

/**
 * What adding a person, or sending them a new temporary password, did: it was
 * emailed to them, or here it is to hand over, shown once.
 */
export interface TemporaryPasswordOutcome {
  email: string
  role?: string
  temporaryPasswordExpiresAt: ISOTime
  emailed: boolean
  /** Why it could not be emailed, when sending was tried and failed. */
  emailError?: string
  /** Present only when it was not emailed. */
  temporaryPassword?: string
}

/** The signed-in person's own server account. */
export interface ServerAccount {
  loginName: string
  sshKey: string
  sshFingerprint: string
  /** Whether any team of theirs may SSH straight to a server. */
  directSsh: boolean
}

/**
 * What the signed-in person may do on one server with their own account:
 * whether they have one there, and whether they may SSH straight to it.
 */
export interface MyServer {
  hostname: string
  personalAccount: boolean
  sudo: boolean
  directSsh: TeamDirectSsh
}

/** What the agent last said about one personal account on a server. */
export interface PersonalAccountState {
  name: string
  owner?: string
  state: 'ready' | 'locked' | 'taken' | 'failed' | 'deleted'
  detail?: string
}

/** The owner the agent reports for a login account an admin made from the console. */
export const SHARED_LOGIN_OWNER = 'shared login'

/**
 * A login account an admin made on a server from the console. `state` is
 * what Argus asked for; `agent` is what the server's agent last said it did.
 */
export interface ServerLogin {
  name: string
  state: 'active' | 'locked' | 'delete' | 'deleted'
  createdBy: string
  createdAt: ISOTime
  changedBy: string
  changedAt: ISOTime
  agent: '' | PersonalAccountState['state']
  agentDetail?: string
}

/**
 * Whether a team may SSH straight to its personal accounts on a server:
 * not at all, once an admin approves each login, or let in. Recorded either way.
 */
export type TeamDirectSsh = 'off' | 'approve' | 'allow'

/** What a team is given on one server. */
export interface TeamAccess {
  principals: string[]
  personalAccounts: boolean
  sudo: boolean
  directSsh: TeamDirectSsh
}

/* ── Assets ──────────────────────────────────────────────────────────────── */

/**
 * How Argus authenticates *to* the target.
 *
 * `injected-*` means the gateway holds the secret and the user never sees it.
 * `ca-certificate` means no standing credential exists at all — the gateway
 * mints a short-lived cert per session. It requires TrustedUserCAKeys on the
 * host, which is why it's opt-in per asset rather than global.
 */
export type CredentialMode = 'injected-key' | 'injected-password' | 'ca-certificate'

/**
 * Host key state. Argus is a deliberate MITM, so *it* is responsible for
 * verifying the target's identity. An unpinned host is a host nobody has
 * vouched for yet.
 */
export type HostKeyState = 'pinned' | 'unpinned' | 'changed'

export type AssetHealth = 'reachable' | 'degraded' | 'unreachable'

/**
 * Host agent state. The agent is what makes direct-to-port-22 sessions
 * visible, so its absence is a coverage gap, not a cosmetic detail.
 *
 * `stale` means it stopped reporting. Treat that as hostile until proven
 * otherwise: a user with root can kill the agent, and silence is exactly what
 * that looks like.
 */
export type AgentState = 'healthy' | 'stale' | 'absent'

/**
 * Whether the host can still be reached without going through Argus.
 *
 * `enforced`  — no standing keys, CA-only trust, port 22 firewalled to the
 *               gateway. A direct session is not possible.
 * `monitored` — bypass is possible but the agent will record it.
 * `open`      — bypass is possible and would NOT be recorded. This is the gap.
 */
export type BypassPosture = 'enforced' | 'monitored' | 'open'

/** Which protocol reaches an asset. Absent means SSH. */
export type AssetProtocol = 'ssh' | 'rdp'

export interface Asset {
  /** Absent means SSH, matching the gateway's own default. */
  protocol?: AssetProtocol
  id: UUID
  hostname: string
  /**
   * The name people know it by, shown in place of the hostname; empty or
   * absent shows the hostname. A label only: sessions and the audit chain
   * name the hostname. Read it through assetName().
   */
  alias?: string
  address: string
  port: number
  os: string
  tags: string[]
  groupId: UUID
  credentialMode: CredentialMode
  hostKeyState: HostKeyState
  /** SHA256 fingerprint of the pinned host key, base64, OpenSSH format. */
  hostKeyFingerprint: string | null
  hostKeyPinnedAt: ISOTime | null
  health: AssetHealth
  lastCheckedAt: ISOTime
  agentState: AgentState
  /** Null when no agent has ever reported. */
  agentLastSeenAt: ISOTime | null
  bypassPosture: BypassPosture
  /** Keys in authorized_keys that Argus did not issue — each is a way in. */
  unmanagedKeyCount: number
  /**
   * Accounts on the target that Argus can broker a session as.
   *
   * An administrator sees the host's whole list. Everyone else sees the
   * accounts they were assigned — the control plane narrows it before sending
   * it, so this is what the person can actually use rather than what exists.
   */
  principals: string[]
  /**
   * Who owns this row: `console` if an administrator entered or edited it here,
   * `gateway` if it arrived from an inventory file. A gateway publish never
   * overwrites a console-owned asset.
   */
  source?: 'console' | 'gateway'
  /**
   * The credential the gateway injects, named rather than pathed: it is
   * resolved inside the gateway's own vault directory. Empty for
   * `ca-certificate`, where there is no standing credential at all.
   */
  credentialRef?: string
  /** Windows domain for an RDP asset. Empty means the account is local. */
  domain?: string
  /**
   * How a Windows server's desktop opens: `rdp` through its own Remote
   * Desktop, as the account, with its password; `screen` by sharing the
   * console as it is, without Remote Desktop. Absent from an older control
   * plane, which is `rdp`.
   */
  desktopMode?: DesktopMode
  /**
   * What this row is: a `server`, a `workstation` (a laptop or PC someone
   * uses, stored and opened exactly as a server is), or an `android` device
   * driven through the Argus app on it. A device opens the way a shared Windows console does
   * (protocol `rdp`, desktop mode `screen`) and has no accounts: its
   * principals are `view` and `control`. Absent from an older control
   * plane, which is `server`.
   */
  kind?: AssetKind
  /** How a device is controlled; see ControlMode. Devices only. */
  controlMode?: ControlMode
  /**
   * Whether `control` on this device needs an approval first, the way root
   * does on a server. `view` never does.
   */
  controlNeedsApproval?: boolean
  /** Who is asked before connecting: null follows the type's rule. */
  connectAsk?: 'never' | 'ask' | 'tell' | null
  /** Null when credentialMode is 'ca-certificate' — nothing to rotate. */
  credentialRotatedAt: ISOTime | null
  rotationIntervalDays: number | null
  /**
   * Whether the server's agent holds a live tunnel to the control plane, so
   * a session can open through it. Absent from an older control plane.
   */
  tunnelConnected?: boolean
  /**
   * The name the server's agent goes by when it is not the hostname: the one
   * it had before it was renamed in Argus, or the short name a discovered
   * server's agent reports. Sessions still reach it under that name.
   */
  agentHostname?: string
  /**
   * The server argus-control itself runs on, looked after through the helper
   * its package ships rather than an agent someone installed. It updates with
   * argus-control, cannot be removed from Argus, and plain SSH to it is never
   * gated: that is the way back in when Argus is down. Absent from an older
   * control plane.
   */
  controlHost?: boolean
  /**
   * Whether the agent said, at its last check-in, that it refuses logins that
   * did not come through Argus. Null until it says: unconfirmed, never applied.
   */
  agentConsoleOnly?: boolean | null
  /** This server's own choice for direct SSH logins. Null follows Settings. */
  directSsh?: DirectSsh | null
  /** What the agent said at its last check-in it is applying. Null is unconfirmed. */
  agentDirectSsh?: DirectSsh | null
  /**
   * Login accounts on this server that do something other than `directSsh`,
   * each with its own mode. An account not named follows the server.
   */
  directSshAccounts?: Record<string, DirectSsh>
  /** What the agent said about the personal accounts it keeps. Admins only. */
  agentPersonalAccounts?: PersonalAccountState[]
  /**
   * The addresses the server's agent last reported, each once, in interface
   * order. Only for those who see the whole fleet; absent for everyone else,
   * from an older control plane, and when the agent reported none.
   */
  addresses?: AssetAddress[]
}

/** One address a server's agent reported, as the assets list carries it. */
export interface AssetAddress {
  address: string
  family: 'ipv4' | 'ipv6'
  /** Globally routable: not private, carrier NAT or reserved. */
  public: boolean
  /** Absent for an older agent's flat list. */
  interface?: string
}

/**
 * What an agent does with a login that reached sshd without the console:
 * refuse it, hold it until someone approves it, or let it in and record it.
 */
export type DirectSsh = 'refuse' | 'approve' | 'allow'

/** A direct SSH login held for approval. */
export interface DirectLogin {
  id: UUID
  hostname: string
  principal: string
  clientAddr: string
  command: string
  state: 'pending' | 'approved' | 'denied' | 'expired'
  createdAt: ISOTime
  expiresAt: ISOTime
  decidedAt: ISOTime | null
  decidedByEmail: string | null
  decisionNote: string | null
  /**
   * How it signed in: 'publickey', 'password', ... Empty when the server's
   * sshd does not say (ExposeAuthInfo off) or the agent is older than this.
   */
  authMethod?: string
  /** The key a key login used. */
  keyFingerprint?: string
  keyType?: string
  /** The key's comment in authorized_keys; set only when the agent found it there. */
  keyComment?: string
  /** Whether the agent found that key in the account's authorized_keys. */
  keyFound?: boolean
}

/** One account with a login shell on a server, as its agent last reported it. */
export interface ServerLoginAccount {
  user: string
  /** 'set', 'none', 'locked', or 'unknown' when the agent could not read shadow. Never the hash. */
  password: 'set' | 'none' | 'locked' | 'unknown'
  /** Keys in its authorized_keys, Argus's or not. */
  keys: number
  /** Whether sshd as configured would let it in with either. */
  canLogin: boolean
}

/** Who can log in to a server directly, from its agent's last check-in. */
/** One account on a server's Login accounts list, as the request form offers it. */
export interface RequestableAccount {
  name: string
  /** root and the like: approved per request, never held standing. */
  elevated: boolean
  /** False when the server's agent reported its accounts and this was not among them. */
  onServer: boolean
}

/** What one server lets a person ask for. */
export interface RequestableHost {
  hostname: string
  /** False when no server in the inventory has that name: the approver decides. */
  known: boolean
  /** Whether `onServer` was checked against the agent's last report. */
  agentReported: boolean
  accounts: RequestableAccount[]
}

export interface ServerLoginAccounts {
  /** False when the agent has not sent the list: too old, or not checked in. */
  reported: boolean
  checkedAt: ISOTime | null
  accounts: ServerLoginAccount[]
  passwords: boolean
  keys: boolean
  /** Whether sshd tells the agent how each login signed in. */
  exposeAuthInfo: boolean
}

/** An address something on a server listens on. */
export interface ServerAppListen {
  address: string
  port: number
  /** A loopback address: nothing off the server can connect. */
  localOnly: boolean
  process?: string
  unit?: string
  kind?: string
}

/** One known application the agent found on a server. */
export interface ServerApp {
  /** Catalogue key: postgres, redis, rabbitmq, nginx, docker... */
  kind: string
  name: string
  version: string
  running: boolean
  listen: ServerAppListen[]
  process: string
  unit: string
  package: string
  /** Set when it runs in a container. */
  container: string
  image: string
  /** How it was found: package, unit, process, container. */
  evidence: string[]
}

/** One container on a server. */
export interface ServerAppContainer {
  name: string
  image: string
  state: string
  runtime: string
  ports: ServerAppListen[]
  kind: string
}

/** What runs on a server, from its agent's last report. */
export interface ServerApplications {
  /** False when the agent has not sent it: too old, or not checked in. */
  reported: boolean
  checkedAt: ISOTime | null
  applications: ServerApp[]
  listeners: ServerAppListen[]
  containers: ServerAppContainer[]
  services: string[]
  /** What the agent could read: sockets, systemd, dpkg/rpm/apk, docker/podman. */
  sources: string[]
}

/** One app installed on an Android device. */
export interface ServerAndroidApp {
  /** The application id, com.example.app. */
  package: string
  /** What the launcher shows. */
  label: string
  /** The versionName the package reports. */
  version: string
  /** Where it came from: 'play', 'preinstalled' or 'sideloaded' (or ''). */
  source: string
  /** A preinstalled app disabled for the device's user: restorable. */
  disabled?: boolean
}

/** What is installed on an Android device, from the app's last report. */
export interface ServerAndroidApps {
  /** False when the app has not sent a list: too old, or not checked in. */
  reported: boolean
  checkedAt: ISOTime | null
  apps: ServerAndroidApp[]
}

/**
 * One point on a device replay's timeline: something the device did, chained
 * into the recording next to the screen. `ms` is milliseconds into the
 * session.
 */
export interface ReplayActivity {
  ms: number
  /** 'app' | 'lock' | 'unlock' | 'secure' | 'typed' | 'consent' | 'ended'. */
  kind: string
  /** The foreground app, for 'app'. */
  package?: string
  title?: string
  /** Whether secure content is now hidden, for 'secure'. */
  hidden?: boolean
  /** What was typed, for 'typed' and never for a password. */
  text?: string
  /** Whether the field was a password, for 'typed'. */
  password?: boolean
  /** Who, for 'consent' and 'ended'. */
  by?: string
}

/** What kind of machine a server is, from its agent's host facts. */
export interface ServerSystemFacts {
  kernel: string
  arch: string
  cpuModel: string
  /** Logical processors. */
  cpus: number
  memoryBytes: number
  /** '' when the agent could not tell, which is not the same as physical. */
  machine: 'physical' | 'virtual' | 'container' | ''
  hypervisor: string
  bootedAt: ISOTime | null
  timeZone: string
  utcOffsetSeconds: number
  /**
   * How an Android device is driven: 'attended' (someone accepts each
   * session) or 'unattended' (the helper over local ADB, nobody there).
   * Absent for a server, or an app older than the unattended engine.
   */
  engine?: 'attended' | 'unattended'
  /**
   * Whether wireless debugging is on, which the unattended engine needs and
   * which listens on every interface on Android 11+. Shown on the device so
   * nobody finds it by surprise.
   */
  wirelessDebugging?: boolean
  /** Whether the Argus app may install apps ("Install unknown apps"); absent from a server or an older app. */
  canInstallApps?: boolean
}

export interface ServerDisk {
  mount: string
  fs: string
  total: number
  used: number
  /** What a normal writer can still use; less than total − used by the root reserve. */
  avail: number
}

/** How busy a server was at usageAt. */
export interface ServerUsage {
  /** Null where the system keeps no load average (Windows). */
  load: [number, number, number] | null
  /** Percent of processor time not idle; null on an agent's first sample. */
  cpuBusy: number | null
  memTotal: number
  memAvailable: number
  swapTotal: number
  swapFree: number
  disks: ServerDisk[]
  /** Each interface's traffic over the last sample. Missing from an older control plane. */
  net?: ServerNetRate[]
}

/** One interface's traffic, averaged over the agent's last sample (about a minute). */
export interface ServerNetRate {
  name: string
  /** Bytes per second; null when the agent had no earlier reading to measure against. */
  rxBps: number | null
  txBps: number | null
  /** The link speed the machine states, 0 when it states none. */
  speedMbps: number
}

/** A resource running low, decided by the control plane's one rule. */
export interface ResourceWarning {
  resource: 'disk' | 'memory' | 'swap' | 'cpu'
  level: 'low' | 'critical'
  /** The disk's mount point or drive; absent for the others. */
  mount?: string
  /** How full (disk, swap), how busy (cpu), or for memory how much is left. */
  percent: number
  /** Bytes left and in all; 0 for the processor. */
  left: number
  total: number
}

/** A server's System card. */
export interface ServerSystem {
  /** False when no agent has sent system details: too old, or none. */
  reported: boolean
  os: string
  system: ServerSystemFacts | null
  usage: ServerUsage | null
  usageAt: ISOTime | null
  /** Each resource running low in usage. Missing from an older control plane. */
  warnings?: ResourceWarning[]
  /**
   * The addresses the server's network interfaces hold. An agent older than
   * per-interface reporting gives one entry with no name. Missing from a
   * control plane older than this field.
   */
  network?: ServerInterface[]
}

/** One network interface on a server, as its agent reports it. */
export interface ServerInterface {
  /** Empty for an older agent's flat list of addresses. */
  name: string
  mac: string
  addresses: ServerAddress[]
}

export interface ServerAddress {
  address: string
  /** Network length in bits; -1 when the agent did not say. */
  prefix: number
  family: 'ipv4' | 'ipv6'
  /** Globally routable: not private, carrier NAT or reserved. */
  public: boolean
}

/**
 * One person's access to one asset.
 *
 * Explicit rather than computed from a group: the question an auditor asks is
 * "who can reach pay-01 as ops", and it should have one answer rather than a
 * union to reconstruct.
 */
export interface AssetAssignment {
  assetId: UUID
  hostname: string
  userEmail: string
  /** The accounts this person may assume there. Empty is not a state: it is a removal. */
  principals: string[]
  /**
   * Their own setting for their personal account here, which replaces their
   * teams' on this server: true with sudo and direct SSH as below, false for
   * none here. Null or absent: their teams decide.
   */
  personalAccounts?: boolean | null
  sudo?: boolean
  directSsh?: TeamDirectSsh
  grantedBy: string
  grantedAt: ISOTime
}

/** A person's own setting for their personal account on one server. */
export interface PersonOwnSetting {
  personalAccounts: boolean
  sudo: boolean
  directSsh: TeamDirectSsh
}

/** What one person may do on one server, and where it comes from. */
export interface PersonServer {
  assetId: UUID
  hostname: string
  principals: string[]
  personalAccount: boolean
  sudo: boolean
  directSsh: TeamDirectSsh
  /** "own" for their own setting, "teams" for their teams', "" for no own account. */
  source: 'own' | 'teams' | ''
  teams: string[]
  /** An admin gave them a setting of their own here, even one with no account. */
  ownSetting: boolean
}

/** One team's access to one asset. Same rules as a person's. */
export interface TeamAssignment {
  assetId: UUID
  hostname: string
  teamId: UUID
  teamName: string
  principals: string[]
  /** Each member has an account of their own on the server. */
  personalAccounts?: boolean
  /** Those accounts run anything as root without a password. */
  sudo?: boolean
  directSsh?: TeamDirectSsh
  grantedBy: string
  grantedAt: ISOTime
}

/**
 * A named group of people. A server given to a team is given to everyone in
 * it, and leaving the team is the whole revocation.
 */
export interface Team {
  id: UUID
  name: string
  members: string[]
  servers: TeamAssignment[]
  createdBy: string
  createdAt: ISOTime
}

/**
 * What the console submits to create or edit an asset.
 *
 * Deliberately not `Asset`. Host-key state, agent liveness, posture and
 * unmanaged key counts are observed by something that looked; letting the
 * console write them would let an administrator declare a host verified rather
 * than have it verified.
 */
export interface AssetInput {
  hostname: string
  /** Free text shown in place of the hostname; empty for none. */
  alias?: string
  address: string
  port?: number
  protocol?: AssetProtocol
  os?: string
  tags?: string[]
  groupName?: string
  principals: string[]
  credentialMode?: CredentialMode
  credentialRef?: string
  domain?: string
  desktopMode?: DesktopMode
  kind?: AssetKind
  controlMode?: ControlMode
  controlNeedsApproval?: boolean
  /** Null follows the type's rule in Settings › Asset types. */
  connectAsk?: 'never' | 'ask' | 'tell' | null
  rotationIntervalDays?: number | null
}

/** How a Windows server's desktop opens; see Asset.desktopMode. */
export type DesktopMode = 'rdp' | 'screen'

/** What an asset is; see Asset.kind. */
export type AssetKind = 'server' | 'workstation' | 'android'

/**
 * How an Android device is controlled: `attended`, where whoever holds it
 * accepts each session on the device and sees that it is live; or
 * `unattended`, where the app's helper opens it with nobody there.
 */
export type ControlMode = 'attended' | 'unattended'

/** The two principals a device has: watch only, or drive it. */
export const DEVICE_PRINCIPALS = ['view', 'control'] as const

export interface AssetGroup {
  id: UUID
  name: string
  description: string
  assetCount: number
}

/* ── Sessions ────────────────────────────────────────────────────────────── */

export type SessionState = 'active' | 'closed' | 'terminated' | 'rejected'
export type SessionProtocol = 'ssh' | 'sftp' | 'rdp' | 'postgres'

/**
 * How the session reached the host.
 *
 * `brokered` went through argus-gateway: policy was evaluated before the
 * connection existed, and a credential was injected the user never saw.
 *
 * `direct` did not. Someone connected to sshd on port 22 with a standing
 * credential and the host agent captured it after the fact. The recording is
 * just as complete, but NO policy was enforced — no approval, no time window,
 * no principal restriction. Every direct session is a control failure, and the
 * UI treats it as one even when the user was authorised.
 */
export type SessionOrigin = 'brokered' | 'direct'

/**
 * Recording fidelity, surfaced honestly in the UI.
 *
 * `pty` is a replay of what the terminal showed. It is an audit aid — a user
 * can obscure intent with base64, or run a script whose contents never appear
 * on screen. `ebpf` additionally carries kernel-observed execve/connect events
 * and is the tier that survives an adversarial auditor.
 */
/**
 * How much of a session the recording can actually evidence.
 *
 * 'rdp' is its own value rather than folded into 'pty': a desktop recording
 * shows what was on screen and carries no command list at all, so treating it
 * as a terminal capture would promise a timeline that does not exist.
 */
export type RecordingFidelity = 'pty' | 'ebpf' | 'rdp' | 'statement' | 'none'

export interface Session {
  id: UUID
  userId: UUID
  userEmail: string
  assetId: UUID
  assetHostname: string
  principal: string
  protocol: SessionProtocol
  origin: SessionOrigin
  state: SessionState
  startedAt: ISOTime
  endedAt: ISOTime | null
  clientIp: string
  fidelity: RecordingFidelity
  /** Bytes of recorded PTY output. */
  recordingBytes: number
  /** Populated for ebpf fidelity; the count of observed execve events. */
  commandCount: number | null
  /** Set when this session was unlocked by an approved access request. */
  accessRequestId: UUID | null
  /** Head of the recording's hash chain, hex SHA-256. */
  chainHead: string | null
  /**
   * Where the artefact lives in object storage. Null means it does not.
   *
   * A sealed recording with no key is not lost -- the gateway that produced it
   * still holds the file and queues the upload for retry -- but it cannot be
   * opened from here until that lands. 44 sessions in the dev control plane are
   * in that state, and the list offered Replay on every one of them.
   *
   * The control plane has always sent this; the console simply had no field to
   * put it in.
   */
  recordingKey: string | null
  riskFlags: RiskFlag[]
  /** Who stopped this session, when state is 'terminated'. */
  terminatedBy?: string | null
  /** Why it was stopped. An auditor reading a terminated session asks this
   *  first, and the answer has to come from the record rather than from
   *  whoever remembers. */
  terminationReason?: string | null

  /** When a reporter last said this session existed. */
  lastReportedAt: ISOTime
  /**
   * The session is `active` and nothing has spoken for it in minutes.
   *
   * A gateway killed rather than drained never reports the end, so `state`
   * alone could not tell a session still running from one whose gateway is
   * gone -- fifteen showed as live in dev, the oldest for nineteen days, and
   * every duration on the page counted up against them. Nothing reaps them:
   * a gateway carries no identity to attribute orphans to, and with no
   * maximum session duration, age alone proves nothing. So the console says
   * it does not know, rather than saying "live" and being wrong.
   *
   * Derived by the control plane, not here: the comparison is against the
   * database clock and a browser's may be anything at all.
   */
  silent: boolean
  /**
   * The end was deduced, not observed.
   *
   * True for a session the control plane closed after a day of silence. Its
   * `endedAt` is the last moment it was seen alive, not a reported end -- so
   * it ran at least that long and probably longer, and how much longer is not
   * knowable. Without this a closed session with an end time reads as a clean
   * logout, and an auditor could not tell the two apart.
   */
  endInferred: boolean
}

export type RiskFlag =
  | 'bypassed-gateway'
  | 'off-hours'
  | 'new-asset-for-user'
  | 'root-principal'
  | 'unpinned-host-key'
  | 'break-glass'
  | 'bulk-file-transfer'
  | 'kernel-evidence-waived'
  | 'terminated'
  | 'recording-incomplete'
  | 'no-credential-injection'
  | 'legacy-credssp-binding'
  | 'no-network-level-auth'
  | 'graphics-pipeline'

/* ── Access requests (JIT) ───────────────────────────────────────────────── */

/** cancelled: the person who asked took it back before anyone decided. */
export type RequestState = 'pending' | 'approved' | 'denied' | 'expired' | 'revoked' | 'cancelled'

export interface AccessRequest {
  id: UUID
  requesterId: UUID
  requesterEmail: string
  assetIds: UUID[]
  assetHostnames: string[]
  principal: string
  justification: string
  /** Requested window in minutes. Policy caps this per role. */
  durationMinutes: number
  state: RequestState
  createdAt: ISOTime
  decidedAt: ISOTime | null
  decidedByEmail: string | null
  decisionNote: string | null
  /** When an approved grant stops working. */
  expiresAt: ISOTime | null
  breakGlass: boolean
  /** Set when the request stands for restoring a database in place. */
  restoreId?: string
  /** Set when the request is to run statements on a database whose changes need approval. */
  statement?: StatementAsk
  /** Set on a change the reader may decide as an editor of its database. */
  asEditor?: boolean
  /** Set when the request is for a message broker (RabbitMQ, Kafka), not a database. */
  broker?: boolean
}

/** One statement of an ask to run statements on a database whose changes need approval. */
export interface StatementItem {
  sql: string
  params?: (string | null)[]
  /** Rows the dry run counted when it was asked for: the most it may change once approved. */
  rows?: number
  used?: boolean
}

/** What a request to run statements on a database whose changes need approval is for. */
export interface StatementAsk {
  requestId: string
  databaseId: string
  databaseName: string
  databaseLabel?: DBLabel
  items: StatementItem[]
  summary?: Record<string, unknown>
  /** pending, approved, denied, cancelled or expired. */
  state: string
  expiresAt?: string
  decidedBy?: string | null
  note?: string | null
}

/* ── Audit ───────────────────────────────────────────────────────────────── */

export type AuditAction =
  | 'session.start'
  | 'session.end'
  | 'session.terminate'
  | 'session.command'
  | 'file.upload'
  | 'file.download'
  | 'files.opened'
  | 'session.direct_refused'
  | 'request.create'
  | 'request.approve'
  | 'request.deny'
  | 'credential.rotate'
  | 'hostkey.pin'
  | 'hostkey.mismatch'
  | 'session.shadow'
  | 'asset.enrolled'
  | 'asset.dismissed'
  | 'session.direct_detected'
  | 'agent.went_silent'
  | 'sshd_config.drift'
  | 'unmanaged_key.found'
  | 'unmanaged_key.revoked'
  | 'policy.change'
  | 'auth.login'
  | 'auth.mfa_fail'

export type AuditSeverity = 'info' | 'notice' | 'warning' | 'critical'

/**
 * One link in the tamper-evident chain.
 *
 * `hash = SHA-256(prevHash || canonicalJSON(payload))`. The UI recomputes this
 * client-side in a worker so an operator can verify integrity without trusting
 * the server that served the log.
 */
export interface AuditEvent {
  seq: number
  id: UUID
  at: ISOTime
  action: AuditAction
  severity: AuditSeverity
  actorEmail: string
  target: string
  detail: string
  prevHash: string
  hash: string
}

/* ── Dashboard ───────────────────────────────────────────────────────────── */

export interface FleetStats {
  assetsTotal: number
  assetsUnreachable: number
  hostKeysUnpinned: number
  /** Sessions a reporter still speaks for. Excludes `sessionsSilent`. */
  sessionsActive: number
  /** Active on paper, unheard from for minutes. See Session.silent. */
  sessionsSilent: number
  sessionsToday: number
  requestsPending: number
  credentialsOverdue: number
  /** Assets still on standing credentials rather than the CA path. */
  standingCredentialAssets: number
  /** Sessions in the last 24h that did not go through the gateway. */
  sessionsDirectToday: number
  /** Assets where a bypass would go completely unrecorded. */
  assetsUnmonitored: number
  /** Agents that have stopped reporting. */
  agentsStale: number
}

/**
 * A host running an agent that the inventory has no entry for.
 *
 * The inventory is hand-written, which makes "Argus covers all privileged
 * access" a claim rather than something anyone can check. A host reporting from
 * outside it is a privileged machine Argus is not managing, and the whole point
 * of surfacing it is that nobody had to remember it existed.
 */
export interface DiscoveredHost {
  hostname: string
  fqdn?: string
  machineId?: string
  os?: string
  addresses: string[]
  /** More than one is worth reading: a port the inventory misses is an unmonitored way in. */
  sshPorts: number[]
  /** Candidate principals. Discovery reports which accounts exist; it grants nothing. */
  accounts: string[]
  version: string
  firstSeenAt: ISOTime
  lastSeenAt: ISOTime
  state: 'unreviewed' | 'enrolled' | 'ignored'
  reviewNote?: string
  reviewedBy?: string
  reviewedAt?: ISOTime
}

/**
 * Both directions of the coverage question, which are separate gaps.
 *
 * An asset with no agent is a host where a direct connection to port 22 leaves
 * no trace. An agent with no asset is a host Argus is not managing at all.
 * Reporting only the first would look complete while missing whole machines.
 */
export interface Coverage {
  assets: number
  assetsWithAgent: number
  assetsAgentStale: number
  assetsUnmonitored: number
  unreviewedHosts: number
  ignoredHosts: number
  sshAssets: number
  rdpAssets: number
  /**
   * Remote Desktop assets that cannot be covered yet because no Windows agent
   * exists. A known limit of the product rather than a deployment mistake, and
   * shown as such — a gap nobody can act on teaches people to ignore the ones
   * they can.
   */
  rdpAwaitingAgent: number
}

/**
 * Gateway policy, as the control plane holds it.
 *
 * Every field loosens or tightens what a brokered session may do, so the shape
 * is flat and boolean on purpose: a policy an operator cannot read off the
 * screen in one pass is one they will get wrong.
 */
export interface GatewayPolicy {
  allowLocalForward: boolean
  allowRemoteForward: boolean
  allowAgentForward: boolean
  allowX11Forward: boolean
  proxySftpSubsystem: boolean
  failClosedOnRecordingLoss: boolean
  requireEbpfForRoot: boolean
  /** Not a toggle: three positions, so it is edited on its own. */
  directSsh: DirectSsh
}

export type PolicyKey = Exclude<keyof GatewayPolicy, 'directSsh'>

/* ── Databases ──────────────────────────────────────────────────────────── */

/** What a person may do on a database, least to most. */
export type DBLevel = 'read' | 'write' | 'owner' | 'dba'
export type DBLabel = 'production' | 'staging' | 'development'

/** The safety settings of one database. 0 or absent means the default. */
export interface DBPolicy {
  statementTimeoutS?: number
  idleTxTimeoutS?: number
  maxRows?: number
  idleTimeoutS?: number
  maxSessionS?: number
  /** Redis: the largest db KEYS runs on. */
  keysLimit?: number
  /**
   * Postgres: a statement that changes it runs once someone else approves
   * it. Unset follows the label: on for production, off otherwise.
   */
  writeApproval?: boolean
  /** Who decides those changes. Unset is 'approvers'. */
  writeApprovers?: ChangeApprovers
}

/** Who decides a database's changes: the organization's admins and approvers, or also its editors. */
export type ChangeApprovers = 'approvers' | 'editors'

/** How a database's changes are approved, from the control plane. */
export interface ChangeApproval {
  needed: boolean
  /** Whether they would be by the label alone. */
  default: boolean
  approvers: ChangeApprovers
  /** Who may change it now (Write, Owner or DBA), when editors approve. */
  editors: string[]
}

/** What a database entry is: a Postgres database, a Redis server or a RabbitMQ vhost. */
export type DBEngine = 'postgres' | 'mysql' | 'mongodb' | 'redis' | 'rabbitmq' | 'kafka'

/** One database on one server. */
export interface Database {
  id: UUID
  assetId: UUID
  hostname: string
  engine: DBEngine
  host: string
  port: number
  /** A PgBouncer sessions go through; a port of 0 is none. */
  poolerHost: string
  poolerPort: number
  /** Redis: the server's redis.conf, where the agent reads the admin password. A path, never the password. */
  configPath?: string
  /** The database (Postgres), the logical db it opens in (Redis) or the vhost (RabbitMQ). */
  dbname: string
  adminUser: string
  label: DBLabel
  policy: DBPolicy
  createdBy: string
  createdAt: string
  tunnelConnected: boolean
}

/** The one-time PgBouncer setup for a database, from the control plane. */
export interface DBPoolerSetup {
  authRole: string
  authQuery: string
  sql: string[]
  grant: string
  ini: string
  poolerHost: string
  poolerPort: number
}

/** What one person may do on one database right now. */
export interface DBAccess {
  /** What they can open without asking; '' is nothing. */
  level: DBLevel | ''
  /** The highest level they could ask for; absent is none. */
  approvable?: DBLevel
  /** Set when `level` comes from an approved request. */
  grantExpires?: string
}

/** Who may open a database, at what level: a team or a person. */
export interface DBAssignment {
  id: UUID
  databaseId: UUID
  teamId?: UUID
  teamName?: string
  userEmail?: string
  level: DBLevel
  needsApproval: boolean
  grantedBy: string
  grantedAt: string
  /** The server's rule for databases added later that wrote this row. */
  ruleId?: UUID
}

/** Whether sessions wait for an approver: as each label says, always, never. */
export type BulkApproval = 'label' | 'always' | 'never'

/** Teams and people given a level on many databases at once. */
export interface BulkAccessInput {
  databaseIds: UUID[]
  teams: UUID[]
  people: string[]
  /** For databases; brokerLevel for brokers. */
  level: DBLevel
  brokerLevel?: DBLevel
  approval?: BulkApproval
  /** Also lower anyone who has more. */
  lower?: boolean
  /** Also every database (or broker) added to the same server from now on. */
  later?: boolean
}

/** One team or person on one database: what they have and will have. */
export interface BulkAccessLine {
  databaseId: UUID
  databaseName: string
  engine: string
  hostname: string
  label: DBLabel
  teamId?: UUID
  teamName?: string
  userEmail?: string
  before: DBLevel | ''
  beforeApproval: boolean
  after: DBLevel | ''
  afterApproval: boolean
  change: 'new' | 'raised' | 'approval' | 'same' | 'kept' | 'lowered' | 'removed'
}

/** Every database (or broker) added to a server from now on goes to a team or person. */
export interface DatabaseRule {
  id: UUID
  assetId: UUID
  hostname: string
  teamId?: UUID
  teamName?: string
  userEmail?: string
  kind: 'database' | 'broker'
  level: DBLevel
  approval: BulkApproval
  createdBy: string
  createdAt: string
  /** Rows it gave that are still its own. */
  gave: number
}

/* ── Database backups ────────────────────────────────────────────────────── */

export type BackupDestination = 'server' | 'control' | 'both'

/** When a database is backed up and what is kept. */
export interface BackupSchedule {
  databaseId: UUID
  enabled: boolean
  every: 'hourly' | 'daily' | 'weekly'
  atHour: number
  atMinute: number
  /** 0 is Sunday. */
  weekday: number
  destination: BackupDestination
  keepLast: number
  /** 0 keeps by count only. */
  keepDays: number
  nextRunAt: string | null
  misses: number
  updatedBy: string
  updatedAt: string
  /** False until an admin saves one; the rest are the defaults. */
  exists: boolean
  /** Test restores: off, after every scheduled backup, daily or weekly. */
  testEvery: 'off' | 'after' | 'daily' | 'weekly'
  testHour: number
  testMinute: number
  testWeekday: number
  testNextAt?: string | null
  /** An admin's checks, each one SELECT, run on the restored copy. */
  testChecks: TestCheck[]
}

export interface TestCheck { name: string; sql: string }

export type BackupTestState = 'running' | 'passed' | 'warned' | 'failed' | 'skipped'

export interface TestStep { name: string; ok: boolean; detail?: string; ms: number }

/** One check of a test: built in or an admin's. */
export interface TestCheckOutcome { name: string; kind: 'builtin' | 'custom'; state: 'ok' | 'warn' | 'fail'; value: string }

export interface TestTableRows { name: string; rows: number; previous?: number }

/** Numbers about the restored copy, never rows. */
export interface TestReport {
  steps: TestStep[] | null
  checks: TestCheckOutcome[] | null
  /** Only on a single test's read; the list leaves it out. */
  tables: TestTableRows[] | null
  tablesTotal: number
  missing?: string[]
  indexes: number
  invalidIndexes: number
  warnings: number
  needBytes: number
  freeBytes: number
  sourceBytes: number
  previousId?: string
  previousBytes?: number
}

/** One test restore: a backup restored into a throwaway copy, checked and dropped. */
export interface BackupTest {
  id: UUID
  backupId: UUID
  databaseId: UUID
  startedBy: string
  state: BackupTestState
  stage: string
  detail: string
  error: string
  scratchName: string
  scratchDropped: boolean
  /** How long pg_restore took: the measured recovery time. */
  restoreMs: number
  tables: number
  totalRows: number
  bytes: number
  report: TestReport
  startedAt: string
  finishedAt?: string
  reportedAt: string
  backupStartedAt: string
}

/** A check tried against the live database. */
export interface CheckResult { name: string; ok: boolean; value?: string; error?: string }

export type BackupState = 'running' | 'done' | 'failed' | 'skipped' | 'missed' | 'deleted'

/** One backup, running or kept. */
export interface DatabaseBackup {
  id: UUID
  databaseId: UUID
  /** An email, or schedule:<id> for a scheduled run. */
  startedBy: string
  reason: string
  state: BackupState
  stage: string
  detail: string
  error: string
  destination: BackupDestination
  onServer: boolean
  onControl: boolean
  bytes: number
  sha256: string
  keyId: string
  serverVersion: string
  dumpVersion: string
  verified: boolean
  pinned: boolean
  startedAt: string
  finishedAt?: string
  reportedAt: string
}

export type RestoreState = 'pending' | 'denied' | 'cancelled' | 'running' | 'done' | 'failed' | 'undone'

/** One restore: into a new database, or over the existing one. */
export interface DatabaseRestore {
  id: UUID
  backupId: UUID
  databaseId: UUID
  mode: 'new' | 'replace'
  newName: string
  requestedBy: string
  reason: string
  requestId?: string
  state: RestoreState
  stage: string
  detail: string
  error: string
  /** replace: where the previous database went, until keepUntil. */
  renamedTo: string
  keepUntil?: string
  keptDropped: boolean
  createdDatabaseId?: string
  warnings: number
  createdAt: string
  startedAt?: string
  finishedAt?: string
  reportedAt: string
  backupStartedAt: string
  backupBytes: number
  backupReason: string
  backupBy: string
  dbname: string
  hostname: string
}

/** What a server can do about backups, from its agent. */
export interface BackupCatalog {
  dumpVersion: string
  restoreVersion: string
  serverVersion: string
  freeBytes: number
  dir: string
  compression: string
  local?: Record<string, number>
}

/** The backups page of one database. */
export interface BackupsView {
  backups: DatabaseBackup[]
  running: DatabaseBackup | null
  schedule: BackupSchedule
  restores: DatabaseRestore[]
  level: DBLevel
  /** The control plane has storage a backup can be sent to. */
  storage: boolean
  staleAfterS: number
  /** The latest test restores, newest first. */
  tests: BackupTest[]
  testing: BackupTest | null
  /** The newest backup whose test passed; retention keeps it. */
  lastGood: BackupTest | null
}

/**
 * Home's live dashboard for the roles that see the whole fleet; mirrors
 * HomeSnapshot in internal/control/home_snapshot.go.
 */
export interface HomeSnapshot {
  servers: { total: number; answering: number; quiet: number; down: number }
  live: { count: number; root: number; database: number; broker?: number; silent: number }
  /** The last 24 hours. `hours` is oldest first, each [terminal, desktop, database]. */
  sessions: { last24h: number; weekAgo: number; hours: [number, number, number][]; kinds: [number, number, number] }
  waiting: { count: number; oldestAt: string | null }
  /** From each agent's last report, if it is younger than 3 minutes. */
  processor: { average: number | null; reporting: number; busiest: { assetId: string; name: string; busy: number }[] }
  disks: { assetId: string; name: string; mount: string; used: number }[]
  signIns: { last24h: number; failed24h: number; failed15m: number; hours: number[] }
  liveList: { id: string; user: string; principal: string; host: string; kind: string; startedAt: string }[]
  audit: AuditEvent[]
  /** The last hour, one point a minute, oldest first; `since` is where they begin. */
  trends: { since: string | null; answering: number[]; live: number[]; waiting: number[]; processor: (number | null)[] }
  unreviewedHosts: number
  at: string
}

/** One message about Home on the stream: the whole of it, or what changed. */
export interface HomeFrame {
  v: number
  full?: boolean
  set: Partial<HomeSnapshot>
  /** Sent instead when this person may no longer see Home's fleet numbers. */
  off?: boolean
}
