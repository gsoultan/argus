# Cloud › Risks & waste

Security risks, what is paid for and not in use, and firewall rules nothing
needs, across every connected cloud account. Plan:
`/mnt/project-files/plans/cloud-risks-waste/design.md` (Part A of the joint
Cloud plan in `plans/cloud-plan/`).

## Where it is worked out
- `cloud.Findings(resources, facts, now)` in `internal/control/cloud/findings.go`
  is pure: stored inventory (every region plus the account-wide kinds) and
  agent facts in, findings out. No provider call, no write.
- It folds in `cloud.Idle`, so Costs' idle total and the "Not in use" group are
  the same numbers by construction (a test says so).
- `GET /api/v1/cloud/findings` (`internal/control/cloud_findings.go`) runs it per
  account on each request. Viewers: anyone who sees Cloud. Keeping: changers
  (admin, owner).

## Rules it keeps
- **A check whose input was not read says nothing.** A firewall's use comes
  from `Details["interfaces"]`, written only when DescribeNetworkInterfaces
  succeeded; no key means unknown, never "unused". Same for
  `publicChecked`, `templatesChecked`, a bucket's `publicBlock`.
- **Every finding says how it is known**: `cloud` (fact from the read), `agent`
  (Argus agent measured), `judgement` (a threshold Argus chose: idle). The
  console labels them "AWS says", "Agent measured", "Argus's judgement".
- **Agent-based checks need every instance behind a group to have a fresh
  agent** (`ServerFacts.HasAgent`, listeners reported within 2 h) and the
  group used by nothing but those instances. One unknown means no finding.
- **Nothing changes from Risks & waste.** A fix opens the ordinary change
  (rules editor with the rule struck, the resource's own panel, or one
  snapshot-delete change per picked snapshot).
- **Removing a rule matches, it does not check.** The server swaps each rule
  to take away for the group's stored copy (`storedRules`), so a rule the
  cloud lists but Argus would not let be added (ICMP type, protocol by number,
  peered group) can still go. In Google a firewall rule is the rule: striking
  its last allow makes the editor ask for `firewall-delete`, which Google
  allows while VMs carry its tag (the summary names them).
- A firewall finding with a removal waiting, being made, or made since the
  group was last read shows where that change is instead of the fix
  (`ruleChangeFor`), so the same change is not asked for twice.
- AWS keeps no per-rule hit count. "Firewall rules" means rules nothing needs
  (covered by a wider rule, nothing listens, group used by nothing), not rules
  with no traffic. Stale group references are not checked: inside one account
  AWS refuses to delete a referenced group, so they only occur across peering.

## Account-wide read
`cloud.AccountReader` (optional) is called once per sync after the regions:
IAM credential report (root MFA and keys, people without MFA, unused or old
keys), S3 public access block per bucket, RDS instances in every region.
Stored in `cloud_resources` under region `global`. The snapshot's `Kinds` are
only the parts read in full, so a failed part keeps its last read rather than
being marked gone. All of it is in the existing read policy; no stack update.
The account-wide S3 block (`s3:GetAccountPublicAccessBlock`) is not readable,
so a bucket finding says it may be overridden.

## Keeping a finding
Table `cloud_finding_keeps` (migration 081). A keep hides a finding until
`expires_at`, or with `until_change` until the finding's `Basis` (a digest of
what it rests on) differs. A High risk is never kept for more than 90 days.
Keep and un-keep write `cloud.finding.kept` / `cloud.finding.unkept`.
