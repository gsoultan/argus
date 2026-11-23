package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
The app's user made with a new RabbitMQ vhost or Kafka topic, in Add →
Create new (plans/broker-users/design.md, picture 08). A new user gets
everything on the new vhost, or produce and consume on the one topic and
groups starting with its name; an existing one is given the same. The
password comes in its own frame, as a database owner's does, is used once
and checked by signing in, and is never in a statement shown or recorded.
*/

// rabbitUserList is the node's users and their tags.
func rabbitUserList(ctx context.Context, ctl func(context.Context, []string, string) ([]byte, error)) (map[string][]string, error) {
	out, err := ctl(ctx, []string{"list_users", "--silent", "--formatter", "json"}, "")
	if err != nil {
		return nil, err
	}
	var users []struct {
		User string   `json:"user"`
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(out, &users); err != nil {
		return nil, fmt.Errorf("rabbitmqctl list_users answered something unexpected: %w", err)
	}
	m := map[string][]string{}
	for _, u := range users {
		m[u.User] = u.Tags
	}
	return m, nil
}

// rabbitAppUsers are the users a new vhost may be given to: not Argus's,
// and none whose tags reach past one vhost.
func rabbitAppUsers(ctx context.Context, ctl func(context.Context, []string, string) ([]byte, error)) []tunnel.OwnerUser {
	m, err := rabbitUserList(ctx, ctl)
	if err != nil {
		return nil
	}
	var out []tunnel.OwnerUser
	for name, tags := range m {
		if tunnel.IsStoreUser(name) || tunnel.RabbitWideTag(tags) != "" || name == "guest" {
			continue
		}
		out = append(out, tunnel.OwnerUser{Name: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// rabbitOwnerBefore makes a new user, or checks an existing one, before the
// vhost. made says a user was made that must go if the vhost is not.
func rabbitOwnerBefore(ctx context.Context, ctl func(context.Context, []string, string) ([]byte, error), c tunnel.GrantCreate, pw string, res *tunnel.CreateResult) (made, ok bool) {
	o := c.Owner
	if c.OwnerName() == "" {
		return false, true
	}
	users, err := rabbitUserList(ctx, ctl)
	if err != nil {
		res.Error = err.Error()
		return false, false
	}
	tags, exists := users[o.Name]
	if o.Kind == tunnel.OwnerExisting {
		switch {
		case !exists:
			res.Error = "there is no user named " + o.Name + " on this broker"
			return false, false
		case tunnel.RabbitWideTag(tags) != "":
			res.Error = o.Name + " is tagged " + tunnel.RabbitWideTag(tags) + ", which reaches past one vhost; pick a user of the app's own"
			return false, false
		}
		res.Steps = append(res.Steps, tunnel.CreateStep{Text: "The user " + o.Name + " is on this broker", OK: true})
		return false, true
	}
	if exists {
		res.Error = "a user named " + o.Name + " is already on this broker; pick it under an existing user, or another name"
		return false, false
	}
	if _, err := ctl(ctx, []string{"add_user", o.Name}, pw+"\n"); err != nil {
		res.Error = err.Error()
		return false, false
	}
	res.Steps = append(res.Steps, tunnel.CreateStep{Text: "Made the user " + o.Name, OK: true})
	return true, true
}

func rabbitUndoUser(ctx context.Context, ctl func(context.Context, []string, string) ([]byte, error), name string) tunnel.CreateStep {
	_, err := ctl(context.WithoutCancel(ctx), []string{"delete_user", name}, "")
	return step(true, "Removed the user "+name+" again, since the vhost was not made", err, func(e error) string { return e.Error() })
}

// rabbitOwnerAfter gives the user the new vhost and, for a new user, checks
// it signs in. Anything that fails leaves what was made, said as partial.
func rabbitOwnerAfter(ctx context.Context, ctl func(context.Context, []string, string) ([]byte, error), c tunnel.GrantCreate, pw string, res *tunnel.CreateResult) {
	o := c.Owner
	if _, err := ctl(ctx, []string{"set_permissions", "-p", c.Name, o.Name, ".*", ".*", ".*"}, ""); err != nil {
		res.Steps = append(res.Steps, tunnel.CreateStep{Text: "Give " + o.Name + " the vhost", Error: err.Error()})
		res.OK, res.Error = false, "the vhost was made, but "+o.Name+" was not given it: "+err.Error()
		return
	}
	res.Steps = append(res.Steps, tunnel.CreateStep{Text: "Gave " + o.Name + " everything on " + c.Name, OK: true})
	if o.Kind != tunnel.OwnerNew {
		return
	}
	if _, err := ctl(ctx, []string{"authenticate_user", o.Name}, pw+"\n"); err != nil {
		res.Steps = append(res.Steps, tunnel.CreateStep{Text: o.Name + " signs in", Error: err.Error()})
		res.OK, res.Error = false, o.Name+" was made, but signing in as it failed: "+err.Error()
		return
	}
	res.Steps = append(res.Steps, tunnel.CreateStep{Text: o.Name + " signs in with its password", OK: true})
	if port := rabbitAMQPPort(ctx, ctl); port > 0 {
		res.AppPort = port
	}
}

// rabbitAMQPPort is the node's AMQP listener, where apps sign in.
func rabbitAMQPPort(ctx context.Context, ctl func(context.Context, []string, string) ([]byte, error)) int {
	out, err := ctl(ctx, []string{"status", "--formatter", "json"}, "")
	if err != nil {
		return 0
	}
	var st struct {
		Listeners []struct {
			Protocol string `json:"protocol"`
			Port     int    `json:"port"`
		} `json:"listeners"`
	}
	if json.Unmarshal(out, &st) != nil {
		return 0
	}
	for _, l := range st.Listeners {
		if l.Protocol == "amqp" {
			return l.Port
		}
	}
	return 0
}

// kafkaAppUsers are the SCRAM users a new topic may be given to.
func kafkaAppUsers(ctx context.Context, adm *kadm.Client, file string) []tunnel.OwnerUser {
	admin, _ := readKafkaAdmin(file)
	described, err := adm.DescribeUserSCRAMs(ctx)
	if err != nil {
		return nil
	}
	var out []tunnel.OwnerUser
	for name := range described {
		if tunnel.IsStoreUser(name) || name == admin.User {
			continue
		}
		out = append(out, tunnel.OwnerUser{Name: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// kafkaOwnerBefore checks the user before the topic: a new one is not
// there yet, an existing one is, and neither is the agent's own.
func kafkaOwnerBefore(ctx context.Context, adm *kadm.Client, file string, c tunnel.GrantCreate, res *tunnel.CreateResult) bool {
	o := c.Owner
	if c.OwnerName() == "" {
		return true
	}
	if admin, err := readKafkaAdmin(file); err == nil && admin.User == o.Name {
		res.Error = o.Name + " is the user the agent signs in as; pick a user of the app's own"
		return false
	}
	described, err := adm.DescribeUserSCRAMs(ctx, o.Name)
	exists := false
	if err == nil {
		if d, ok := described[o.Name]; ok && d.Err == nil && len(d.CredInfos) > 0 {
			exists = true
		}
	}
	switch {
	case o.Kind == tunnel.OwnerExisting && !exists:
		res.Error = "there is no SCRAM user named " + o.Name + " on this cluster"
		return false
	case o.Kind == tunnel.OwnerNew && exists:
		res.Error = "a user named " + o.Name + " is already on this cluster; pick it under an existing user, or another name"
		return false
	}
	if o.Kind == tunnel.OwnerExisting {
		res.Steps = append(res.Steps, tunnel.CreateStep{Text: "The user " + o.Name + " is on this cluster", OK: true})
	}
	return true
}

// kafkaOwnerAfter makes a new user's SCRAM credential, gives the user the
// topic and groups starting with its name, and checks a new user signs in.
func kafkaOwnerAfter(ctx context.Context, adm *kadm.Client, addr string, c tunnel.GrantCreate, pw string, res *tunnel.CreateResult) {
	o := c.Owner
	fail := func(what string, err error) {
		res.Steps = append(res.Steps, tunnel.CreateStep{Text: what, Error: kafkaErrText(err)})
		res.OK, res.Error = false, "the topic was made, but "+strings.ToLower(what[:1])+what[1:]+" failed: "+kafkaErrText(err)
	}
	pre, post := tunnel.OwnerPlan(c, tunnel.PasswordRedacted)
	if o.Kind == tunnel.OwnerNew {
		altered, err := adm.AlterUserSCRAMs(ctx, nil, []kadm.UpsertSCRAM{{User: o.Name, Mechanism: kadm.ScramSha512,
			Iterations: 8192, Password: pw}})
		if err == nil {
			for _, a := range altered {
				if a.Err != nil {
					err = a.Err
				}
			}
		}
		if err != nil {
			fail("Make the user "+o.Name, err)
			return
		}
		res.Steps = append(res.Steps, tunnel.CreateStep{Text: "Made the user " + o.Name + " (SCRAM-SHA-512)", OK: true})
		res.Statements = append(append([]string{}, pre...), res.Statements...)
	}
	principal := "User:" + o.Name
	b := kadm.NewACLs().Allow(principal).AllowHosts().Topics(c.Name).ResourcePatternType(kadm.ACLPatternLiteral).
		Operations(kadm.OpRead, kadm.OpWrite, kadm.OpDescribe)
	g := kadm.NewACLs().Allow(principal).AllowHosts().Groups(c.Name).ResourcePatternType(kadm.ACLPatternPrefixed).
		Operations(kadm.OpRead)
	for _, acl := range []*kadm.ACLBuilder{b, g} {
		created, err := adm.CreateACLs(ctx, acl)
		if err == nil {
			for _, r := range created {
				if r.Err != nil {
					err = r.Err
				}
			}
		}
		if err != nil {
			fail("Give "+o.Name+" the topic", err)
			return
		}
	}
	res.Statements = append(res.Statements, post...)
	res.Steps = append(res.Steps, tunnel.CreateStep{Text: "Gave " + o.Name + " produce and consume on " + c.Name +
		", and groups starting with it", OK: true})
	if o.Kind != tunnel.OwnerNew {
		return
	}
	// A new credential can take a moment to reach every broker.
	var last error
	for i := 0; i < 8; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(400 * time.Millisecond):
			}
		}
		cl, err := kafkaClient(addr, o.Name, pw, "SCRAM-SHA-512", 10*time.Second, kgo.DisableIdempotentWrite())
		if err != nil {
			last = err
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err = kadm.NewClient(cl).ListTopics(cctx, c.Name)
		cancel()
		cl.Close()
		if err == nil {
			res.Steps = append(res.Steps, tunnel.CreateStep{Text: o.Name + " signs in with its password", OK: true})
			if _, port, err := net.SplitHostPort(addr); err == nil {
				res.AppPort, _ = strconv.Atoi(port)
			}
			return
		}
		last = err
	}
	fail(o.Name+" signs in", last)
}
