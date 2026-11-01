package agent

import (
	"bufio"
	"context"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

/*
Personal accounts: an account of each person's own on this server, which the
control plane asks for when a team holding the server has them turned on.

The agent creates them, keeps them usable while they are wanted, and locks --
never deletes -- one that is no longer wanted: its home and its history stay,
nobody can log in to it, and anything still running as it is stopped. It only
ever touches accounts it created, which it knows by a marker in the account's
comment field. A name already held by an account it did not create is left
alone and reported as taken.

A person's public key goes into their account's authorized_keys only while a
team of theirs may SSH directly to the server; otherwise the file is emptied.
The file is rewritten on every pass, so a key someone adds by hand does not
last. Sudo, where given, asks no password: these accounts have none.
*/

// PersonalAccount is one personal account as the control plane wants it.
type PersonalAccount struct {
	Name  string `json:"name"`
	Owner string `json:"owner"`
	Sudo  bool   `json:"sudo"`
	// Direct is DirectSSHAllow, DirectSSHApprove, or "" for no direct SSH.
	Direct string   `json:"direct,omitempty"`
	Keys   []string `json:"keys,omitempty"`
}

// PersonalAccountState is what the agent reports about one account.
type PersonalAccountState struct {
	Name   string `json:"name"`
	Owner  string `json:"owner,omitempty"`
	State  string `json:"state"` // ready, locked, taken, failed
	Detail string `json:"detail,omitempty"`
}

// personalMarker starts the comment field of every account this agent made.
const personalMarker = "argus-managed"

var rePersonalName = regexp.MustCompile(`^[a-z_][a-z0-9._-]{0,31}$`)

// SetPersonalAccounts applies the control plane's list. Only accounts that
// are well-formed are kept, so a bad entry cannot become a way in.
func (c *Collector) SetPersonalAccounts(list []PersonalAccount) {
	m := make(map[string]PersonalAccount, len(list))
	for _, p := range list {
		if !rePersonalName.MatchString(p.Name) {
			continue
		}
		switch p.Direct {
		case "", DirectSSHAllow, DirectSSHApprove:
		default:
			p.Direct = ""
		}
		m[p.Name] = p
	}
	c.personal.Store(&m)
}

// PersonalAccount is the personal account by this name, if the control plane
// has said there is one.
func (c *Collector) PersonalAccount(name string) (PersonalAccount, bool) {
	m := c.personal.Load()
	if m == nil {
		return PersonalAccount{}, false
	}
	p, ok := (*m)[name]
	return p, ok
}

// passwdEntry is the part of a passwd(5) line the provisioner needs.
type passwdEntry struct {
	name, comment, home string
	uid, gid            int
}

func (e passwdEntry) ours() bool { return strings.HasPrefix(e.comment, personalMarker) }

func readPasswdEntries(path string) (map[string]passwdEntry, error) {
	rc, err := openPasswd(path)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	out := map[string]passwdEntry{}
	sc := bufio.NewScanner(rc)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := passwdFields(line)
		if len(f) < 7 {
			continue
		}
		uid, _ := strconv.Atoi(f[2])
		gid, _ := strconv.Atoi(f[3])
		out[f[0]] = passwdEntry{name: f[0], comment: f[4], home: f[5], uid: uid, gid: gid}
	}
	return out, sc.Err()
}

// Provisioner keeps this server's personal accounts in step with the
// control plane.
type Provisioner struct {
	// Passwd is the account list to read; "" is the platform's.
	Passwd string
	// Sudoers is the drop-in file that gives sudo to the accounts that have it.
	Sudoers string
	// Run runs a system command. Replaced in tests.
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
	Log *slog.Logger
	// Outside, when set, carries each pass to a process outside the agent's
	// own sandbox (see personal_apply.go). The daemon sets it on a systemd
	// host; nil runs the pass here, as tests and non-systemd hosts do.
	Outside func(ctx context.Context, request []byte) (reply, log []byte, err error)

	// state is what was last done to each account, so an account is
	// unlocked or locked once rather than on every pass.
	state map[string]string
}

// passwd is the account list Reconcile reads. The agent leaves Passwd empty,
// and on Linux "" must mean /etc/passwd: openPasswd reads "" as the platform's
// account directory, which Linux does not have, so every pass failed and no
// personal account was ever made or locked.
func (p *Provisioner) passwd() string {
	if p.Passwd != "" {
		return p.Passwd
	}
	return systemPasswd
}

// commentFor is the comment field of an account made for owner. Anything
// passwd(5) would read as a field separator is dropped.
func commentFor(owner string) string {
	clean := strings.Map(func(r rune) rune {
		if r == ':' || r == ',' || r == '\n' || r == '\r' || r < 0x20 {
			return -1
		}
		return r
	}, owner)
	return personalMarker + " " + clean
}

// Reconcile makes the accounts match want and says what each one is now.
func (p *Provisioner) Reconcile(ctx context.Context, want []PersonalAccount) []PersonalAccountState {
	if p.Outside != nil && personalSupported {
		return p.reconcileOutside(ctx, want)
	}
	return p.reconcile(ctx, want)
}

func (p *Provisioner) reconcile(ctx context.Context, want []PersonalAccount) []PersonalAccountState {
	if p.state == nil {
		p.state = map[string]string{}
	}
	if !personalSupported {
		out := make([]PersonalAccountState, 0, len(want))
		for _, a := range want {
			out = append(out, PersonalAccountState{Name: a.Name, Owner: a.Owner, State: "failed",
				Detail: "personal accounts are made on Linux servers only, so far"})
		}
		return out
	}
	existing, err := readPasswdEntries(p.passwd())
	if err != nil {
		p.Log.Error("personal accounts: cannot read the account list", "error", err)
		out := make([]PersonalAccountState, 0, len(want))
		for _, a := range want {
			out = append(out, PersonalAccountState{Name: a.Name, Owner: a.Owner, State: "failed",
				Detail: "the agent cannot read the server's account list"})
		}
		return out
	}

	sort.Slice(want, func(i, j int) bool { return want[i].Name < want[j].Name })
	wanted := map[string]bool{}
	var states []PersonalAccountState
	var sudo []string
	for _, a := range want {
		st := p.keep(ctx, a, existing)
		wanted[a.Name] = true
		if st.State == "ready" && a.Sudo {
			sudo = append(sudo, a.Name)
		}
		states = append(states, st)
	}

	// Every account this agent made and nobody wants any more is locked.
	var gone []string
	for name, e := range existing {
		if e.ours() && !wanted[name] {
			gone = append(gone, name)
		}
	}
	sort.Strings(gone)
	for _, name := range gone {
		states = append(states, p.lock(ctx, existing[name]))
	}

	if err := writeSudoers(ctx, p, sudo); err != nil {
		p.Log.Error("personal accounts: cannot give sudo", "error", err)
		for i := range states {
			if states[i].State == "ready" && slices.Contains(sudo, states[i].Name) {
				states[i].State, states[i].Detail = "failed", "sudo could not be given: "+err.Error()
			}
		}
	}
	return states
}

// keep creates or reopens one wanted account and writes its keys.
func (p *Provisioner) keep(ctx context.Context, a PersonalAccount, existing map[string]passwdEntry) PersonalAccountState {
	st := PersonalAccountState{Name: a.Name, Owner: a.Owner}
	fail := func(detail string) PersonalAccountState {
		st.State, st.Detail = "failed", detail
		return st
	}
	if !rePersonalName.MatchString(a.Name) {
		return fail("not a name an account can have")
	}
	e, ok := existing[a.Name]
	if ok && !e.ours() {
		st.State = "taken"
		st.Detail = "the server already has an account with this name that Argus did not create; " +
			"give the person another server account name in People"
		return st
	}
	if !ok {
		if out, err := p.Run(ctx, "useradd", "--create-home", "--shell", newAccountShell(),
			"--comment", commentFor(a.Owner), a.Name); err != nil {
			return fail("useradd failed: " + firstLine(out, err))
		}
		p.Log.Warn("personal account created", "account", a.Name, "owner", a.Owner)
		fresh, err := readPasswdEntries(p.passwd())
		if err != nil {
			return fail("the account was created and cannot be read back")
		}
		if e, ok = fresh[a.Name]; !ok {
			return fail("the account was created and is not in the account list")
		}
		p.state[a.Name] = ""
	}
	if p.state[a.Name] != "ready" {
		// No password, which is not the same as a locked one: "*" lets a key
		// in where sshd checks, and "" for the expiry undoes a lock.
		if out, err := p.Run(ctx, "usermod", "--expiredate", "", "--password", "*", a.Name); err != nil {
			return fail("usermod failed: " + firstLine(out, err))
		}
	}
	var keys []string
	if a.Direct != "" {
		keys = a.Keys
	}
	if err := writeAuthorizedKeys(e, keys); err != nil {
		return fail("authorized_keys: " + err.Error())
	}
	p.state[a.Name] = "ready"
	st.State = "ready"
	return st
}

// lock shuts an account that is no longer wanted: its key goes, it is
// locked and expired, and whatever still runs as it is stopped. Its home and
// files stay.
func (p *Provisioner) lock(ctx context.Context, e passwdEntry) PersonalAccountState {
	st := PersonalAccountState{Name: e.name, Owner: strings.TrimSpace(strings.TrimPrefix(e.comment, personalMarker)),
		State: "locked"}
	if err := writeAuthorizedKeys(e, nil); err != nil {
		p.Log.Error("personal accounts: cannot empty authorized_keys", "account", e.name, "error", err)
	}
	if p.state[e.name] == "locked" {
		return st
	}
	if out, err := p.Run(ctx, "usermod", "--lock", "--expiredate", "1", e.name); err != nil {
		st.State, st.Detail = "failed", "could not lock: "+firstLine(out, err)
		return st
	}
	// Exit status 1 is "nothing was running", which is what is hoped for.
	_, _ = p.Run(ctx, "pkill", "-KILL", "-u", e.name)
	p.Log.Warn("personal account locked", "account", e.name)
	p.state[e.name] = "locked"
	return st
}

func firstLine(out []byte, err error) string {
	s := strings.TrimSpace(string(out))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return err.Error()
	}
	return s
}
