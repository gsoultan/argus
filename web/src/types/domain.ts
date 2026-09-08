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
  /** Sourced from the OIDC provider; null for local break-glass accounts. */
  idpSubject: string | null
  mfaEnrolled: boolean
  lastSeenAt: ISOTime | null
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
  /** Accounts on the target that Argus can broker a session as. */
  principals: string[]
  /** Null when credentialMode is 'ca-certificate' — nothing to rotate. */
  credentialRotatedAt: ISOTime | null
  rotationIntervalDays: number | null
}

export interface AssetGroup {
  id: UUID
  name: string
  description: string
  assetCount: number
}

/* ── Sessions ────────────────────────────────────────────────────────────── */

export type SessionState = 'active' | 'closed' | 'terminated' | 'rejected'
export type SessionProtocol = 'ssh' | 'sftp' | 'rdp'

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
export type RecordingFidelity = 'pty' | 'ebpf' | 'rdp' | 'none'

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
  riskFlags: RiskFlag[]
  /** Who stopped this session, when state is 'terminated'. */
  terminatedBy?: string | null
  /** Why it was stopped. An auditor reading a terminated session asks this
   *  first, and the answer has to come from the record rather than from
   *  whoever remembers. */
  terminationReason?: string | null
}

export type RiskFlag =
  | 'bypassed-gateway'
  | 'off-hours'
  | 'new-asset-for-user'
  | 'root-principal'
  | 'unpinned-host-key'
  | 'break-glass'
  | 'bulk-file-transfer'

/* ── Access requests (JIT) ───────────────────────────────────────────────── */

export type RequestState = 'pending' | 'approved' | 'denied' | 'expired' | 'revoked'

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
}

/* ── Audit ───────────────────────────────────────────────────────────────── */

export type AuditAction =
  | 'session.start'
  | 'session.end'
  | 'session.terminate'
  | 'session.command'
  | 'file.upload'
  | 'file.download'
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
  sessionsActive: number
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
}

export type PolicyKey = keyof GatewayPolicy
