package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

/*
Each organization's approval policy.

Argus decides when something waits for a second person: a Production
database's changes, a Production cloud account, a server's update rule, a
login as root, and so on. Those rules stay where they are. The policy decides
only who may act directly instead of waiting, for each kind of action and each
environment:

	second   a second person approves (the default, and what every
	         organization did before this existed)
	owners   owners act directly
	admins   admins and owners act directly
	anyone   anyone who may already do it acts directly: no approval

It never gives anyone access they do not already have: every caller has
already decided that this person may do the thing, and asks the policy only
whether they must wait.

Every place that waits for a second person names one of the kinds below and
asks actsDirectly. A kind this file does not know, a policy that cannot be
read, a person whose role cannot be read: each is "a second person
approves". An action that goes straight through is recorded by recordDirect,
at notice severity, with the rule that allowed it.

The platform may lock a kind (argus_platform.approval_locks) so that no
organization loosens it.
*/

// ApprovalKind names one kind of action that can wait for a second person.
type ApprovalKind string

// The kinds. New features that wait for a second person add one here, with
// its words, before they ship: the settings page is built from this list.
const (
	KindElevatedLogin  ApprovalKind = "logins.elevated"
	KindDirectSSH      ApprovalKind = "logins.direct"
	KindServerRuns     ApprovalKind = "servers.runs"
	KindServerUpkeep   ApprovalKind = "servers.upkeep"
	KindScriptReview   ApprovalKind = "servers.scripts"
	KindDataChange     ApprovalKind = "data.change"
	KindDataSession    ApprovalKind = "data.session"
	KindDataNetwork    ApprovalKind = "data.network"
	KindDataRestore    ApprovalKind = "data.restore"
	KindCloudChange    ApprovalKind = "cloud.change"
	KindCloudSchedule  ApprovalKind = "cloud.schedule"
	KindCloudAccess    ApprovalKind = "cloud.access"
	KindCloudPurchase  ApprovalKind = "cloud.purchase"
	KindApprovalPolicy ApprovalKind = "policy.loosen" // not a row: changing the policy itself
)

// Environments a rule is kept for. EnvAll is for kinds that have none.
const (
	EnvProduction = "production"
	EnvStaging    = "staging"
	EnvOther      = "other"
	EnvAll        = "all"
)

// Choices, from closed to open.
const (
	ChoiceSecond = "second"
	ChoiceOwners = "owners"
	ChoiceAdmins = "admins"
	ChoiceAnyone = "anyone"
)

func choiceRank(c string) int {
	switch c {
	case ChoiceOwners:
		return 1
	case ChoiceAdmins:
		return 2
	case ChoiceAnyone:
		return 3
	}
	return 0
}

func validChoice(c string) bool { return c == ChoiceSecond || choiceRank(c) > 0 }

// choiceWords is a choice as the settings page says it.
func choiceWords(c string) string {
	switch c {
	case ChoiceOwners:
		return "owners act directly"
	case ChoiceAdmins:
		return "admins and owners act directly"
	case ChoiceAnyone:
		return "no approval"
	}
	return "a second person approves"
}

func envWords(env string) string {
	switch env {
	case EnvProduction:
		return "Production"
	case EnvStaging:
		return "Staging"
	case EnvOther:
		return "Development and other"
	}
	return "Every environment"
}

// ApprovalKindInfo is one row of the settings page.
type ApprovalKindInfo struct {
	Kind  ApprovalKind `json:"kind"`
	Group string       `json:"group"`
	Title string       `json:"title"`
	What  string       `json:"what"`
	// Envs are the columns this kind has: the three environments, or "all".
	Envs []string `json:"envs"`
	// Today says, per environment, when Argus asks for a second person,
	// before the policy: "always", "sometimes" (Note says when) or "never".
	Today map[string]string `json:"today"`
	Notes map[string]string `json:"notes,omitempty"`
	// Locked environments, and why, when the platform locked them.
	Locked map[string]string `json:"locked,omitempty"`
}

var threeEnvs = []string{EnvProduction, EnvStaging, EnvOther}

func every(v string) map[string]string {
	return map[string]string{EnvProduction: v, EnvStaging: v, EnvOther: v}
}

// ApprovalKinds is every kind, in the order the settings page lists them.
var ApprovalKinds = []ApprovalKindInfo{
	{Kind: KindElevatedLogin, Group: "Servers", Title: "Logins that need a request",
		What: "Root, admin and administrator, taking control of a device whose settings ask for approval, and any login whose access says it needs approval.",
		Envs: threeEnvs, Today: every("always")},
	{Kind: KindDirectSSH, Group: "Servers", Title: "Direct SSH logins",
		What: "Your own login to your personal account on a server set to “ask for approval”, let in from the console.",
		Envs: threeEnvs, Today: every("sometimes"), Notes: every("where the server asks")},
	{Kind: KindServerRuns, Group: "Servers", Title: "Updates, software and apps",
		What: "Update, install and remove runs on servers whose rule asks for a second admin.",
		Envs: threeEnvs, Today: every("sometimes"), Notes: every("where the rule asks")},
	{Kind: KindServerUpkeep, Group: "Servers", Title: "Growing disks, cleaning logs",
		What: "Disk grows and log cleanups on servers whose rule asks for a second admin.",
		Envs: threeEnvs, Today: every("sometimes"), Notes: every("where the rule asks")},
	{Kind: KindScriptReview, Group: "Servers", Title: "Reviewing your own scripts",
		What: "A new script version runs nowhere until someone other than its author reviews it.",
		Envs: []string{EnvAll}, Today: map[string]string{EnvAll: "always"}},
	{Kind: KindDataChange, Group: "Databases and brokers", Title: "Changing data",
		What: "Statements that change a database, and changes to Redis, RabbitMQ and Kafka, where the database asks.",
		Envs: threeEnvs, Today: map[string]string{EnvProduction: "always", EnvStaging: "sometimes", EnvOther: "sometimes"},
		Notes: map[string]string{EnvStaging: "where the database asks", EnvOther: "where the database asks"}},
	{Kind: KindDataSession, Group: "Databases and brokers", Title: "Opening a session",
		What: "Where someone's access says “needs approval each time”.",
		Envs: threeEnvs, Today: every("sometimes"), Notes: every("where access asks")},
	{Kind: KindDataNetwork, Group: "Databases and brokers", Title: "Opening to other machines",
		What: "Changing which machines can connect to a database or broker.",
		Envs: threeEnvs, Today: map[string]string{EnvProduction: "always", EnvStaging: "sometimes", EnvOther: "sometimes"},
		Notes: map[string]string{EnvStaging: "where it asks, and opening to anywhere", EnvOther: "where it asks, and opening to anywhere"}},
	{Kind: KindDataRestore, Group: "Databases and brokers", Title: "Restoring in place, making replicas",
		What: "Replacing a database from a backup, or adding a streaming replica.",
		Envs: threeEnvs, Today: every("always")},
	{Kind: KindCloudChange, Group: "Cloud", Title: "Changing resources",
		What: "Instances, disks, firewalls, load balancers, scaling groups and images.",
		Envs: threeEnvs, Today: map[string]string{EnvProduction: "always", EnvStaging: "sometimes", EnvOther: "never"},
		Notes: map[string]string{EnvProduction: "changes and removals", EnvStaging: "removals, opening ports"}},
	{Kind: KindCloudSchedule, Group: "Cloud", Title: "Start and stop schedules",
		What: "A schedule that starts or stops Production instances.",
		Envs: []string{EnvProduction}, Today: map[string]string{EnvProduction: "always"}},
	{Kind: KindCloudAccess, Group: "Cloud", Title: "Console and command-line access",
		What: "Just-in-time access to the provider's console.",
		Envs: threeEnvs, Today: map[string]string{EnvProduction: "always", EnvStaging: "sometimes", EnvOther: "never"},
		Notes: map[string]string{EnvProduction: "read and operate", EnvStaging: "operate"}},
	{Kind: KindCloudPurchase, Group: "Cloud", Title: "Buying reservations",
		What: "Reserved Instances and Savings Plans.",
		Envs: []string{EnvAll}, Today: map[string]string{EnvAll: "always"}},
}

func kindInfo(k ApprovalKind) (ApprovalKindInfo, bool) {
	for _, i := range ApprovalKinds {
		if i.Kind == k {
			return i, true
		}
	}
	return ApprovalKindInfo{}, false
}

// cellEnv is where a rule for kind in env is kept: env itself, or "all" for
// a kind with no environments. "" when kind has no such column.
func cellEnv(k ApprovalKind, env string) string {
	info, ok := kindInfo(k)
	if !ok {
		return ""
	}
	for _, e := range info.Envs {
		if e == env || e == EnvAll {
			return e
		}
	}
	return ""
}

// envOfWord is the environment a label or tag value names, or "".
func envOfWord(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "production", "prod", "prd", "live":
		return EnvProduction
	case "staging", "stage", "stg", "uat", "preprod":
		return EnvStaging
	case "development", "dev", "test", "testing", "sandbox", "other":
		return EnvOther
	}
	return ""
}

// envOfLabel is a database's or account's environment: production, staging,
// or other.
func envOfLabel(label string) string {
	if e := envOfWord(label); e != "" {
		return e
	}
	return EnvOther
}

// envOfTags is a server's environment from its tags: "production",
// "env:production", "environment=prod" and the like. A server with none is
// "other". A server tagged as two is the stricter of them.
func envOfTags(tags []string) string {
	out := EnvOther
	for _, t := range tags {
		v := t
		if k, rest, ok := strings.Cut(t, ":"); ok && isEnvKey(k) {
			v = rest
		} else if k, rest, ok := strings.Cut(t, "="); ok && isEnvKey(k) {
			v = rest
		}
		out = stricterOf(out, envOfWord(v))
	}
	return out
}

func isEnvKey(k string) bool {
	k = strings.ToLower(strings.TrimSpace(k))
	return k == "env" || k == "environment"
}

// stricterOf is the stricter of two environments ("" counts as none).
func stricterOf(a, b string) string {
	rank := func(e string) int {
		switch e {
		case EnvProduction:
			return 3
		case EnvStaging:
			return 2
		case EnvOther:
			return 1
		}
		return 0
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

// ApprovalRule is one cell an organization changed.
type ApprovalRule struct {
	Kind   ApprovalKind `json:"kind"`
	Env    string       `json:"env"`
	Choice string       `json:"choice"`
	SetBy  string       `json:"setBy,omitempty"`
	SetAt  *time.Time   `json:"setAt,omitempty"`
}

// ApprovalPolicy is an organization's whole policy.
type ApprovalPolicy struct {
	Rules            []ApprovalRule `json:"rules"`
	RequireReason    bool           `json:"requireReason"`
	NotifyProduction bool           `json:"notifyProduction"`
	UpdatedBy        string         `json:"updatedBy,omitempty"`
	UpdatedAt        *time.Time     `json:"updatedAt,omitempty"`
}

// Version names this state of the policy, for a change asked against it.
func (p ApprovalPolicy) Version() string {
	if p.UpdatedAt == nil {
		return "default"
	}
	return p.UpdatedAt.UTC().Format(time.RFC3339Nano)
}

// choice is the cell's choice, "second" when unset.
func (p ApprovalPolicy) choice(k ApprovalKind, env string) string {
	for _, r := range p.Rules {
		if r.Kind == k && r.Env == env {
			return r.Choice
		}
	}
	return ChoiceSecond
}

// normalise drops cells that are the default, unknown kinds and columns,
// and anything not a choice; it returns an error for those last three.
func (p *ApprovalPolicy) normalise() error {
	seen := map[string]bool{}
	out := []ApprovalRule{}
	for _, r := range p.Rules {
		if _, ok := kindInfo(r.Kind); !ok {
			return fmt.Errorf("%q is not a kind of action Argus knows", r.Kind)
		}
		if cellEnv(r.Kind, r.Env) != r.Env {
			return fmt.Errorf("%s has no %q column", r.Kind, r.Env)
		}
		if !validChoice(r.Choice) {
			return fmt.Errorf("%q is not a choice: use second, owners, admins or anyone", r.Choice)
		}
		key := string(r.Kind) + "/" + r.Env
		if seen[key] {
			return fmt.Errorf("%s in %s is given twice", r.Kind, r.Env)
		}
		seen[key] = true
		if r.Choice != ChoiceSecond {
			out = append(out, ApprovalRule{Kind: r.Kind, Env: r.Env, Choice: r.Choice})
		}
	}
	p.Rules = out
	return nil
}

// Loosens lists, in words, what next lets more people do than p does.
func (p ApprovalPolicy) Loosens(next ApprovalPolicy) []string {
	var out []string
	for _, info := range ApprovalKinds {
		for _, env := range info.Envs {
			was, now := p.choice(info.Kind, env), next.choice(info.Kind, env)
			if choiceRank(now) > choiceRank(was) {
				out = append(out, envWords(env)+" · "+info.Title+": "+choiceWords(was)+" → "+choiceWords(now))
			}
		}
	}
	if p.RequireReason && !next.RequireReason {
		out = append(out, "No longer asks for a reason when someone acts directly")
	}
	if p.NotifyProduction && !next.NotifyProduction {
		out = append(out, "No longer tells owners when someone acts directly in Production")
	}
	return out
}

// Changes lists every difference between p and next, in words.
func (p ApprovalPolicy) Changes(next ApprovalPolicy) []string {
	var out []string
	for _, info := range ApprovalKinds {
		for _, env := range info.Envs {
			was, now := p.choice(info.Kind, env), next.choice(info.Kind, env)
			if was != now {
				out = append(out, envWords(env)+" · "+info.Title+": "+choiceWords(was)+" → "+choiceWords(now))
			}
		}
	}
	if p.RequireReason != next.RequireReason {
		out = append(out, "Ask for a reason when acting directly: "+onOff(next.RequireReason))
	}
	if p.NotifyProduction != next.NotifyProduction {
		out = append(out, "Tell owners each time in Production: "+onOff(next.NotifyProduction))
	}
	return out
}

// ApprovalPolicy reads the organization's policy. An organization that never
// set one has the default: every cell a second person, both switches on.
func (s *Store) ApprovalPolicy(ctx context.Context) (ApprovalPolicy, error) {
	p := ApprovalPolicy{Rules: []ApprovalRule{}, RequireReason: true, NotifyProduction: true}
	var at time.Time
	err := s.db(ctx).QueryRow(ctx, `SELECT require_reason, notify_production, updated_by, updated_at
		FROM approval_policy_settings WHERE id`).Scan(&p.RequireReason, &p.NotifyProduction, &p.UpdatedBy, &at)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return p, err
	case p.UpdatedBy != "":
		p.UpdatedAt = &at
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT kind, environment, choice, set_by, set_at
		FROM approval_policy_rules ORDER BY kind, environment`)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var r ApprovalRule
		var setAt time.Time
		if err := rows.Scan(&r.Kind, &r.Env, &r.Choice, &r.SetBy, &setAt); err != nil {
			return p, err
		}
		r.SetAt = &setAt
		p.Rules = append(p.Rules, r)
	}
	return p, rows.Err()
}

// ErrPolicyMoved is a change asked against a policy that has changed since.
var ErrPolicyMoved = errors.New("the approval policy changed after this was asked; ask again from the current policy")

// SetApprovalPolicy replaces the policy with next, as by. base, when not
// empty, is the version the change was asked against.
func (s *Store) SetApprovalPolicy(ctx context.Context, next ApprovalPolicy, by, base string) (ApprovalPolicy, error) {
	if err := next.normalise(); err != nil {
		return ApprovalPolicy{}, err
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return ApprovalPolicy{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// One writer at a time, and the version checked under the lock.
	if _, err := tx.Exec(ctx, `INSERT INTO approval_policy_settings (id, updated_at) VALUES (true, 'epoch')
		ON CONFLICT (id) DO NOTHING`); err != nil {
		return ApprovalPolicy{}, err
	}
	var at time.Time
	var updatedBy string
	if err := tx.QueryRow(ctx, `SELECT updated_by, updated_at FROM approval_policy_settings WHERE id FOR UPDATE`).
		Scan(&updatedBy, &at); err != nil {
		return ApprovalPolicy{}, err
	}
	if base != "" {
		cur := ApprovalPolicy{UpdatedAt: &at}
		if updatedBy == "" {
			cur.UpdatedAt = nil
		}
		if cur.Version() != base {
			return ApprovalPolicy{}, ErrPolicyMoved
		}
	}
	old := map[string]ApprovalRule{}
	rows, err := tx.Query(ctx, `SELECT kind, environment, choice, set_by, set_at FROM approval_policy_rules`)
	if err != nil {
		return ApprovalPolicy{}, err
	}
	for rows.Next() {
		var r ApprovalRule
		var t time.Time
		if err := rows.Scan(&r.Kind, &r.Env, &r.Choice, &r.SetBy, &t); err != nil {
			rows.Close()
			return ApprovalPolicy{}, err
		}
		r.SetAt = &t
		old[string(r.Kind)+"/"+r.Env] = r
	}
	rows.Close()
	if _, err := tx.Exec(ctx, `DELETE FROM approval_policy_rules`); err != nil {
		return ApprovalPolicy{}, err
	}
	for _, r := range next.Rules {
		// A cell left as it was keeps who set it and when.
		setBy, setAt := by, time.Now().UTC()
		if o, ok := old[string(r.Kind)+"/"+r.Env]; ok && o.Choice == r.Choice {
			setBy, setAt = o.SetBy, *o.SetAt
		}
		if _, err := tx.Exec(ctx, `INSERT INTO approval_policy_rules (kind, environment, choice, set_by, set_at)
			VALUES ($1, $2, $3, $4, $5)`, string(r.Kind), r.Env, r.Choice, setBy, setAt); err != nil {
			return ApprovalPolicy{}, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE approval_policy_settings SET require_reason = $1, notify_production = $2,
		updated_by = $3, updated_at = clock_timestamp() WHERE id`, next.RequireReason, next.NotifyProduction, by); err != nil {
		return ApprovalPolicy{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ApprovalPolicy{}, err
	}
	return s.ApprovalPolicy(ctx)
}

// ApprovalLock is a cell the platform does not let organizations loosen.
type ApprovalLock struct {
	Kind   ApprovalKind `json:"kind"`
	Env    string       `json:"env"`
	Reason string       `json:"reason"`
	SetBy  string       `json:"setBy,omitempty"`
	SetAt  *time.Time   `json:"setAt,omitempty"`
}

// ApprovalLocks lists the platform's locks. They are the same for every
// organization.
func (s *Store) ApprovalLocks(ctx context.Context) ([]ApprovalLock, error) {
	rows, err := s.pool.Query(ctx, `SELECT kind, environment, reason, set_by, set_at
		FROM argus_platform.approval_locks ORDER BY kind, environment`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ApprovalLock{}
	for rows.Next() {
		var l ApprovalLock
		var t time.Time
		if err := rows.Scan(&l.Kind, &l.Env, &l.Reason, &l.SetBy, &t); err != nil {
			return nil, err
		}
		l.SetAt = &t
		out = append(out, l)
	}
	return out, rows.Err()
}

// SetApprovalLocks replaces the platform's locks.
func (s *Store) SetApprovalLocks(ctx context.Context, locks []ApprovalLock, by string) error {
	for _, l := range locks {
		if _, ok := kindInfo(l.Kind); !ok {
			return fmt.Errorf("%q is not a kind of action Argus knows", l.Kind)
		}
		if cellEnv(l.Kind, l.Env) != l.Env {
			return fmt.Errorf("%s has no %q column", l.Kind, l.Env)
		}
		if len(l.Reason) > 300 {
			return errors.New("keep each reason under 300 characters")
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM argus_platform.approval_locks`); err != nil {
		return err
	}
	for _, l := range locks {
		if _, err := tx.Exec(ctx, `INSERT INTO argus_platform.approval_locks (kind, environment, reason, set_by)
			VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, string(l.Kind), l.Env, strings.TrimSpace(l.Reason), by); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func lockFor(locks []ApprovalLock, k ApprovalKind, env string) *ApprovalLock {
	for i := range locks {
		if locks[i].Kind == k && locks[i].Env == env {
			return &locks[i]
		}
	}
	return nil
}

// DirectAllowance is the policy letting someone act without a second person.
type DirectAllowance struct {
	Kind   ApprovalKind
	Env    string
	Choice string
	SetBy  string
	SetAt  *time.Time
	// RequireReason and Notify are the policy's switches at the time.
	RequireReason bool
	Notify        bool
}

// Rule is the rule that allowed it, in words.
func (d *DirectAllowance) Rule() string {
	info, _ := kindInfo(d.Kind)
	s := envWords(d.Env) + " · " + info.Title + " · " + choiceWords(d.Choice)
	if d.SetBy != "" && d.SetAt != nil {
		s += " (set by " + d.SetBy + " on " + d.SetAt.UTC().Format("2 Jan 2006") + ")"
	}
	return s
}

// roleActsDirectly is whether role may act directly under choice.
func roleActsDirectly(choice, role string) bool {
	switch choice {
	case ChoiceOwners:
		return role == "owner"
	case ChoiceAdmins:
		return role == "owner" || role == "admin"
	case ChoiceAnyone:
		return role != ""
	}
	return false
}

// actsDirectly is whether email may do something of kind in env without a
// second person. nil means they wait, as before the policy existed; so does
// every error, which is logged.
func (a *API) actsDirectly(ctx context.Context, kind ApprovalKind, env, email string) *DirectAllowance {
	cell := cellEnv(kind, env)
	if cell == "" || email == "" {
		return nil
	}
	p, err := a.store.ApprovalPolicy(ctx)
	if err != nil {
		a.log.Error("could not read the approval policy; a second person approves", "kind", kind, "error", err)
		return nil
	}
	choice := p.choice(kind, cell)
	if choice == ChoiceSecond {
		return nil
	}
	locks, err := a.store.ApprovalLocks(ctx)
	if err != nil {
		a.log.Error("could not read the platform's approval locks; a second person approves", "kind", kind, "error", err)
		return nil
	}
	if lockFor(locks, kind, cell) != nil {
		return nil
	}
	var role string
	var disabled bool
	err = a.store.db(ctx).QueryRow(ctx, `SELECT role, disabled_at IS NOT NULL FROM users WHERE lower(email) = lower($1)`,
		email).Scan(&role, &disabled)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			a.log.Error("could not read a role for the approval policy; a second person approves", "error", err)
		}
		return nil
	}
	if disabled || !roleActsDirectly(choice, role) {
		return nil
	}
	d := &DirectAllowance{Kind: kind, Env: cell, Choice: choice, RequireReason: p.RequireReason,
		Notify: p.NotifyProduction}
	for _, r := range p.Rules {
		if r.Kind == kind && r.Env == cell {
			d.SetBy, d.SetAt = r.SetBy, r.SetAt
		}
	}
	return d
}

// actsDirectlyWithReason is actsDirectly for an action that carries a
// reason: where the policy asks for one and none was given, they wait.
func (a *API) actsDirectlyWithReason(ctx context.Context, kind ApprovalKind, env, email, reason string) *DirectAllowance {
	d := a.actsDirectly(ctx, kind, env, email)
	if d != nil && d.RequireReason && strings.TrimSpace(reason) == "" {
		return nil
	}
	return d
}

// recordDirect records that actor did something without a second person,
// because d allowed it: what they did, on target, and why. It tells the
// owners when the policy asks for that in Production. A nil d does nothing.
func (a *API) recordDirect(ctx context.Context, d *DirectAllowance, actor, target, what, reason string) {
	if d == nil {
		return
	}
	detail := strings.TrimSpace(what)
	if detail != "" && !strings.HasSuffix(detail, ".") {
		detail += "."
	}
	detail += " No second person: allowed by the approval policy, " + d.Rule() + "."
	if r := strings.TrimSpace(reason); r != "" {
		detail += " Reason: " + r
	}
	if _, err := a.store.AppendAudit(ctx, AuditEvent{Action: "approval.direct", Severity: "notice",
		ActorEmail: actor, Target: target, Detail: detail}); err != nil {
		a.log.Error("audit append failed", "error", err)
	}
	if d.Notify && d.Env == EnvProduction {
		a.tellPolicyKeepers(ctx, actor, actor+" acted directly in Production", detail, "/activity")
	}
}

// policyKeepers are who may change the policy and decide a loosening: the
// enabled owners, or the enabled admins of an organization with no owner
// (first-run setup makes the first account an admin).
func (s *Store) policyKeepers(ctx context.Context) ([]string, error) {
	rows, err := s.db(ctx).Query(ctx, `
		SELECT lower(email) FROM users
		 WHERE disabled_at IS NULL AND (role = 'owner' OR (role = 'admin' AND NOT EXISTS (
		       SELECT 1 FROM users WHERE role = 'owner' AND disabled_at IS NULL)))
		 ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (a *API) tellPolicyKeepers(ctx context.Context, actor, title, detail, link string) {
	keepers, err := a.store.policyKeepers(ctx)
	if err != nil {
		a.log.Error("listing owners for an alert failed", "error", err)
		return
	}
	for _, k := range keepers {
		if strings.EqualFold(k, actor) {
			continue
		}
		a.raiseAlert(ctx, ConsoleAlert{Kind: AlertApproval, Severity: "info", Audience: AudiencePerson, Recipient: k,
			Actor: actor, Title: title, Detail: detail, Link: link})
	}
}

// ApprovalPolicyChange is a loosening waiting for another owner.
type ApprovalPolicyChange struct {
	ID          string         `json:"id"`
	RequestedBy string         `json:"requestedBy"`
	RequestedAt time.Time      `json:"requestedAt"`
	Reason      string         `json:"reason"`
	Proposal    ApprovalPolicy `json:"proposal"`
	Base        string         `json:"-"`
	State       string         `json:"state"`
	DecidedBy   *string        `json:"decidedBy,omitempty"`
	DecidedAt   *time.Time     `json:"decidedAt,omitempty"`
	Note        string         `json:"note,omitempty"`
	// Changes and Loosens are worked out against the policy now.
	Changes []string `json:"changes"`
	Loosens []string `json:"loosens"`
}

// ErrPolicyChangePending is a second loosening asked while one waits.
var ErrPolicyChangePending = errors.New("a change to the approval policy is already waiting for an owner; decide or take it back first")

// ErrPolicyChangeNotFound names no waiting change.
var ErrPolicyChangeNotFound = errors.New("no such change to the approval policy; it may have been decided")

const policyChangeColumns = `id::text, requested_by, requested_at, reason, proposal, base, state, decided_by, decided_at, note`

func scanPolicyChange(row pgx.Row) (ApprovalPolicyChange, error) {
	var c ApprovalPolicyChange
	var raw []byte
	err := row.Scan(&c.ID, &c.RequestedBy, &c.RequestedAt, &c.Reason, &raw, &c.Base, &c.State,
		&c.DecidedBy, &c.DecidedAt, &c.Note)
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(raw, &c.Proposal)
	return c, err
}

// AskApprovalPolicyChange files a loosening for another owner to decide.
func (s *Store) AskApprovalPolicyChange(ctx context.Context, next ApprovalPolicy, by, reason, base string) (ApprovalPolicyChange, error) {
	if err := next.normalise(); err != nil {
		return ApprovalPolicyChange{}, err
	}
	raw, err := json.Marshal(ApprovalPolicy{Rules: next.Rules, RequireReason: next.RequireReason,
		NotifyProduction: next.NotifyProduction})
	if err != nil {
		return ApprovalPolicyChange{}, err
	}
	c, err := scanPolicyChange(s.db(ctx).QueryRow(ctx, `
		INSERT INTO approval_policy_changes (requested_by, reason, proposal, base)
		VALUES ($1, $2, $3, $4) RETURNING `+policyChangeColumns, by, reason, raw, base))
	if err != nil && strings.Contains(err.Error(), "approval_policy_changes_one_pending") {
		return c, ErrPolicyChangePending
	}
	return c, err
}

// PendingApprovalPolicyChange is the change waiting, or nil.
func (s *Store) PendingApprovalPolicyChange(ctx context.Context) (*ApprovalPolicyChange, error) {
	c, err := scanPolicyChange(s.db(ctx).QueryRow(ctx, `SELECT `+policyChangeColumns+`
		FROM approval_policy_changes WHERE state = 'pending'`))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// CloseApprovalPolicyChange marks the waiting change id decided or taken
// back. The caller has checked who may.
func (s *Store) CloseApprovalPolicyChange(ctx context.Context, id, state, by, note string) (ApprovalPolicyChange, error) {
	if !uuidRE.MatchString(id) {
		return ApprovalPolicyChange{}, ErrPolicyChangeNotFound
	}
	c, err := scanPolicyChange(s.db(ctx).QueryRow(ctx, `
		UPDATE approval_policy_changes SET state = $2, decided_by = $3, decided_at = now(), note = $4
		 WHERE id = $1::uuid AND state = 'pending' RETURNING `+policyChangeColumns, id, state, by, note))
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrPolicyChangeNotFound
	}
	return c, err
}
