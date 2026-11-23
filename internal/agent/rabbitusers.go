package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
A RabbitMQ entry's Users tab (tunnel/rabbitusers.go has the rules): the users
with permissions on the entry's vhost, made, changed and removed through the
management API as the DBA session's user, which is the node's administrator.

A user is the node's and an entry is a vhost, so its password, tags and
limits change, and the user is deleted, only while it has no permissions on
another vhost; otherwise the entry takes away its access here.

The password_hash comes from the browser. The management API's PUT on a user
sets its password and tags together, and one without a password takes the
password away, so tags alone change through rabbitmqctl.
*/

// RabbitUserInfo is one user with permissions on the entry's vhost.
type RabbitUserInfo struct {
	Name       string                   `json:"name"`
	Tags       []string                 `json:"tags"`
	Perms      tunnel.RabbitPerms       `json:"perms"`
	TopicPerms []tunnel.RabbitTopicPerm `json:"topicPerms"`
	Limits     tunnel.RabbitLimits      `json:"limits"`
	// Elsewhere are the other vhosts it has permissions on.
	Elsewhere   []string `json:"elsewhere,omitempty"`
	Connections int      `json:"connections"`
	HasPassword bool     `json:"hasPassword"`
	// Locked is why it is shown but never changed from here.
	Locked string `json:"locked,omitempty"`
	// DefaultPassword: guest still signs in with guest.
	DefaultPassword bool `json:"defaultPassword,omitempty"`
}

// RabbitUsersData is the users answer.
type RabbitUsersData struct {
	Users    []RabbitUserInfo `json:"users"`
	Sessions []string         `json:"sessions"`
	NotShown int              `json:"notShown"`
	Vhost    string           `json:"vhost"`
	AMQPPort int              `json:"amqpPort"`
	TLSPort  int              `json:"tlsPort,omitempty"`
}

// RabbitLoginResult is the sign-in check.
type RabbitLoginResult struct {
	User    string `json:"user"`
	Address string `json:"address"`
	OK      bool   `json:"ok"`
	Detail  string `json:"detail,omitempty"`
}

// rabbitUserRow is /api/users: tags are a list from 3.13 and a
// comma-separated string before.
type rabbitUserRow struct {
	Name        string          `json:"name"`
	Tags        json.RawMessage `json:"tags"`
	PasswordSet *bool           `json:"has_password"`
	Hash        string          `json:"password_hash"`
}

func (r rabbitUserRow) tags() []string {
	var l []string
	if json.Unmarshal(r.Tags, &l) == nil {
		return l
	}
	var s string
	_ = json.Unmarshal(r.Tags, &s)
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			l = append(l, t)
		}
	}
	return l
}

type rabbitPermRow struct {
	User      string `json:"user"`
	Vhost     string `json:"vhost"`
	Configure string `json:"configure"`
	Write     string `json:"write"`
	Read      string `json:"read"`
}

type rabbitTopicRow struct {
	User     string `json:"user"`
	Vhost    string `json:"vhost"`
	Exchange string `json:"exchange"`
	Write    string `json:"write"`
	Read     string `json:"read"`
}

type rabbitLimitRow struct {
	User  string         `json:"user"`
	Value map[string]int `json:"value"`
}

// rabbitUserState is what the node has for one user.
type rabbitUserState struct {
	exists    bool
	tags      []string
	here      *tunnel.RabbitPerms
	topics    []tunnel.RabbitTopicPerm
	limits    tunnel.RabbitLimits
	elsewhere []string
}

func (s *rabbitSession) amqpPorts(ctx context.Context) (plain, tls int) {
	var ov struct {
		Listeners []struct {
			Protocol string `json:"protocol"`
			Port     int    `json:"port"`
		} `json:"listeners"`
	}
	plain = 5672
	if _, err := s.call(ctx, http.MethodGet, "/api/overview", nil, &ov); err != nil {
		return plain, 0
	}
	for _, l := range ov.Listeners {
		switch l.Protocol {
		case "amqp":
			plain = l.Port
		case "amqp/ssl":
			tls = l.Port
		}
	}
	return plain, tls
}

func (s *rabbitSession) users(ctx context.Context) (tunnel.StoreAnswer, error) {
	var (
		users  []rabbitUserRow
		perms  []rabbitPermRow
		topics []rabbitTopicRow
		limits []rabbitLimitRow
		conns  []struct {
			User  string `json:"user"`
			Vhost string `json:"vhost"`
		}
	)
	for _, c := range []struct {
		path string
		out  any
	}{{"/api/users", &users}, {"/api/permissions", &perms}, {"/api/vhosts/" + s.v() + "/topic-permissions", &topics},
		{"/api/user-limits", &limits}, {"/api/connections?columns=user,vhost", &conns}} {
		if _, err := s.call(ctx, http.MethodGet, c.path, nil, c.out); err != nil {
			return s.failed(ctx, err)
		}
	}
	d := RabbitUsersData{Vhost: s.vhost, Users: []RabbitUserInfo{}, Sessions: []string{}}
	d.AMQPPort, d.TLSPort = s.amqpPorts(ctx)
	byName := map[string]*RabbitUserInfo{}
	tagsOf := map[string]rabbitUserRow{}
	for _, u := range users {
		tagsOf[u.Name] = u
	}
	others := map[string][]string{}
	for _, p := range perms {
		if p.Vhost != s.vhost {
			others[p.User] = append(others[p.User], p.Vhost)
			continue
		}
		if tunnel.IsStoreUser(p.User) {
			d.Sessions = append(d.Sessions, p.User)
			continue
		}
		row, ok := tagsOf[p.User]
		if !ok {
			continue
		}
		u := &RabbitUserInfo{Name: p.User, Tags: row.tags(), Perms: tunnel.RabbitPerms{Configure: p.Configure, Write: p.Write, Read: p.Read},
			TopicPerms: []tunnel.RabbitTopicPerm{}, HasPassword: row.PasswordSet == nil || *row.PasswordSet}
		if u.Tags == nil {
			u.Tags = []string{}
		}
		if t := tunnel.RabbitWideTag(u.Tags); t != "" {
			u.Locked = "tagged " + t + ", which reaches past one vhost; changed on the server, not from an entry"
		}
		byName[p.User] = u
	}
	for name, u := range byName {
		u.Elsewhere = others[name]
		sort.Strings(u.Elsewhere)
	}
	for _, t := range topics {
		if u := byName[t.User]; u != nil {
			u.TopicPerms = append(u.TopicPerms, tunnel.RabbitTopicPerm{Exchange: t.Exchange, Write: t.Write, Read: t.Read})
		}
	}
	for _, l := range limits {
		if u := byName[l.User]; u != nil {
			u.Limits = tunnel.RabbitLimits{MaxConnections: l.Value["max-connections"], MaxChannels: l.Value["max-channels"]}
		}
	}
	for _, c := range conns {
		if u := byName[c.User]; u != nil && c.Vhost == s.vhost {
			u.Connections++
		}
	}
	if u := byName["guest"]; u != nil {
		u.DefaultPassword = s.guestDefault(ctx)
	}
	for _, u := range byName {
		d.Users = append(d.Users, *u)
	}
	for _, u := range users {
		if byName[u.Name] == nil && !tunnel.IsStoreUser(u.Name) {
			d.NotShown++
		}
	}
	sort.Slice(d.Users, func(i, j int) bool { return d.Users[i].Name < d.Users[j].Name })
	sort.Strings(d.Sessions)
	return tunnel.StoreAnswer{Type: "result", Data: d}, nil
}

// guestDefault reports whether guest still signs in with guest.
func (s *rabbitSession) guestDefault(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/api/whoami", nil)
	if err != nil {
		return false
	}
	req.SetBasicAuth("guest", "guest")
	resp, err := s.http.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (s *rabbitSession) rabbitUser(ctx context.Context, name string) (rabbitUserState, error) {
	var st rabbitUserState
	var row rabbitUserRow
	if code, err := s.call(ctx, http.MethodGet, "/api/users/"+esc(name), nil, &row); err != nil {
		if code == http.StatusNotFound {
			return st, nil
		}
		return st, err
	}
	st.exists, st.tags = true, row.tags()
	var perms []rabbitPermRow
	if _, err := s.call(ctx, http.MethodGet, "/api/users/"+esc(name)+"/permissions", nil, &perms); err != nil {
		return st, err
	}
	for _, p := range perms {
		if p.Vhost == s.vhost {
			st.here = &tunnel.RabbitPerms{Configure: p.Configure, Write: p.Write, Read: p.Read}
		} else {
			st.elsewhere = append(st.elsewhere, p.Vhost)
		}
	}
	var topics []rabbitTopicRow
	if _, err := s.call(ctx, http.MethodGet, "/api/users/"+esc(name)+"/topic-permissions", nil, &topics); err != nil {
		return st, err
	}
	for _, t := range topics {
		if t.Vhost == s.vhost {
			st.topics = append(st.topics, tunnel.RabbitTopicPerm{Exchange: t.Exchange, Write: t.Write, Read: t.Read})
		} else if !slices.Contains(st.elsewhere, t.Vhost) {
			st.elsewhere = append(st.elsewhere, t.Vhost)
		}
	}
	var limits []rabbitLimitRow
	if code, err := s.call(ctx, http.MethodGet, "/api/user-limits/"+esc(name), nil, &limits); err != nil && code != http.StatusNotFound {
		return st, err
	}
	for _, l := range limits {
		st.limits = tunnel.RabbitLimits{MaxConnections: l.Value["max-connections"], MaxChannels: l.Value["max-channels"]}
	}
	sort.Strings(st.elsewhere)
	return st, nil
}

// RabbitUserResult is a change's answer.
type RabbitUserResult struct {
	User string `json:"user"`
}

func (s *rabbitSession) userOp(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	need := tunnel.BrokerUserOpLevelFor(tunnel.EngineRabbitMQ, req.Op)
	if tunnel.LevelRank(s.db.Level) < tunnel.LevelRank(need) {
		return tunnel.StoreAnswer{Type: "error", Error: "this needs " + need + " or above; this session is at " + s.db.Level}, nil
	}
	if why := tunnel.ValidRabbitUserChange(s.vhost, req); why != "" {
		return tunnel.StoreAnswer{Type: "error", Error: why}, nil
	}
	if req.Op == tunnel.BrokerOpUsers {
		return s.users(ctx)
	}
	text := tunnel.RabbitUserText(s.vhost, req)
	if tunnel.BrokerUserChanges(req.Op) && s.db.ChangesNeedApproval && req.Approval == "" {
		why := "on " + s.g.Host + " this change runs once someone else approves it"
		if err := rec.refused(req.ID, text, why); err != nil {
			return tunnel.StoreAnswer{}, err
		}
		return tunnel.StoreAnswer{Type: "error", Error: why, NeedsApproval: &tunnel.RedisApprovalNeed{Text: text}}, nil
	}
	if req.Op == tunnel.BrokerOpUserLogin {
		return s.userLogin(ctx, req, rec)
	}
	u := req.BrokerUser
	st, err := s.rabbitUser(ctx, u.Name)
	if err != nil {
		return s.failed(ctx, err)
	}
	refuse := func(why string) (tunnel.StoreAnswer, error) {
		if err := rec.refused(req.ID, text, why); err != nil {
			return tunnel.StoreAnswer{}, err
		}
		return tunnel.StoreAnswer{Type: "error", Error: why}, nil
	}
	if t := tunnel.RabbitWideTag(st.tags); t != "" {
		return refuse(u.Name + " is tagged " + t + ", which reaches past one vhost; it is changed on the server, not from an entry")
	}
	elsewhere := func(what string) string {
		return u.Name + " also has permissions on " + strings.Join(st.elsewhere, ", ") + ", so " + what +
			" only once it has none there; here you can remove its access to " + s.vhost
	}
	switch req.Op {
	case tunnel.BrokerOpUserSet:
		if u.Create && st.exists {
			return refuse(u.Name + " is already a user on this server; open it and give it access here instead")
		}
		if !u.Create && !st.exists {
			return refuse("there is no user " + u.Name + " on this server")
		}
		if !u.Create && len(st.elsewhere) > 0 && (u.PasswordHash != "" || u.Tags != nil || u.Limits != nil) {
			return refuse(elsewhere("its password, tags and limits change"))
		}
		if !u.Create && st.here == nil && u.Perms == nil {
			return refuse(u.Name + " has no permissions on " + s.vhost + "; give it some first")
		}
	case tunnel.BrokerOpUserDel:
		if !st.exists {
			return refuse("there is no user " + u.Name + " on this server")
		}
		if len(st.elsewhere) > 0 {
			return refuse(elsewhere("it is deleted"))
		}
	case tunnel.BrokerOpUserScopeDel:
		if st.here == nil && len(st.topics) == 0 {
			return refuse(u.Name + " has no access to " + s.vhost + " to remove")
		}
	}
	if err := rec.stmt(req.ID, text); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	start := time.Now()
	switch req.Op {
	case tunnel.BrokerOpUserSet:
		err = s.setRabbitUser(ctx, *u, st)
	case tunnel.BrokerOpUserScopeDel:
		err = s.removeRabbitAccess(ctx, u.Name, st)
	case tunnel.BrokerOpUserDel:
		_, err = s.call(ctx, http.MethodDelete, "/api/users/"+esc(u.Name), nil, nil)
	}
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		if rerr := rec.fail(req.ID, err.Error(), elapsed); rerr != nil {
			return tunnel.StoreAnswer{}, rerr
		}
		return tunnel.StoreAnswer{Type: "error", Error: err.Error(), MS: elapsed}, nil
	}
	tag := map[string]string{tunnel.BrokerOpUserSet: "SET USER", tunnel.BrokerOpUserDel: "DELETE USER",
		tunnel.BrokerOpUserScopeDel: "REMOVE ACCESS"}[req.Op]
	if req.Op == tunnel.BrokerOpUserSet && u.Create {
		tag = "CREATE USER"
	}
	res := RabbitUserResult{User: u.Name}
	if err := rec.done(req.ID, tag, 1, elapsed, res, false); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	return tunnel.StoreAnswer{Type: "result", Data: res, MS: elapsed}, nil
}

// setRabbitUser makes or changes a user. A new user that cannot be given
// its permissions is deleted again, so a failed create leaves nothing.
func (s *rabbitSession) setRabbitUser(ctx context.Context, u tunnel.BrokerUser, st rabbitUserState) (err error) {
	tags := st.tags
	if u.Tags != nil {
		tags = *u.Tags
	}
	if u.Create {
		defer func() {
			if err != nil {
				_, _ = s.call(context.WithoutCancel(ctx), http.MethodDelete, "/api/users/"+esc(u.Name), nil, nil)
			}
		}()
	}
	switch {
	case u.PasswordHash != "":
		if _, err := s.call(ctx, http.MethodPut, "/api/users/"+esc(u.Name), map[string]any{
			"password_hash": u.PasswordHash, "hashing_algorithm": "rabbit_password_hashing_sha256",
			"tags": strings.Join(tags, ","),
		}, nil); err != nil {
			return err
		}
	case u.Tags != nil:
		// A PUT without the password would take it away.
		if _, err := s.f.cfg.Ctl(ctx, append([]string{"set_user_tags", u.Name}, tags...), ""); err != nil {
			return err
		}
	}
	if p := u.Perms; p != nil {
		if _, err := s.call(ctx, http.MethodPut, "/api/permissions/"+s.v()+"/"+esc(u.Name), p, nil); err != nil {
			return err
		}
	}
	if u.TopicPerms != nil {
		want := map[string]tunnel.RabbitTopicPerm{}
		for _, t := range u.TopicPerms {
			want[t.Exchange] = t
		}
		for _, t := range u.TopicPerms {
			if _, err := s.call(ctx, http.MethodPut, "/api/topic-permissions/"+s.v()+"/"+esc(u.Name), t, nil); err != nil {
				return err
			}
		}
		for _, t := range st.topics {
			if _, keep := want[t.Exchange]; keep {
				continue
			}
			if err := s.deleteTopicPerm(ctx, u.Name, t.Exchange); err != nil {
				return err
			}
		}
	}
	if l := u.Limits; l != nil {
		for name, v := range map[string]int{"max-connections": l.MaxConnections, "max-channels": l.MaxChannels} {
			path := "/api/user-limits/" + esc(u.Name) + "/" + name
			if v > 0 {
				_, err = s.call(ctx, http.MethodPut, path, map[string]int{"value": v}, nil)
			} else {
				var code int
				code, err = s.call(ctx, http.MethodDelete, path, nil, nil)
				if code == http.StatusNotFound {
					err = nil
				}
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// deleteTopicPerm removes one exchange's topic permission. The management
// API deletes all of a user's on a vhost, so the others are put back.
func (s *rabbitSession) deleteTopicPerm(ctx context.Context, user, exchange string) error {
	st, err := s.rabbitUser(ctx, user)
	if err != nil {
		return err
	}
	if code, err := s.call(ctx, http.MethodDelete, "/api/topic-permissions/"+s.v()+"/"+esc(user), nil, nil); err != nil && code != http.StatusNotFound {
		return err
	}
	for _, t := range st.topics {
		if t.Exchange == exchange {
			continue
		}
		if _, err := s.call(ctx, http.MethodPut, "/api/topic-permissions/"+s.v()+"/"+esc(user), t, nil); err != nil {
			return err
		}
	}
	return nil
}

func (s *rabbitSession) removeRabbitAccess(ctx context.Context, user string, st rabbitUserState) error {
	if st.here != nil {
		if code, err := s.call(ctx, http.MethodDelete, "/api/permissions/"+s.v()+"/"+esc(user), nil, nil); err != nil && code != http.StatusNotFound {
			return err
		}
	}
	if len(st.topics) > 0 {
		if code, err := s.call(ctx, http.MethodDelete, "/api/topic-permissions/"+s.v()+"/"+esc(user), nil, nil); err != nil && code != http.StatusNotFound {
			return err
		}
	}
	return nil
}

// userLogin opens one AMQP connection to the vhost as the user, on the
// loopback, and closes it. The password is used for that and dropped.
func (s *rabbitSession) userLogin(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	u := req.BrokerUser
	if err := rec.stmt(req.ID, tunnel.RabbitUserText(s.vhost, req)); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	start := time.Now()
	port, _ := s.amqpPorts(ctx)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	res := RabbitLoginResult{User: u.Name, Address: addr}
	uri := (&url.URL{Scheme: "amqp", User: url.UserPassword(u.Name, req.Password), Host: addr, Path: "/" + s.vhost}).String()
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := amqp.DialConfig(uri, amqp.Config{Vhost: s.vhost, Heartbeat: 10 * time.Second,
		Dial: func(network, a string) (net.Conn, error) { return (&net.Dialer{}).DialContext(cctx, network, a) }})
	if err == nil {
		res.OK = true
		_ = conn.Close()
	} else {
		var ae *amqp.Error
		switch {
		case errors.As(err, &ae) && ae.Code == amqp.AccessRefused:
			res.Detail = "RabbitMQ did not accept the user and password"
		case errors.Is(err, amqp.ErrCredentials):
			res.Detail = "RabbitMQ did not accept the user and password"
		case errors.Is(err, amqp.ErrVhost):
			res.Detail = "the user may not open " + s.vhost
		default:
			res.Detail = err.Error()
		}
	}
	tag := "login refused"
	if res.OK {
		tag = "login ok"
	}
	if err := rec.done(req.ID, tag, 0, time.Since(start).Milliseconds(), res, false); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	return tunnel.StoreAnswer{Type: "result", Data: res}, nil
}
