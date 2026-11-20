package cloud

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"
)

/*
Risks & waste: what in one account is a security risk, is paid for and not
in use, or is a firewall rule nothing needs. Every finding is a suggestion
worked out from the stored inventory (and, for instances with the Argus
agent, what the agent measured); nothing here calls a provider or changes
anything. A fix is the ordinary change the console already makes, filled in.

Each finding says how it is known, because the console must not show a
rule of thumb as if the cloud had said it:

	SourceCloud     a fact in the last read
	SourceAgent     measured by the Argus agent on the server
	SourceJudgement a threshold Argus chose ("barely used")

A check whose input was not read says nothing, never "fine" and never
"bad": a firewall whose use was not read is not called unused.
*/

// Kinds read once per account rather than per region.
const (
	KindAccount  Kind = "account"  // the account itself: root user settings
	KindUser     Kind = "user"     // an IAM user
	KindBucket   Kind = "bucket"   // an S3 bucket
	KindDatabase Kind = "database" // an RDS instance (read per region)
)

// AccountKinds are the kinds an AccountReader reads.
var AccountKinds = []Kind{KindAccount, KindUser, KindBucket, KindDatabase}

// GlobalRegion is the region account-wide resources are stored under.
const GlobalRegion = "global"

// AccountReader is a provider that reads what belongs to the whole account
// rather than one region: its root user, people and keys, buckets, and
// databases across the regions given. It is optional.
//
// The snapshot's Kinds are only the kinds read in full, so a part that
// failed keeps its last read instead of being marked gone; the error says
// what failed.
type AccountReader interface {
	AccountInventory(ctx context.Context, a Account, regions []string) (Snapshot, error)
}

// Finding groups, severities and sources.
const (
	GroupRisk  = "risk"
	GroupWaste = "waste"
	GroupRules = "rules"

	SeverityHigh   = "high"
	SeverityMedium = "medium"
	SeverityLow    = "low"

	SourceCloud     = "cloud"
	SourceAgent     = "agent"
	SourceJudgement = "judgement"
)

// Thresholds Argus chose. They are judgements, and findings built on them
// say so.
const (
	IdleCPUP95     = 5.0  // percent
	IdleNetBps     = 5000 // bytes a second, in and out together
	IdleNoSession  = 30 * 24 * time.Hour
	IdleMinHistory = 14 * 24 * time.Hour
	UnusedImageAge = 180 * 24 * time.Hour
	KeyUnusedFor   = 90 * 24 * time.Hour
	KeyOldAfter    = 365 * 24 * time.Hour
	ListenFreshFor = 2 * time.Hour
)

// Finding is one thing Risks & waste lists.
type Finding struct {
	// ID is stable for the same problem on the same resource, so a person
	// can keep it.
	ID       string `json:"id"`
	Check    string `json:"check"`
	Group    string `json:"group"`
	Severity string `json:"severity"`
	Source   string `json:"source"`
	Kind     Kind   `json:"kind"`
	// ResourceID and Name are the resource the finding is about.
	ResourceID string `json:"resourceId"`
	Name       string `json:"name"`
	Region     string `json:"region"`
	Title      string `json:"title"`
	Why        string `json:"why"`
	// Matters says why it matters, in a sentence; HowToFix is the fix in
	// words when Argus cannot make it.
	Matters  string `json:"matters,omitempty"`
	HowToFix string `json:"howToFix,omitempty"`
	// Monthly is what it bills a month, for waste; AtMost marks an upper
	// bound.
	Monthly float64    `json:"monthly,omitempty"`
	AtMost  bool       `json:"atMost,omitempty"`
	Since   *time.Time `json:"since,omitempty"`
	// Fix is the change that ends it, "" when Argus has none. Rule is the
	// rule a firewall-rules fix takes away.
	Fix  Action `json:"fix,omitempty"`
	Rule *Rule  `json:"rule,omitempty"`
	// Related names the other resources involved: instances behind a
	// group, a database's groups.
	Related []Ref `json:"related,omitempty"`
	// Basis is a digest of what the finding rests on. A finding kept
	// "until it changes" comes back when its basis differs.
	Basis string `json:"basis"`
}

// ServerFacts is what Argus knows about an instance from its own agent.
// The zero value is an instance without one.
type ServerFacts struct {
	ServerID string
	Hostname string
	// SeenAt is when the agent last checked in.
	SeenAt *time.Time
	// HistoryFor is how far back the usage history goes, up to 30 days.
	HistoryFor time.Duration
	// CPUP95 is the 95th percentile of 5-minute processor use, in percent;
	// NetBps the average bytes a second in and out. Nil when not measured.
	CPUP95 *float64
	NetBps *float64
	// LastSession is the last session anyone opened through Argus.
	LastSession *time.Time
	// Listening is the TCP ports something listens on, reachable from
	// other machines; ListenAt when the agent reported them.
	Listening []int
	ListenAt  *time.Time
}

// HasAgent is an instance matched to an Argus server whose agent has
// checked in.
func (s ServerFacts) HasAgent() bool { return s.ServerID != "" && s.SeenAt != nil }

// Findings works out one account's risks and waste from its stored
// inventory (every region, plus the account-wide kinds) and what agents
// measured, keyed by instance id. Highest severity first, then biggest bill.
func Findings(rs []Resource, facts map[string]ServerFacts, now time.Time) []Finding {
	return FindingsIn("", rs, facts, now)
}

// FindingsIn is Findings for an account of the provider named, so what is
// found is priced and worded as that cloud's. "" is AWS.
func FindingsIn(provider string, rs []Resource, facts map[string]ServerFacts, now time.Time) []Finding {
	f := finder{provider: provider, now: now, facts: facts, byID: map[string]Resource{}, referenced: map[string]bool{}, imageUsed: map[string]bool{}}
	for _, r := range rs {
		f.byID[r.ID] = r
		switch r.Kind {
		case KindFirewall:
			for _, x := range r.Rules {
				if strings.HasPrefix(x.Source, "sg-") && x.Source != r.ID {
					f.referenced[x.Source] = true
				}
			}
		case KindInstance:
			if im := r.Details["image"]; im != "" {
				f.imageUsed[im] = true
			}
			for _, l := range r.Links {
				if l.Kind == KindFirewall {
					f.groupInstances = appendRef(f.groupInstances, l.ID, r)
				}
			}
		}
	}
	for _, x := range IdleIn(provider, rs, now) {
		f.idle(x)
	}
	f.reservations(rs)
	for _, r := range rs {
		switch r.Kind {
		case KindFirewall:
			f.firewall(r)
		case KindInstance:
			f.instance(r)
		case KindVolume:
			if r.Details["encrypted"] == "no" {
				f.add(Finding{Check: "disk-unencrypted", Group: GroupRisk, Severity: SeverityMedium, Source: SourceCloud, Title: "Disk not encrypted",
					Why:      fmt.Sprintf("%s (%s) is stored unencrypted.", r.Name, r.Size),
					Matters:  "Anyone who gets a copy of the disk or its snapshots can read it.",
					HowToFix: "Snapshot it, copy the snapshot with encryption on, make a disk from the copy and swap it in. Turn on EBS encryption by default for the region so new disks are encrypted."}, r, "no")
			}
		case KindSnapshot:
			if r.Details["public"] == "yes" {
				f.add(Finding{Check: "snapshot-public", Group: GroupRisk, Severity: SeverityHigh, Source: SourceCloud, Title: "Snapshot shared with everyone",
					Why:      fmt.Sprintf("%s (%s) can be made into a disk by any AWS account.", r.Name, r.Size),
					Matters:  "Whatever was on the disk, files, keys, database data, can be read by anyone who finds it.",
					HowToFix: "In AWS, change the snapshot's permissions to private, or delete it if nothing needs it."}, r, "public")
			}
		case KindImage:
			f.image(r)
		case KindAccount:
			f.account(r)
		case KindUser:
			f.user(r)
		case KindBucket:
			f.bucket(r)
		case KindDatabase:
			f.database(r)
		}
	}
	sevRank := map[string]int{SeverityHigh: 0, SeverityMedium: 1, SeverityLow: 2}
	grpRank := map[string]int{GroupRisk: 0, GroupWaste: 1, GroupRules: 2}
	slices.SortStableFunc(f.out, func(a, b Finding) int {
		if d := grpRank[a.Group] - grpRank[b.Group]; d != 0 {
			return d
		}
		if d := sevRank[a.Severity] - sevRank[b.Severity]; d != 0 {
			return d
		}
		switch {
		case a.Monthly > b.Monthly:
			return -1
		case a.Monthly < b.Monthly:
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	return f.out
}

type finder struct {
	// provider is the cloud the inventory is of; "" is AWS.
	provider       string
	now            time.Time
	facts          map[string]ServerFacts
	byID           map[string]Resource
	referenced     map[string]bool
	imageUsed      map[string]bool
	groupInstances map[string][]Resource
	out            []Finding
}

// cloudName is the provider as a sentence names it.
func (f *finder) cloudName() string {
	switch f.provider {
	case "azure":
		return "Azure"
	case "gcp":
		return "Google Cloud"
	}
	return "AWS"
}

func appendRef(m map[string][]Resource, k string, r Resource) map[string][]Resource {
	if m == nil {
		m = map[string][]Resource{}
	}
	m[k] = append(m[k], r)
	return m
}

func (f *finder) add(x Finding, r Resource, basis string) {
	x.Kind, x.ResourceID, x.Name, x.Region = r.Kind, r.ID, r.Name, r.Region
	if x.ID == "" {
		x.ID = x.Check + ":" + r.ID
	}
	sum := sha256.Sum256([]byte(x.Check + "\x00" + basis))
	x.Basis = hex.EncodeToString(sum[:8])
	if x.Monthly != 0 {
		x.Monthly = round2(x.Monthly)
	}
	f.out = append(f.out, x)
}

// idle turns a cost-side idle resource into a waste finding, so Costs and
// Risks & waste agree by construction.
func (f *finder) idle(x IdleFinding) {
	r, ok := f.byID[x.ID]
	if !ok {
		r = Resource{Kind: x.Kind, ID: x.ID, Name: x.Name, Region: x.Region}
	}
	title := map[Kind]string{KindVolume: "Disk attached to nothing", KindAddress: "Elastic IP attached to nothing",
		KindInstance: "Stopped for a long time", KindSnapshot: "Snapshot of a disk that is gone", KindLoadBalancer: "Load balancer with nothing behind it"}[x.Kind]
	if title == "" {
		title = "Not in use"
	}
	if x.Kind == KindAddress && f.provider == "azure" {
		title = "Public address attached to nothing"
	}
	if x.Kind == KindInstance && x.Since != nil {
		title = "Stopped for " + daysWords(f.now.Sub(*x.Since))
	}
	if x.Title != "" {
		title = x.Title
	}
	f.add(Finding{Check: "idle-" + string(x.Kind), Group: GroupWaste, Severity: SeverityLow, Source: SourceCloud, Title: title,
		Why: x.Why + ".", Monthly: x.Monthly, AtMost: x.AtMost, Since: x.Since, Fix: x.Fix}, r, r.State)
}

func (f *finder) firewall(r Resource) {
	users, usersRead := r.Details["interfaces"]
	insts := f.groupInstances[r.ID]
	used := len(insts) > 0 || (usersRead && users != "0")
	unused := usersRead && users == "0" && len(insts) == 0

	if unused && r.Details["default"] != "yes" && !f.referenced[r.ID] {
		why := fmt.Sprintf("No instance, load balancer, database or other network interface uses %s, and no other group's rule names it.", r.Name)
		if f.provider == "azure" {
			why = fmt.Sprintf("No network interface and no subnet uses %s.", r.Name)
		}
		f.add(Finding{Check: "group-unused", Group: GroupRules, Severity: SeverityLow, Source: SourceCloud, Title: "Security group used by nothing",
			Why:     why,
			Matters: "An unused group is easy to attach by mistake later, with whatever rules it still has.",
			Fix:     ActionFirewallDelete}, r, "unused")
	}

	var related []Ref
	for _, i := range insts {
		related = append(related, Ref{Kind: KindInstance, ID: i.ID, Name: i.Name})
	}
	allAgents := len(insts) > 0 && usersRead && users == strconv.Itoa(len(insts))
	for _, i := range insts {
		if !f.facts[i.ID].HasAgent() {
			allAgents = false
		}
	}

	for idx, x := range r.Rules {
		if !x.Inbound {
			continue
		}
		rule := x
		key := ruleKey(x)
		if exp := x.Exposure(); exp != "" && !unused {
			sev, title := SeverityMedium, fmt.Sprintf("Port %s open to the whole internet", x.PortText())
			risky := exp == "every port" || strings.Contains(exp, "(")
			if risky {
				sev = SeverityHigh
				title = openTitle(exp) + " open to the whole internet"
			}
			why := fmt.Sprintf("%s lets %s reach %s.", r.Name, x.Source, exp)
			if len(insts) > 0 {
				why = fmt.Sprintf("%s lets %s reach %s. Used by %s.", r.Name, x.Source, exp, refNames(related))
			}
			matters := "Anyone on the internet can try to reach it."
			if allAgents {
				matters += " Every instance behind it has the Argus agent, so nobody needs this rule to sign in through Argus."
			}
			f.add(Finding{ID: "open:" + r.ID + ":" + key, Check: "rule-open", Group: GroupRisk, Severity: sev, Source: SourceCloud, Title: title,
				Why: why, Matters: matters, Fix: ActionFirewallRules, Rule: &rule, Related: related}, r, key)
			continue
		}
		if !x.Open() && allAgents && r.Details["default"] != "yes" && (x.Covers(22) || x.Covers(3389)) && x.Protocol != "all" {
			svc := "SSH"
			if x.Covers(3389) {
				svc = "Remote Desktop"
			}
			f.add(Finding{ID: "agentonly:" + r.ID + ":" + key, Check: "rule-not-needed", Group: GroupRules, Severity: SeverityLow, Source: SourceJudgement,
				Title:   svc + " allowed in, but Argus does not need it",
				Why:     fmt.Sprintf("%s lets %s reach %s. Every instance behind it (%s) has the Argus agent, which needs no inbound port.", r.Name, x.Source, x.PortText(), refNames(related)),
				Matters: "Fewer open ports means less to attack. Keep it if people still connect without Argus, for example from a bastion.",
				Fix:     ActionFirewallRules, Rule: &rule, Related: related}, r, key)
		}
		if by := coveredBy(r.Rules, idx); by != nil {
			f.add(Finding{ID: "covered:" + r.ID + ":" + key, Check: "rule-covered", Group: GroupRules, Severity: SeverityLow, Source: SourceCloud,
				Title: "Rule already covered by another",
				Why:   fmt.Sprintf("%s allows %s from %s, which %s from %s already allows.", r.Name, x.PortText(), sourceWords(x.Source), by.PortText(), sourceWords(by.Source)),
				Fix:   ActionFirewallRules, Rule: &rule}, r, key)
			continue
		}
		if allAgents && x.Protocol == "tcp" && x.ToPort-x.FromPort < 100 {
			if lines, quiet := f.nothingListens(insts, x); quiet {
				f.add(Finding{ID: "quiet:" + r.ID + ":" + key, Check: "rule-nothing-listens", Group: GroupRules, Severity: SeverityLow, Source: SourceAgent,
					Title:   "Port open but nothing listens",
					Why:     fmt.Sprintf("%s lets %s reach %s. The agent on %s sees nothing listening there.", r.Name, sourceWords(x.Source), x.PortText(), everyWord(len(insts))),
					Matters: "It may be left over from an old service. " + f.cloudName() + " does not record whether a rule was ever used, so this is from what listens now. " + lines,
					Fix:     ActionFirewallRules, Rule: &rule, Related: related}, r, key)
			}
		}
	}

	if r.Details["default"] == "yes" && used {
		for _, x := range r.Rules {
			if x.Inbound {
				f.add(Finding{Check: "group-default-open", Group: GroupRisk, Severity: SeverityLow, Source: SourceCloud, Title: "Default security group lets traffic in",
					Why:      fmt.Sprintf("The default group of %s has inbound rules and is in use.", r.Network),
					Matters:  "Anything launched without choosing a group gets it, often by accident.",
					HowToFix: "Give what uses it its own group, then remove the default group's inbound rules."}, r, "default-in")
				break
			}
		}
	}
}

// nothingListens reports whether no instance listens on any port of rule,
// with every instance's report fresh. Lines lists what each one listens on.
func (f *finder) nothingListens(insts []Resource, x Rule) (string, bool) {
	var lines []string
	for _, i := range insts {
		s := f.facts[i.ID]
		if s.ListenAt == nil || f.now.Sub(*s.ListenAt) > ListenFreshFor {
			return "", false
		}
		ports := slices.Clone(s.Listening)
		slices.Sort(ports)
		for _, p := range ports {
			if int32(p) >= x.FromPort && int32(p) <= x.ToPort {
				return "", false
			}
		}
		strs := make([]string, len(ports))
		for k, p := range ports {
			strs[k] = strconv.Itoa(p)
		}
		lines = append(lines, fmt.Sprintf("%s listens on %s.", i.Name, orNone(strings.Join(strs, ", "))))
	}
	return strings.Join(lines, " "), true
}

func (f *finder) instance(r Resource) {
	s := f.facts[r.ID]
	live := r.State == "running" || r.State == "stopped"
	if r.State == "running" && r.PublicIP != "" && !s.HasAgent() {
		f.add(Finding{Check: "public-no-agent", Group: GroupRisk, Severity: SeverityMedium, Source: SourceCloud, Title: "Public instance without the Argus agent",
			Why:      fmt.Sprintf("%s has the public IP %s and no Argus server matches it.", r.Name, r.PublicIP),
			Matters:  "It can be reached from the internet, and sessions on it are not recorded by Argus.",
			HowToFix: "Add it as a server and install the agent, or take away its public IP if nothing needs it."}, r, r.PublicIP)
	}
	if r.State == "running" && (r.Details["systemStatus"] == "impaired" || r.Details["instanceStatus"] == "impaired") {
		which, fix := "its own", "Look at its boot log from its panel, then reboot it. If it stays impaired, stop and start it, which moves it to other hardware."
		if r.Details["systemStatus"] == "impaired" {
			which, fix = "AWS's hardware or network", "Stop and start it, which moves it to healthy hardware. A reboot does not."
		}
		f.add(Finding{Check: "status-impaired", Group: GroupRisk, Severity: SeverityHigh, Source: SourceCloud, Title: "Failing AWS health checks",
			Why:      fmt.Sprintf("AWS reports a problem with %s for %s.", which, r.Name),
			Matters:  "It may not answer at all, and AWS will not fix a problem inside the instance by itself.",
			HowToFix: fix}, r, r.Details["systemStatus"]+"/"+r.Details["instanceStatus"])
	}
	if code := r.Details["eventCode"]; code != "" && live {
		when := "soon"
		if t, err := time.Parse(time.RFC3339, r.Details["eventNotBefore"]); err == nil {
			when = "from " + t.Format("2 Jan 2006 15:04 UTC")
		}
		f.add(Finding{Check: "scheduled-event", Group: GroupRisk, Severity: SeverityMedium, Source: SourceCloud, Title: "AWS has planned maintenance",
			Why:      fmt.Sprintf("%s: %s (%s).", r.Name, r.Details["eventDescription"], when),
			Matters:  "AWS will reboot, stop or retire it on its own schedule, possibly at a busy time.",
			HowToFix: "Stop and start it yourself at a quiet time before then; that usually clears a retirement or a hardware reboot."}, r, code+"/"+r.Details["eventNotBefore"])
	}
	if live && r.Details["metadataTokens"] == "optional" {
		f.add(Finding{Check: "imds-v1", Group: GroupRisk, Severity: SeverityMedium, Source: SourceCloud, Title: "Old instance metadata allowed",
			Why:      fmt.Sprintf("%s still answers metadata requests without a token (IMDSv1).", r.Name),
			Matters:  "A server-side request forgery bug in anything it runs can read its role's keys.",
			HowToFix: "Require tokens (IMDSv2). Most software since 2020 already uses them; check anything old on it first.",
			Fix:      ActionSettings}, r, "optional")
	}
	if r.State != "running" || !s.HasAgent() || s.CPUP95 == nil || s.HistoryFor < IdleMinHistory {
		return
	}
	if *s.CPUP95 >= IdleCPUP95 || (s.NetBps != nil && *s.NetBps >= IdleNetBps) {
		return
	}
	if s.LastSession != nil && f.now.Sub(*s.LastSession) < IdleNoSession {
		return
	}
	var m float64
	if p, ok := HourlyPrice(r.Size); ok {
		m = p * HoursPerMonth
	}
	session := "no Argus session in the last 30 days"
	if s.LastSession != nil {
		session = "no Argus session for " + daysWords(f.now.Sub(*s.LastSession))
	}
	f.add(Finding{Check: "instance-idle", Group: GroupWaste, Severity: SeverityMedium, Source: SourceJudgement, Title: "Running, but barely used",
		Why: fmt.Sprintf("Processor at most %.1f%% for 95%% of the time, %s, over the agent's last %s of readings.",
			*s.CPUP95, session, daysWords(s.HistoryFor)),
		Matters: "If nothing needs it, stopping it ends the instance bill; its disks still bill.",
		Monthly: m, Fix: ActionStop, Related: []Ref{{Kind: "server", ID: s.ServerID, Name: s.Hostname}}}, r, r.Size)
}

func (f *finder) image(r Resource) {
	if r.Details["public"] == "yes" {
		f.add(Finding{Check: "image-public", Group: GroupRisk, Severity: SeverityHigh, Source: SourceCloud, Title: "Image shared with everyone",
			Why:      fmt.Sprintf("%s (%s) can be launched by any AWS account.", r.Name, r.ID),
			Matters:  "Anyone can start a copy and read what is on its disks: keys, configuration, data.",
			HowToFix: "In AWS, make the image private, or deregister it if nothing needs it."}, r, "public")
	}
	if r.Details["templatesChecked"] != "yes" || r.Details["inTemplate"] == "yes" || f.imageUsed[r.ID] || r.CreatedAt == nil ||
		f.now.Sub(*r.CreatedAt) < UnusedImageAge {
		return
	}
	f.add(Finding{Check: "image-unused", Group: GroupWaste, Severity: SeverityLow, Source: SourceCloud, Title: "Image nothing uses",
		Why:     fmt.Sprintf("%s old; no instance or launch template in the account uses it. Auto Scaling launch configurations are not read.", daysWords(f.now.Sub(*r.CreatedAt))),
		Monthly: float64(GiB(r.Size)) * SnapshotPerGB, AtMost: true, Since: r.CreatedAt, Fix: ActionImageDelete}, r, "unused")
}

func (f *finder) account(r Resource) {
	if r.Details["rootMfa"] == "no" {
		f.add(Finding{Check: "root-no-mfa", Group: GroupRisk, Severity: SeverityHigh, Source: SourceCloud, Title: "Root user has no MFA",
			Why:      "The account's root user signs in with a password alone.",
			Matters:  "Root can do anything in the account, including close it. A leaked password is enough.",
			HowToFix: "Sign in as root and add an MFA device under Security credentials."}, r, "no-mfa")
	}
	if n, _ := strconv.Atoi(r.Details["rootKeys"]); n > 0 {
		f.add(Finding{Check: "root-keys", Group: GroupRisk, Severity: SeverityHigh, Source: SourceCloud, Title: "Root user has access keys",
			Why:      fmt.Sprintf("The root user has %d active access key%s.", n, plural(n)),
			Matters:  "A root key is full control of the account from anywhere, and cannot be limited by a policy.",
			HowToFix: "Sign in as root and delete the keys. Use an IAM role for anything that needed them."}, r, "keys")
	}
}

func (f *finder) user(r Resource) {
	d := r.Details
	if d["password"] == "yes" && d["mfa"] == "no" {
		f.add(Finding{Check: "user-no-mfa", Group: GroupRisk, Severity: SeverityMedium, Source: SourceCloud, Title: "Person signs in without MFA",
			Why:      fmt.Sprintf("IAM user %s has a console password and no MFA device.", r.Name),
			Matters:  "A leaked or guessed password is enough to get into the AWS console as them.",
			HowToFix: "Add an MFA device for the user, or remove the console password if they do not sign in."}, r, "no-mfa")
	}
	for _, k := range []string{"1", "2"} {
		if d["key"+k+"Active"] != "yes" {
			continue
		}
		rotated := parseTime(d["key"+k+"Rotated"])
		used := parseTime(d["key"+k+"LastUsed"])
		id := "key" + k + ":" + r.ID
		switch {
		case used == nil && rotated != nil && f.now.Sub(*rotated) >= KeyUnusedFor:
			f.add(Finding{ID: "unused-" + id, Check: "key-unused", Group: GroupRisk, Severity: SeverityMedium, Source: SourceCloud, Title: "Access key never used",
				Why:      fmt.Sprintf("%s's access key %s was made %s ago and has never been used.", r.Name, k, daysWords(f.now.Sub(*rotated))),
				Matters:  "A key nobody uses is still a working key if it leaks.",
				HowToFix: "Make the key inactive in IAM, then delete it once nothing breaks."}, r, "key"+k+"never")
		case used != nil && f.now.Sub(*used) >= KeyUnusedFor:
			f.add(Finding{ID: "unused-" + id, Check: "key-unused", Group: GroupRisk, Severity: SeverityMedium, Source: SourceCloud, Title: "Access key not used for " + daysWords(f.now.Sub(*used)),
				Why:      fmt.Sprintf("%s's access key %s was last used on %s.", r.Name, k, used.Format(time.DateOnly)),
				Matters:  "A key nobody uses is still a working key if it leaks.",
				HowToFix: "Make the key inactive in IAM, then delete it once nothing breaks."}, r, "key"+k+"unused")
		case rotated != nil && f.now.Sub(*rotated) >= KeyOldAfter:
			f.add(Finding{ID: "old-" + id, Check: "key-old", Group: GroupRisk, Severity: SeverityLow, Source: SourceCloud, Title: "Access key older than a year",
				Why:      fmt.Sprintf("%s's access key %s was made %s ago and is still in use.", r.Name, k, daysWords(f.now.Sub(*rotated))),
				Matters:  "The longer a key lives, the more places it has been copied to.",
				HowToFix: "Make a new key, move what uses the old one, then delete the old one."}, r, "key"+k+"old")
		}
	}
}

func (f *finder) bucket(r Resource) {
	switch r.Details["publicBlock"] {
	case "none", "partial":
		what := "Nothing blocks public access"
		if r.Details["publicBlock"] == "partial" {
			what = "Public access is only partly blocked"
		}
		f.add(Finding{Check: "bucket-public-block", Group: GroupRisk, Severity: SeverityMedium, Source: SourceCloud, Title: "Bucket can be made public",
			Why:      fmt.Sprintf("%s on %s. An account-wide block, if there is one, still applies; Argus cannot read it with its current permissions.", what, r.Name),
			Matters:  "One policy or ACL change, by mistake or not, puts its files on the internet.",
			HowToFix: "Turn on all four Block Public Access settings for the bucket, unless it serves a public website on purpose."}, r, r.Details["publicBlock"])
	}
}

func (f *finder) database(r Resource) {
	if r.Details["public"] == "yes" {
		port, _ := strconv.Atoi(r.Details["port"])
		open := false
		for _, l := range r.Links {
			if g, ok := f.byID[l.ID]; ok && l.Kind == KindFirewall {
				for _, x := range g.Rules {
					if x.Open() && (x.Protocol == "all" || x.Covers(int32(port))) {
						open = true
					}
				}
			}
		}
		x := Finding{Check: "database-public", Group: GroupRisk, Severity: SeverityMedium, Source: SourceCloud, Title: "Database reachable from the internet",
			Why:      fmt.Sprintf("%s (%s) has a public address. Its security groups decide who can connect.", r.Name, r.Details["engine"]),
			Matters:  "A public database is one firewall mistake away from being open to everyone.",
			HowToFix: "In RDS, turn off Publicly accessible and reach it from inside the network.", Related: r.Links}
		if open {
			x.Severity, x.Title = SeverityHigh, "Database open to the whole internet"
			x.Why = fmt.Sprintf("%s (%s) has a public address, and a security group lets 0.0.0.0/0 reach port %d.", r.Name, r.Details["engine"], port)
		}
		f.add(x, r, fmt.Sprint(open))
	}
	if r.Details["encrypted"] == "no" {
		f.add(Finding{Check: "database-unencrypted", Group: GroupRisk, Severity: SeverityMedium, Source: SourceCloud, Title: "Database not encrypted",
			Why:      fmt.Sprintf("%s (%s) stores its data and backups unencrypted.", r.Name, r.Details["engine"]),
			Matters:  "Anyone who gets a copy of a snapshot can read it.",
			HowToFix: "Snapshot it, copy the snapshot with encryption on, and restore a new database from the copy."}, r, "no")
	}
}

// coveredBy is a wider inbound rule in the same group that already allows
// what rules[i] allows, or nil. Only address ranges are compared; a rule
// naming another group is never called covered.
func coveredBy(rules []Rule, i int) *Rule {
	a := rules[i]
	pa, err := netip.ParsePrefix(a.Source)
	if err != nil {
		return nil
	}
	for j, b := range rules {
		if j == i || !b.Inbound {
			continue
		}
		pb, err := netip.ParsePrefix(b.Source)
		if err != nil || pa.Addr().Is4() != pb.Addr().Is4() {
			continue
		}
		if b.Protocol != "all" && (b.Protocol != a.Protocol || b.FromPort > a.FromPort || b.ToPort < a.ToPort) {
			continue
		}
		if pb.Bits() > pa.Bits() || !pb.Contains(pa.Addr()) {
			continue
		}
		// Identical rules cover each other; flag only the later one.
		if pb.Bits() == pa.Bits() && (b.Protocol == a.Protocol && b.FromPort == a.FromPort && b.ToPort == a.ToPort) && j > i {
			continue
		}
		bb := b
		return &bb
	}
	return nil
}

func ruleKey(x Rule) string {
	return fmt.Sprintf("%s/%d-%d/%s", x.Protocol, x.FromPort, x.ToPort, x.Source)
}

func openTitle(exp string) string {
	if exp == "every port" {
		return "Every port"
	}
	names := strings.Split(exp, ", ")
	for i, n := range names {
		if k := strings.Index(n, " ("); k > 0 {
			names[i] = n[:k]
		}
	}
	if len(names) > 2 {
		return strings.Join(names[:2], ", ") + " and more"
	}
	return strings.Join(names, " and ")
}

func sourceWords(s string) string {
	if s == "0.0.0.0/0" || s == "::/0" {
		return "anywhere"
	}
	return s
}

func refNames(refs []Ref) string {
	var n []string
	for _, r := range refs {
		n = append(n, r.Name)
	}
	if len(n) > 3 {
		return strings.Join(n[:3], ", ") + fmt.Sprintf(" and %d more", len(n)-3)
	}
	switch len(n) {
	case 0:
		return "nothing"
	case 1:
		return n[0]
	}
	return strings.Join(n[:len(n)-1], ", ") + " and " + n[len(n)-1]
}

func everyWord(n int) string {
	if n == 1 {
		return "its only instance"
	}
	return "all " + strconv.Itoa(n) + " instances"
}

func orNone(s string) string {
	if s == "" {
		return "nothing"
	}
	return s
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func parseTime(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return &t
}
