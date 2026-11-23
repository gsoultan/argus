package agent

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
The Users tab of a Kafka entry: the SCRAM users and certificate principals
that applications sign in with, and their ACLs inside the entry's scope (see
tunnel/brokerusers.go for the rules).

These run as Kafka's administrator from the admin file, as giving a person
their session user does, because a person's own user may not see or change
other users. So the agent checks here what Kafka would not: the scope, the
level, Argus's own users, the admin user itself, and that a user with ACLs
outside the scope is not deleted or given a new password from an entry that
cannot see them.

An ACL is deleted only by an exact filter for one ACL that was read back
first, never by a wildcard.
*/

// KafkaUserInfo is one user as the Users tab shows it.
type KafkaUserInfo struct {
	Name string `json:"name"`
	// Mechanisms are its SCRAM credentials; none is a principal that signs
	// in another way, as a TLS certificate's CN=… does.
	Mechanisms []string `json:"mechanisms,omitempty"`
	// ACLs are those inside the entry; Outside those it has elsewhere on
	// the cluster, shown as a count and changed only from a * entry.
	ACLs    []tunnel.KafkaACL  `json:"acls"`
	Outside []tunnel.KafkaACL  `json:"outside,omitempty"`
	Quotas  tunnel.KafkaQuotas `json:"quotas"`
	// Self is the user the agent signs in as: shown, never changed.
	Self bool `json:"self,omitempty"`
}

// KafkaUsersData answers users.
type KafkaUsersData struct {
	Users []KafkaUserInfo `json:"users"`
	// Sessions are Argus's own users, open now.
	Sessions []string `json:"sessions"`
	// NotShown counts the users on the cluster with nothing in this entry.
	NotShown int    `json:"notShown"`
	Scope    string `json:"scope"`
}

// KafkaUserResult answers a change.
type KafkaUserResult struct {
	User    string `json:"user"`
	Added   int    `json:"added,omitempty"`
	Removed int    `json:"removed,omitempty"`
}

// KafkaLoginResult answers the sign-in check.
type KafkaLoginResult struct {
	User    string `json:"user"`
	Address string `json:"address"`
	OK      bool   `json:"ok"`
	Detail  string `json:"detail,omitempty"`
	// Topics are those the user sees in this entry; Elsewhere counts those
	// it sees outside it.
	Topics    []string `json:"topics,omitempty"`
	Elsewhere int      `json:"elsewhere,omitempty"`
}

// kafkaQuotaKeys are Kafka's names for a user's quotas.
const (
	quotaProduce = "producer_byte_rate"
	quotaConsume = "consumer_byte_rate"
	quotaRequest = "request_percentage"
)

// kafkaUserState is everything one user has on the cluster.
type kafkaUserState struct {
	exists  bool
	mechs   []string
	inView  []tunnel.KafkaACL
	outside []tunnel.KafkaACL
	quotas  tunnel.KafkaQuotas
}

// kafkaACLOf reads a described ACL as the user it names and the ACL; ok is
// false for a principal that is not a user.
func kafkaACLOf(d kadm.DescribedACL) (string, tunnel.KafkaACL, bool) {
	user, ok := strings.CutPrefix(d.Principal, "User:")
	if !ok {
		return "", tunnel.KafkaACL{}, false
	}
	return user, tunnel.KafkaACL{
		Resource:   strings.ToLower(d.Type.String()),
		Pattern:    d.Pattern.String(),
		Name:       d.Name,
		Operation:  d.Operation.String(),
		Permission: d.Permission.String(),
		Host:       d.Host,
	}, true
}

// kafkaACLBuilder is one exact ACL, to create or to delete.
func kafkaACLBuilder(user string, a tunnel.KafkaACL) (*kadm.ACLBuilder, error) {
	op, err := kmsg.ParseACLOperation(a.Operation)
	if err != nil {
		return nil, err
	}
	b := kadm.NewACLs()
	p := kafkaPrincipal(user)
	if a.Permission == "DENY" {
		b.Deny(p).DenyHosts(a.Host)
	} else {
		b.Allow(p).AllowHosts(a.Host)
	}
	switch a.Resource {
	case "topic":
		b.Topics(a.Name)
	case "group":
		b.Groups(a.Name)
	case "transactional_id":
		b.TransactionalIDs(a.Name)
	default:
		return nil, errors.New("not an ACL Argus writes: " + a.String())
	}
	pat := kadm.ACLPatternLiteral
	if a.Pattern == "PREFIXED" {
		pat = kadm.ACLPatternPrefixed
	}
	return b.ResourcePatternType(pat).Operations(op), nil
}

// allKafkaACLs reads every ACL on the cluster, by user.
func allKafkaACLs(ctx context.Context, adm *kadm.Client) (map[string][]tunnel.KafkaACL, error) {
	res, err := adm.DescribeACLs(ctx, kadm.NewACLs().AnyResource().Allow().AllowHosts().Deny().DenyHosts().
		ResourcePatternType(kadm.ACLPatternAny).Operations())
	if err != nil {
		return nil, err
	}
	out := map[string][]tunnel.KafkaACL{}
	seen := map[string]bool{}
	for _, r := range res {
		if r.Err != nil {
			return nil, r.Err
		}
		for _, d := range r.Described {
			user, a, ok := kafkaACLOf(d)
			if !ok || seen[user+"\x00"+a.Key()] {
				continue
			}
			seen[user+"\x00"+a.Key()] = true
			out[user] = append(out[user], a)
		}
	}
	return out, nil
}

// kafkaQuotasOf reads users' quotas, by user.
func kafkaQuotasOf(ctx context.Context, adm *kadm.Client) map[string]tunnel.KafkaQuotas {
	out := map[string]tunnel.KafkaQuotas{}
	qs, err := adm.DescribeClientQuotas(ctx, false, []kadm.DescribeClientQuotaComponent{{Type: "user", MatchType: 2}})
	if err != nil {
		return out
	}
	for _, q := range qs {
		if len(q.Entity) != 1 || q.Entity[0].Type != "user" || q.Entity[0].Name == nil {
			continue
		}
		var v tunnel.KafkaQuotas
		for _, x := range q.Values {
			switch x.Key {
			case quotaProduce:
				v.ProduceBytes = x.Value
			case quotaConsume:
				v.ConsumeBytes = x.Value
			case quotaRequest:
				v.RequestPct = x.Value
			}
		}
		out[*q.Entity[0].Name] = v
	}
	return out
}

// split divides a user's ACLs into those inside the scope and the rest.
// Group and transactional ID ACLs go with the topics: in view where the
// user has a topic ACL in view, or on a * entry.
func splitKafkaACLs(scope string, acls []tunnel.KafkaACL) (in, out []tunnel.KafkaACL) {
	topicHere := scope == tunnel.KafkaAllTopics
	for _, a := range acls {
		if a.Resource == "topic" && tunnel.KafkaACLInScope(scope, a) {
			topicHere = true
		}
	}
	for _, a := range acls {
		switch {
		case a.Resource == "topic" && tunnel.KafkaACLInScope(scope, a):
			in = append(in, a)
		case (a.Resource == "group" || a.Resource == "transactional_id") && topicHere:
			in = append(in, a)
		default:
			out = append(out, a)
		}
	}
	tunnel.SortKafkaACLs(in)
	tunnel.SortKafkaACLs(out)
	return in, out
}

func scramNames(d kadm.DescribedUserSCRAM) []string {
	var out []string
	for _, c := range d.CredInfos {
		out = append(out, c.Mechanism.String())
	}
	sort.Strings(out)
	return out
}

// kafkaUser reads one user's state.
func (s *kafkaSession) kafkaUser(ctx context.Context, adm *kadm.Client, name string) (kafkaUserState, error) {
	var st kafkaUserState
	scrams, err := adm.DescribeUserSCRAMs(ctx, name)
	if err == nil {
		if d, ok := scrams[name]; ok && d.Err == nil {
			st.mechs = scramNames(d)
		}
	} else if !isKafkaNotFound(err) {
		return st, err
	}
	acls, err := allKafkaACLs(ctx, adm)
	if err != nil {
		return st, err
	}
	st.inView, st.outside = splitKafkaACLs(s.db.Name, acls[name])
	st.quotas = kafkaQuotasOf(ctx, adm)[name]
	st.exists = len(st.mechs) > 0 || len(acls[name]) > 0
	return st, nil
}

func isKafkaNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "RESOURCE_NOT_FOUND")
}

// users answers the Users tab.
func (s *kafkaSession) users(ctx context.Context, adm *kadm.Client, admin string) (tunnel.StoreAnswer, error) {
	scrams, err := adm.DescribeUserSCRAMs(ctx)
	if err != nil {
		return kafkaErrAnswer(fmt.Errorf("Kafka would not list its users: %w", err)), nil
	}
	acls, err := allKafkaACLs(ctx, adm)
	if err != nil {
		return kafkaErrAnswer(fmt.Errorf("Kafka would not list its ACLs: %w", err)), nil
	}
	quotas := kafkaQuotasOf(ctx, adm)
	names := map[string]bool{}
	for u := range scrams {
		names[u] = true
	}
	for u := range acls {
		names[u] = true
	}
	data := KafkaUsersData{Users: []KafkaUserInfo{}, Sessions: []string{}, Scope: s.db.Name}
	for u := range names {
		if tunnel.IsStoreUser(u) {
			data.Sessions = append(data.Sessions, u)
			continue
		}
		in, out := splitKafkaACLs(s.db.Name, acls[u])
		hasTopic := false
		for _, a := range in {
			if a.Resource == "topic" {
				hasTopic = true
			}
		}
		if s.db.Name != tunnel.KafkaAllTopics && !hasTopic && u != admin {
			data.NotShown++
			continue
		}
		info := KafkaUserInfo{Name: u, ACLs: in, Outside: out, Quotas: quotas[u], Self: u == admin}
		if info.ACLs == nil {
			info.ACLs = []tunnel.KafkaACL{}
		}
		if d, ok := scrams[u]; ok {
			info.Mechanisms = scramNames(d)
		}
		data.Users = append(data.Users, info)
	}
	sort.Slice(data.Users, func(i, j int) bool { return data.Users[i].Name < data.Users[j].Name })
	sort.Strings(data.Sessions)
	return tunnel.StoreAnswer{Type: "result", Total: int64(len(data.Users)), Data: data}, nil
}

// userOp answers the Users tab's operations.
func (s *kafkaSession) userOp(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if tunnel.LevelRank(s.db.Level) < tunnel.LevelRank(tunnel.BrokerUserOpLevel(req.Op)) {
		return tunnel.StoreAnswer{Type: "error", Error: "this needs " + tunnel.BrokerUserOpLevel(req.Op) + " or above; this session is at " + s.db.Level}, nil
	}
	if why := tunnel.ValidKafkaUserChange(s.db.Name, req); why != "" {
		return tunnel.StoreAnswer{Type: "error", Error: why}, nil
	}
	text := tunnel.KafkaUserText(s.db.Name, req)
	if tunnel.BrokerUserChanges(req.Op) && s.db.ChangesNeedApproval && req.Approval == "" {
		why := "on " + s.g.Host + " this change runs once someone else approves it"
		if err := rec.refused(req.ID, text, why); err != nil {
			return tunnel.StoreAnswer{}, err
		}
		return tunnel.StoreAnswer{Type: "error", Error: why, NeedsApproval: &tunnel.RedisApprovalNeed{Text: text}}, nil
	}
	a, err := readKafkaAdmin(s.file)
	if err != nil {
		return tunnel.StoreAnswer{Type: "error", Error: err.Error()}, nil
	}
	if req.Op == tunnel.BrokerOpUserLogin {
		return s.userLogin(ctx, req, rec, a.User)
	}
	cl, err := s.adminClient(ctx)
	if err != nil {
		return tunnel.StoreAnswer{Type: "error", Error: err.Error()}, nil
	}
	defer cl.Close()
	adm := kadm.NewClient(cl)
	cctx, cancel := context.WithTimeout(ctx, max(s.timeout, 20*time.Second))
	defer cancel()
	if req.Op == tunnel.BrokerOpUsers {
		return s.users(cctx, adm, a.User)
	}
	u := req.BrokerUser
	if u.Name == a.User {
		return tunnel.StoreAnswer{Type: "error", Error: u.Name + " is the user the agent signs in as; it is changed on the server, not from here"}, nil
	}
	st, err := s.kafkaUser(cctx, adm, u.Name)
	if err != nil {
		return kafkaErrAnswer(err), nil
	}
	refuse := func(why string) (tunnel.StoreAnswer, error) {
		if err := rec.refused(req.ID, text, why); err != nil {
			return tunnel.StoreAnswer{}, err
		}
		return tunnel.StoreAnswer{Type: "error", Error: why}, nil
	}
	elsewhere := func(what string) string {
		return fmt.Sprintf("%s has %d ACL%s outside topics %s, so %s only from an entry that covers every topic (*)",
			u.Name, len(st.outside), plural(len(st.outside)), "starting "+s.db.Name, what)
	}
	switch req.Op {
	case tunnel.BrokerOpUserSet:
		if u.Create && st.exists {
			return refuse(u.Name + " is already a user on this cluster; open it and give it access here instead")
		}
		if !u.Create && !st.exists {
			return refuse("there is no user " + u.Name + " on this cluster")
		}
		if !u.Create && len(st.outside) > 0 && (u.Credential != nil || u.Quotas != nil) {
			return refuse(elsewhere("its password and limits change"))
		}
		if u.ACLs != nil {
			have := map[string]bool{}
			for _, x := range st.inView {
				have[x.Key()] = true
			}
			for _, x := range u.ACLs {
				if x.Permission == "DENY" && !have[x.Key()] {
					return refuse("Argus keeps or removes a DENY rule; it does not make one: " + x.String())
				}
			}
		}
	case tunnel.BrokerOpUserDel:
		if !st.exists {
			return refuse("there is no user " + u.Name + " on this cluster")
		}
		if len(st.outside) > 0 {
			return refuse(elsewhere("it is deleted"))
		}
	case tunnel.BrokerOpUserScopeDel:
		if len(st.inView) == 0 {
			return refuse(u.Name + " has no access here to remove")
		}
	}
	if err := rec.stmt(req.ID, text); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	start := time.Now()
	res := KafkaUserResult{User: u.Name}
	switch req.Op {
	case tunnel.BrokerOpUserSet:
		err = s.setKafkaUser(cctx, adm, *u, st, &res)
	case tunnel.BrokerOpUserScopeDel:
		res.Removed, err = deleteKafkaACLs(cctx, adm, u.Name, st.inView)
	case tunnel.BrokerOpUserDel:
		res.Removed, err = deleteKafkaACLs(cctx, adm, u.Name, st.inView)
		if err == nil {
			err = deleteKafkaSCRAMs(cctx, adm, u.Name, st.mechs)
		}
		if err == nil && st.quotas != (tunnel.KafkaQuotas{}) {
			err = setKafkaQuotas(cctx, adm, u.Name, tunnel.KafkaQuotas{})
		}
	}
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		if rerr := rec.fail(req.ID, kafkaErrText(err), elapsed); rerr != nil {
			return tunnel.StoreAnswer{}, rerr
		}
		return tunnel.StoreAnswer{Type: "error", Error: kafkaErrText(err), MS: elapsed}, nil
	}
	tag := map[string]string{tunnel.BrokerOpUserSet: "SET USER", tunnel.BrokerOpUserDel: "DELETE USER",
		tunnel.BrokerOpUserScopeDel: "REMOVE ACCESS"}[req.Op]
	if req.Op == tunnel.BrokerOpUserSet && u.Create {
		tag = "CREATE USER"
	}
	if err := rec.done(req.ID, tag, int64(res.Added+res.Removed), elapsed, res, false); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	return tunnel.StoreAnswer{Type: "result", Data: res, MS: elapsed}, nil
}

// setKafkaUser sets a user's credential, its ACLs in view and its quotas.
// New ACLs are made before old ones go, so a user being narrowed never has
// a moment with none.
func (s *kafkaSession) setKafkaUser(ctx context.Context, adm *kadm.Client, u tunnel.BrokerUser, st kafkaUserState, res *KafkaUserResult) error {
	if c := u.Credential; c != nil {
		salt, _ := base64.StdEncoding.DecodeString(c.Salt)
		salted, _ := base64.StdEncoding.DecodeString(c.SaltedPassword)
		mech := kadm.ScramSha512
		if u.Mechanism == "SCRAM-SHA-256" {
			mech = kadm.ScramSha256
		}
		r, err := adm.AlterUserSCRAMs(ctx, nil, []kadm.UpsertSCRAM{{User: u.Name, Mechanism: mech,
			Iterations: int32(c.Iterations), Salt: salt, SaltedPassword: salted}})
		if err == nil {
			err = r.Error()
		}
		if err != nil {
			return fmt.Errorf("setting the password: %w", err)
		}
		// The old password goes with the new one: a credential left under
		// the other mechanism would still let it in.
		var other []string
		for _, m := range st.mechs {
			if m != u.Mechanism {
				other = append(other, m)
			}
		}
		if err := deleteKafkaSCRAMs(ctx, adm, u.Name, other); err != nil {
			return fmt.Errorf("removing the old password: %w", err)
		}
	}
	if u.ACLs != nil {
		want := map[string]bool{}
		for _, a := range u.ACLs {
			want[a.Key()] = true
		}
		have := map[string]bool{}
		var drop []tunnel.KafkaACL
		for _, a := range st.inView {
			have[a.Key()] = true
			if !want[a.Key()] {
				drop = append(drop, a)
			}
		}
		for _, a := range u.ACLs {
			if have[a.Key()] {
				continue
			}
			b, err := kafkaACLBuilder(u.Name, a)
			if err != nil {
				return err
			}
			created, err := adm.CreateACLs(ctx, b)
			if err == nil {
				for _, c := range created {
					if c.Err != nil {
						err = c.Err
					}
				}
			}
			if err != nil {
				return fmt.Errorf("adding %s: %w", a.String(), err)
			}
			res.Added++
		}
		n, err := deleteKafkaACLs(ctx, adm, u.Name, drop)
		res.Removed += n
		if err != nil {
			return err
		}
	}
	if u.Quotas != nil {
		if err := setKafkaQuotas(ctx, adm, u.Name, *u.Quotas); err != nil {
			return fmt.Errorf("setting the limits: %w", err)
		}
	}
	return nil
}

// deleteKafkaACLs deletes these ACLs of a user, each by an exact filter.
func deleteKafkaACLs(ctx context.Context, adm *kadm.Client, user string, acls []tunnel.KafkaACL) (int, error) {
	n := 0
	for _, a := range acls {
		b, err := kafkaACLBuilder(user, a)
		if err != nil {
			return n, err
		}
		res, err := adm.DeleteACLs(ctx, b)
		if err == nil {
			for _, r := range res {
				if r.Err != nil {
					err = r.Err
				}
				n += len(r.Deleted)
			}
		}
		if err != nil {
			return n, fmt.Errorf("removing %s: %w", a.String(), err)
		}
	}
	return n, nil
}

func deleteKafkaSCRAMs(ctx context.Context, adm *kadm.Client, user string, mechs []string) error {
	var del []kadm.DeleteSCRAM
	for _, m := range mechs {
		mech := kadm.ScramSha512
		if m == "SCRAM-SHA-256" {
			mech = kadm.ScramSha256
		}
		del = append(del, kadm.DeleteSCRAM{User: user, Mechanism: mech})
	}
	if len(del) == 0 {
		return nil
	}
	r, err := adm.AlterUserSCRAMs(ctx, del, nil)
	if err == nil {
		err = r.Error()
	}
	return err
}

// setKafkaQuotas makes a user's quotas these: each set, or removed at 0.
func setKafkaQuotas(ctx context.Context, adm *kadm.Client, user string, q tunnel.KafkaQuotas) error {
	name := user
	op := func(key string, v float64) kadm.AlterClientQuotaOp {
		if v <= 0 {
			return kadm.AlterClientQuotaOp{Key: key, Remove: true}
		}
		return kadm.AlterClientQuotaOp{Key: key, Value: v}
	}
	res, err := adm.AlterClientQuotas(ctx, []kadm.AlterClientQuotaEntry{{
		Entity: kadm.ClientQuotaEntity{{Type: "user", Name: &name}},
		Ops:    []kadm.AlterClientQuotaOp{op(quotaProduce, q.ProduceBytes), op(quotaConsume, q.ConsumeBytes), op(quotaRequest, q.RequestPct)},
	}})
	if err != nil {
		return err
	}
	for _, r := range res {
		if r.Err != nil {
			return fmt.Errorf("%w %s", r.Err, r.ErrMessage)
		}
	}
	return nil
}

// userLogin signs in as a user on the entry's listener with the password
// the browser still holds, and lists what it sees. The password is used for
// this and dropped: the recording names the user only.
func (s *kafkaSession) userLogin(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder, admin string) (tunnel.StoreAnswer, error) {
	u := req.BrokerUser
	if u.Name == admin {
		return tunnel.StoreAnswer{Type: "error", Error: "the agent's own sign-in is not checked from here"}, nil
	}
	if err := rec.stmt(req.ID, tunnel.KafkaUserText(s.db.Name, req)); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	mech := u.Mechanism
	if mech == "" {
		mech = "SCRAM-SHA-512"
	}
	start := time.Now()
	res := KafkaLoginResult{User: u.Name, Address: s.addr}
	// A new credential can take a moment to reach every broker.
	var last error
	for i := 0; i < 8 && !res.OK; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				i = 8
				continue
			case <-time.After(400 * time.Millisecond):
			}
		}
		cl, err := kafkaClient(s.addr, u.Name, req.Password, mech, 10*time.Second, kgo.DisableIdempotentWrite())
		if err != nil {
			last = err
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		td, err := kadm.NewClient(cl).ListTopics(cctx)
		cancel()
		cl.Close()
		if err != nil {
			last = err
			continue
		}
		res.OK = true
		for _, t := range td.Sorted() {
			if t.IsInternal || t.Err != nil {
				continue
			}
			if tunnel.InKafkaScope(s.db.Name, t.Topic) {
				res.Topics = append(res.Topics, t.Topic)
			} else {
				res.Elsewhere++
			}
		}
	}
	if !res.OK && last != nil {
		res.Detail = kafkaErrText(last)
		if strings.Contains(res.Detail, "SASL") || strings.Contains(res.Detail, "uthentication") {
			res.Detail = "Kafka did not accept the user and password: " + res.Detail
		}
	}
	tag := "login refused"
	if res.OK {
		tag = "login ok"
	}
	if err := rec.done(req.ID, tag, int64(len(res.Topics)), time.Since(start).Milliseconds(), res, false); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	return tunnel.StoreAnswer{Type: "result", Data: res}, nil
}
