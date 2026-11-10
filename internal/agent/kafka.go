package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
	"github.com/twmb/franz-go/pkg/sasl/scram"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Kafka in the browser, through the tunnel, as a broker.

	browser ──wss──▶ argus-control ──"kafka" stream──▶ agent ──▶ Kafka on this server

The agent signs in as Kafka's administrator with SCRAM credentials from a file
on this server (tunnel.KafkaAdminFile, or the entry's), and uses that client for
one thing: giving the person a SCRAM user with ACLs on the entry's topics for
their level (tunnel.KafkaTopicOps) and a one-time password, and deleting the
user and its ACLs when their last session closes. Everything the person does
runs as that user, so what they may do is what Kafka's authorizer says.

A cluster that answers without an authorizer has nothing to hold a level, so
the agent refuses it. The admin and the person reach the broker over SASL on
the listener the entry names, which must be on this server; the cluster's own
metadata then says where its other brokers are, as for any Kafka client.
*/

// KafkaService is the tunnel service name.
const KafkaService = tunnel.KafkaService

// Kafka is the kafka service.
func Kafka(cfg StoreConfig) TunnelService {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	f := &kafkaFactory{cfg: cfg, uses: &userUse{}}
	return storeService(cfg, tunnel.KafkaService, tunnel.EngineKafka, f.open)
}

type kafkaFactory struct {
	cfg  StoreConfig
	uses *userUse
	mu   sync.Mutex
}

type kafkaSession struct {
	f    *kafkaFactory
	g    tunnel.Grant
	db   tunnel.GrantDB
	addr string
	file string

	user, pw string
	key      string
	cl       *kgo.Client
	adm      *kadm.Client
	timeout  time.Duration
	maxItems int
	ops      map[string]bool
}

// kafkaAdmin is what the admin file holds.
type kafkaAdmin struct {
	User, Password, Mechanism string
}

// readKafkaAdmin reads the admin file: key = value lines (user, password,
// mechanism: SCRAM-SHA-512 or SCRAM-SHA-256), # comments. The file must be
// readable by its owner only, as a password file is.
func readKafkaAdmin(path string) (kafkaAdmin, error) {
	if !tunnel.ValidKafkaAdminFile(path) {
		return kafkaAdmin{}, fmt.Errorf("%s is not an admin file the agent will read", path)
	}
	st, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return kafkaAdmin{}, fmt.Errorf("Kafka needs its administrator to give people users, and %s does not exist: "+
				"put user = and password = for a SCRAM account that may alter users and ACLs in it, readable by root only", path)
		}
		return kafkaAdmin{}, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return kafkaAdmin{}, fmt.Errorf("%s can be read by others; chmod 600 it, since it holds Kafka's admin password", path)
	}
	b, err := readFileLimited(path, 64<<10)
	if err != nil {
		return kafkaAdmin{}, err
	}
	a := kafkaAdmin{Mechanism: "SCRAM-SHA-512"}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		at := strings.IndexAny(line, "=:")
		if at < 0 {
			continue
		}
		k, v := strings.ToLower(strings.TrimSpace(line[:at])), strings.TrimSpace(line[at+1:])
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		switch k {
		case "user", "username":
			a.User = v
		case "password", "pass":
			a.Password = v
		case "mechanism", "sasl.mechanism":
			a.Mechanism = strings.ToUpper(v)
		}
	}
	if a.User == "" || a.Password == "" {
		return kafkaAdmin{}, fmt.Errorf("%s has no user and password", path)
	}
	if a.Mechanism != "SCRAM-SHA-512" && a.Mechanism != "SCRAM-SHA-256" {
		return kafkaAdmin{}, fmt.Errorf("%s names %s; the agent signs in with SCRAM-SHA-512 or SCRAM-SHA-256", path, a.Mechanism)
	}
	return a, nil
}

func kafkaClient(addr, user, pw, mechanism string, timeout time.Duration, extra ...kgo.Opt) (*kgo.Client, error) {
	auth := scram.Auth{User: user, Pass: pw}
	mech := auth.AsSha512Mechanism()
	if mechanism == "SCRAM-SHA-256" {
		mech = auth.AsSha256Mechanism()
	}
	opts := append([]kgo.Opt{
		kgo.SeedBrokers(addr),
		kgo.SASL(mech),
		kgo.DialTimeout(5 * time.Second),
		kgo.RequestTimeoutOverhead(timeout),
		kgo.RetryTimeout(timeout),
		kgo.ClientID("argus-agent"),
	}, extra...)
	return kgo.NewClient(opts...)
}

func (f *kafkaFactory) open(ctx context.Context, g tunnel.Grant, _ string) (storeEngine, error) {
	db := *g.DB
	if !tunnel.LocalLoopback(db.Host) {
		return nil, errors.New("Kafka must be on this server; " + db.Host + " is not")
	}
	if !tunnel.ValidKafkaScope(db.Name) {
		return nil, errors.New("the grant names no Kafka topics")
	}
	ops := tunnel.KafkaStoreOps(db.Level)
	if ops == nil {
		return nil, errors.New("unknown level " + db.Level)
	}
	s := &kafkaSession{f: f, g: g, db: db, addr: net.JoinHostPort(db.Host, strconv.Itoa(db.Port)), file: db.ConfigPath,
		timeout: ms(db.StatementTimeoutMS, defaultStmtTimeout), maxItems: db.MaxRows, ops: map[string]bool{}}
	for _, o := range ops {
		s.ops[o] = true
	}
	if db.Port == 0 {
		s.addr = net.JoinHostPort(db.Host, "9092")
	}
	if s.file == "" {
		s.file = tunnel.KafkaAdminFile
	}
	if s.maxItems <= 0 || s.maxItems > 1000 {
		s.maxItems = 200
	}
	s.user = tunnel.StoreUser(g.Principal, db.Level)
	s.key = s.addr + "/" + s.user

	admin, err := s.adminClient(ctx)
	if err != nil {
		return nil, err
	}
	defer admin.Close()

	f.mu.Lock()
	err = s.giveUser(ctx, kadm.NewClient(admin))
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := s.connect(ctx); err != nil {
		s.takeUser(context.WithoutCancel(ctx))
		return nil, err
	}
	return s, nil
}

// adminClient signs in as Kafka's administrator and makes sure the cluster
// has an authorizer.
func (s *kafkaSession) adminClient(ctx context.Context) (*kgo.Client, error) {
	a, err := readKafkaAdmin(s.file)
	if err != nil {
		return nil, err
	}
	cl, err := kafkaClient(s.addr, a.User, a.Password, a.Mechanism, 15*time.Second)
	if err != nil {
		return nil, err
	}
	adm := kadm.NewClient(cl)
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := adm.BrokerMetadata(cctx); err != nil {
		cl.Close()
		return nil, fmt.Errorf("could not reach Kafka at %s as the administrator in %s: %w", s.addr, s.file, err)
	}
	res, err := adm.DescribeACLs(cctx, kadm.NewACLs().AnyResource().Allow().AllowHosts().ResourcePatternType(kadm.ACLPatternAny).Operations())
	if err == nil {
		for _, r := range res {
			if r.Err != nil {
				err = r.Err
				break
			}
		}
	}
	if errors.Is(err, kerr.SecurityDisabled) {
		cl.Close()
		return nil, errors.New("Kafka on this server has no authorizer, so anyone who can reach it can do anything " +
			"and a person's level would mean nothing; set authorizer.class.name before adding it to Argus")
	}
	if err != nil {
		cl.Close()
		return nil, fmt.Errorf("the administrator in %s cannot read Kafka's ACLs: %w", s.file, err)
	}
	return cl, nil
}

func kafkaPrincipal(user string) string { return "User:" + user }

// grants are the ACLs a level holds: its operations on the scope's topics,
// and describing consumer groups.
func (s *kafkaSession) grants() *kadm.ACLBuilder {
	var ops []kadm.ACLOperation
	for _, o := range tunnel.KafkaTopicOps(s.db.Level) {
		op, err := kmsg.ParseACLOperation(o)
		if err == nil {
			ops = append(ops, op)
		}
	}
	b := kadm.NewACLs().Allow(kafkaPrincipal(s.user)).AllowHosts().Operations(ops...)
	if s.db.Name == tunnel.KafkaAllTopics {
		return b.Topics("*").ResourcePatternType(kadm.ACLPatternLiteral)
	}
	return b.Topics(s.db.Name).ResourcePatternType(kadm.ACLPatternPrefixed)
}

func (s *kafkaSession) groupGrants() *kadm.ACLBuilder {
	return kadm.NewACLs().Allow(kafkaPrincipal(s.user)).AllowHosts().Groups("*").
		ResourcePatternType(kadm.ACLPatternLiteral).Operations(kadm.OpDescribe)
}

// dropUser deletes a user's SCRAM credential and every ACL that names it.
func dropKafkaUser(ctx context.Context, adm *kadm.Client, user string) error {
	var errs []error
	if _, err := adm.DeleteACLs(ctx, kadm.NewACLs().Allow(kafkaPrincipal(user)).AllowHosts().AnyResource().
		ResourcePatternType(kadm.ACLPatternAny).Operations()); err != nil {
		errs = append(errs, err)
	}
	for _, m := range []kadm.ScramMechanism{kadm.ScramSha512, kadm.ScramSha256} {
		res, err := adm.AlterUserSCRAMs(ctx, []kadm.DeleteSCRAM{{User: user, Mechanism: m}}, nil)
		if err == nil {
			err = res.Error()
		}
		if err != nil && !errors.Is(err, kerr.ResourceNotFound) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// giveUser makes the person's user and its ACLs, or reuses it while another
// session holds it. Users Argus left behind -- an agent that stopped
// mid-session -- are deleted first.
func (s *kafkaSession) giveUser(ctx context.Context, adm *kadm.Client) error {
	if users, err := adm.DescribeUserSCRAMs(ctx); err == nil {
		for u := range users {
			if tunnel.IsStoreUser(u) && !s.f.uses.live(s.addr+"/"+u) {
				_ = dropKafkaUser(ctx, adm, u)
			}
		}
	}
	pw, fresh, err := s.f.uses.password(s.key, sessionPassword)
	if err != nil {
		return err
	}
	s.pw = pw
	s.f.uses.add(s.key, 1)
	if !fresh {
		return nil
	}
	fail := func(err error) error {
		s.f.uses.add(s.key, -1)
		_ = dropKafkaUser(context.WithoutCancel(ctx), adm, s.user)
		return fmt.Errorf("could not prepare your Kafka user: %w", err)
	}
	res, err := adm.AlterUserSCRAMs(ctx, nil, []kadm.UpsertSCRAM{{User: s.user, Mechanism: kadm.ScramSha512, Iterations: 8192, Password: pw}})
	if err == nil {
		err = res.Error()
	}
	if err != nil {
		return fail(err)
	}
	for _, b := range []*kadm.ACLBuilder{s.grants(), s.groupGrants()} {
		created, err := adm.CreateACLs(ctx, b)
		if err == nil {
			for _, c := range created {
				if c.Err != nil {
					err = c.Err
					break
				}
			}
		}
		if err != nil {
			return fail(err)
		}
	}
	return nil
}

// takeUser deletes the person's user and its ACLs when this was its last
// session.
func (s *kafkaSession) takeUser(ctx context.Context) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if s.f.uses.add(s.key, -1) > 0 {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	admin, err := s.adminClient(cctx)
	if err == nil {
		defer admin.Close()
		err = dropKafkaUser(cctx, kadm.NewClient(admin), s.user)
	}
	if err != nil {
		s.f.cfg.Log.Error("kafka: could not delete a user after its session", "user", s.user, "error", err,
			"detail", "it is deleted at the next session on this cluster")
	}
}

// connect signs in as the person's user. A new SCRAM credential can take a
// moment to reach every broker, so the first sign-in is retried briefly.
func (s *kafkaSession) connect(ctx context.Context) error {
	var last error
	for i := 0; i < 10; i++ {
		cl, err := kafkaClient(s.addr, s.user, s.pw, "SCRAM-SHA-512", s.timeout, kgo.DisableIdempotentWrite())
		if err == nil {
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_, err = kadm.NewClient(cl).BrokerMetadata(cctx)
			cancel()
			if err == nil {
				s.cl, s.adm = cl, kadm.NewClient(cl)
				return nil
			}
			cl.Close()
		}
		last = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	return fmt.Errorf("could not sign in to Kafka as %s: %w", s.user, last)
}

func (s *kafkaSession) ready() tunnel.StoreAnswer {
	ops := tunnel.KafkaStoreOps(s.db.Level)
	return tunnel.StoreAnswer{Engine: tunnel.EngineKafka, User: s.user, Ops: ops}
}

func (s *kafkaSession) close(ctx context.Context) {
	if s.cl != nil {
		s.cl.Close()
	}
	s.takeUser(ctx)
}

func kafkaErrAnswer(err error) tunnel.StoreAnswer {
	return tunnel.StoreAnswer{Type: "error", Error: kafkaErrText(err)}
}

func kafkaErrText(err error) string {
	var ke *kerr.Error
	if errors.As(err, &ke) {
		return ke.Message + ": " + ke.Description
	}
	return err.Error()
}

func (s *kafkaSession) do(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if !s.ops[req.Op] {
		return tunnel.StoreAnswer{Type: "error", Error: "unknown operation " + req.Op + " for Kafka at " + s.db.Level}, nil
	}
	if req.Name != "" && !tunnel.InKafkaScope(s.db.Name, req.Name) {
		return tunnel.StoreAnswer{Type: "error", Error: req.Name + " is not one of this entry's topics"}, nil
	}
	cctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	switch req.Op {
	case "overview":
		return s.overview(cctx)
	case "topics":
		return s.topics(cctx)
	case "topic":
		return s.topic(cctx, req)
	case "groups":
		return s.groups(cctx, req.Name)
	case "peek":
		return s.peek(cctx, req, rec)
	case "publish":
		return s.publish(cctx, req, rec)
	case "create_topic":
		return s.createTopic(cctx, req, rec)
	case "delete_topic":
		return s.deleteTopic(cctx, req, rec)
	}
	return tunnel.StoreAnswer{Type: "error", Error: "unknown operation " + req.Op}, nil
}

// KafkaBroker, KafkaTopic and the rest are the shapes answers carry in Data.
type KafkaBroker struct {
	ID         int32  `json:"id"`
	Host       string `json:"host"`
	Port       int32  `json:"port"`
	Rack       string `json:"rack,omitempty"`
	Controller bool   `json:"controller,omitempty"`
}

type KafkaTopic struct {
	Name        string `json:"name"`
	Partitions  int    `json:"partitions"`
	Replication int    `json:"replication"`
	// Messages is the end offsets less the start offsets: what the log holds.
	Messages int64 `json:"messages"`
	// UnderReplicated counts partitions with fewer in-sync replicas than
	// replicas; Offline those with no leader.
	UnderReplicated int    `json:"underReplicated,omitempty"`
	Offline         int    `json:"offline,omitempty"`
	Error           string `json:"error,omitempty"`
}

type KafkaPartition struct {
	Partition int32   `json:"partition"`
	Leader    int32   `json:"leader"`
	Replicas  []int32 `json:"replicas"`
	ISR       []int32 `json:"isr"`
	Start     int64   `json:"start"`
	End       int64   `json:"end"`
}

type KafkaGroup struct {
	Group   string `json:"group"`
	State   string `json:"state"`
	Members int    `json:"members"`
	// Lag is how many messages the group has yet to read on the scope's
	// topics, per topic.
	Lag   map[string]int64 `json:"lag,omitempty"`
	Error string           `json:"error,omitempty"`
}

type KafkaMessage struct {
	Partition int32             `json:"partition"`
	Offset    int64             `json:"offset"`
	Time      string            `json:"time"`
	Key       string            `json:"key,omitempty"`
	KeyB64    bool              `json:"keyB64,omitempty"`
	Value     string            `json:"value"`
	ValueB64  bool              `json:"valueB64,omitempty"`
	Cut       bool              `json:"cut,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
}

// inScope keeps the topics the entry covers, sorted, internal ones left out.
func (s *kafkaSession) inScope(td kadm.TopicDetails) []kadm.TopicDetail {
	var out []kadm.TopicDetail
	for _, t := range td {
		if !t.IsInternal && tunnel.InKafkaScope(s.db.Name, t.Topic) {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Topic < out[j].Topic })
	return out
}

func (s *kafkaSession) overview(ctx context.Context) (tunnel.StoreAnswer, error) {
	m, err := s.adm.Metadata(ctx)
	if err != nil {
		return kafkaErrAnswer(err), nil
	}
	var brokers []KafkaBroker
	for _, b := range m.Brokers {
		kb := KafkaBroker{ID: b.NodeID, Host: b.Host, Port: b.Port, Controller: b.NodeID == m.Controller}
		if b.Rack != nil {
			kb.Rack = *b.Rack
		}
		brokers = append(brokers, kb)
	}
	sort.Slice(brokers, func(i, j int) bool { return brokers[i].ID < brokers[j].ID })
	topics := s.inScope(m.Topics)
	parts := 0
	for _, t := range topics {
		parts += len(t.Partitions)
	}
	return tunnel.StoreAnswer{Type: "result", Total: int64(len(topics)), Data: map[string]any{
		"cluster": m.Cluster, "brokers": brokers, "topics": len(topics), "partitions": parts, "scope": s.db.Name,
	}}, nil
}

func (s *kafkaSession) topics(ctx context.Context) (tunnel.StoreAnswer, error) {
	td, err := s.adm.ListTopics(ctx)
	if err != nil {
		return kafkaErrAnswer(err), nil
	}
	topics := s.inScope(td)
	names := make([]string, 0, len(topics))
	for _, t := range topics {
		names = append(names, t.Topic)
	}
	var starts, ends kadm.ListedOffsets
	if len(names) > 0 {
		starts, _ = s.adm.ListStartOffsets(ctx, names...)
		ends, _ = s.adm.ListEndOffsets(ctx, names...)
	}
	out := make([]KafkaTopic, 0, len(topics))
	for _, t := range topics {
		kt := KafkaTopic{Name: t.Topic, Partitions: len(t.Partitions)}
		if t.Err != nil {
			kt.Error = kafkaErrText(t.Err)
		}
		for _, p := range t.Partitions {
			kt.Replication = max(kt.Replication, len(p.Replicas))
			if len(p.ISR) < len(p.Replicas) {
				kt.UnderReplicated++
			}
			if p.Leader < 0 {
				kt.Offline++
			}
			if e, ok := ends.Lookup(t.Topic, p.Partition); ok && e.Err == nil {
				st, _ := starts.Lookup(t.Topic, p.Partition)
				kt.Messages += e.Offset - max(st.Offset, 0)
			}
		}
		out = append(out, kt)
	}
	return tunnel.StoreAnswer{Type: "result", Total: int64(len(out)), Data: out}, nil
}

func (s *kafkaSession) topic(ctx context.Context, req tunnel.StoreRequest) (tunnel.StoreAnswer, error) {
	if !tunnel.ValidKafkaTopic(req.Name) {
		return tunnel.StoreAnswer{Type: "error", Error: "no topic named"}, nil
	}
	td, err := s.adm.ListTopics(ctx, req.Name)
	if err != nil {
		return kafkaErrAnswer(err), nil
	}
	t, ok := td[req.Name]
	if !ok || t.Err != nil {
		if ok {
			return kafkaErrAnswer(t.Err), nil
		}
		return tunnel.StoreAnswer{Type: "error", Error: "there is no topic " + req.Name}, nil
	}
	starts, _ := s.adm.ListStartOffsets(ctx, req.Name)
	ends, _ := s.adm.ListEndOffsets(ctx, req.Name)
	var parts []KafkaPartition
	for _, p := range t.Partitions.Sorted() {
		kp := KafkaPartition{Partition: p.Partition, Leader: p.Leader, Replicas: p.Replicas, ISR: p.ISR, Start: -1, End: -1}
		if o, ok := starts.Lookup(req.Name, p.Partition); ok && o.Err == nil {
			kp.Start = o.Offset
		}
		if o, ok := ends.Lookup(req.Name, p.Partition); ok && o.Err == nil {
			kp.End = o.Offset
		}
		parts = append(parts, kp)
	}
	configs := map[string]string{}
	if rc, err := s.adm.DescribeTopicConfigs(ctx, req.Name); err == nil {
		for _, r := range rc {
			for _, c := range r.Configs {
				if c.Source.String() == "DYNAMIC_TOPIC_CONFIG" && c.Value != nil && !c.Sensitive {
					configs[c.Key] = *c.Value
				}
			}
		}
	}
	groups, _ := s.groups(ctx, req.Name)
	return tunnel.StoreAnswer{Type: "result", Data: map[string]any{
		"name": req.Name, "partitions": parts, "configs": configs, "groups": groups.Data,
	}}, nil
}

// groups lists the consumer groups, with their lag on the scope's topics, or
// on one topic. Groups that read none of them are left out.
func (s *kafkaSession) groups(ctx context.Context, topic string) (tunnel.StoreAnswer, error) {
	listed, err := s.adm.ListGroups(ctx)
	if err != nil {
		return kafkaErrAnswer(err), nil
	}
	names := listed.Groups()
	if len(names) == 0 {
		return tunnel.StoreAnswer{Type: "result", Data: []KafkaGroup{}}, nil
	}
	lags, err := s.adm.Lag(ctx, names...)
	if err != nil {
		return kafkaErrAnswer(err), nil
	}
	out := []KafkaGroup{}
	for _, l := range lags.Sorted() {
		g := KafkaGroup{Group: l.Group, State: l.State, Members: len(l.Members), Lag: map[string]int64{}}
		if e := l.Error(); e != nil && !errors.Is(e, kerr.GroupAuthorizationFailed) {
			g.Error = kafkaErrText(e)
		}
		for t, byTopic := range l.Lag.TotalByTopic() {
			if (topic == "" && tunnel.InKafkaScope(s.db.Name, t)) || t == topic {
				g.Lag[t] = byTopic.Lag
			}
		}
		if len(g.Lag) > 0 {
			out = append(out, g)
		}
	}
	return tunnel.StoreAnswer{Type: "result", Total: int64(len(out)), Data: out}, nil
}

// kafkaText is bytes as text when they are, and as base64 when not, cut to
// what one message may show.
func kafkaText(b []byte, limit int) (string, bool, bool) {
	cut := false
	if len(b) > limit {
		b, cut = b[:limit], true
	}
	if utf8.Valid(b) {
		return string(b), false, cut
	}
	return base64.StdEncoding.EncodeToString(b), true, cut
}

// peek reads the newest messages of a topic, or of one partition, without a
// consumer group: nothing is committed and no other reader is disturbed. It
// is recorded, because reading messages is reading the data.
func (s *kafkaSession) peek(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if !tunnel.ValidKafkaTopic(req.Name) {
		return tunnel.StoreAnswer{Type: "error", Error: "no topic named"}, nil
	}
	n := req.Count
	if n <= 0 {
		n = 20
	}
	n = min(n, s.maxItems)
	only := int32(-1)
	if p, ok := req.Properties["partition"]; ok && p != "" {
		v, err := strconv.ParseInt(p, 10, 32)
		if err != nil || v < 0 {
			return tunnel.StoreAnswer{Type: "error", Error: "the partition is not a number"}, nil
		}
		only = int32(v)
	}
	text := fmt.Sprintf("peek %s last %d", req.Name, n)
	if only >= 0 {
		text = fmt.Sprintf("peek %s partition %d last %d", req.Name, only, n)
	}
	if err := rec.stmt(req.ID, text); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	start := time.Now()
	msgs, truncated, err := s.readNewest(ctx, req.Name, only, n)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		if rerr := rec.fail(req.ID, kafkaErrText(err), elapsed); rerr != nil {
			return tunnel.StoreAnswer{}, rerr
		}
		return tunnel.StoreAnswer{Type: "error", Error: kafkaErrText(err), MS: elapsed}, nil
	}
	if err := rec.done(req.ID, fmt.Sprintf("PEEK %d", len(msgs)), int64(len(msgs)), elapsed, msgs, truncated); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	return tunnel.StoreAnswer{Type: "result", Data: msgs, Total: int64(len(msgs)), MS: elapsed, Truncated: truncated}, nil
}

func (s *kafkaSession) readNewest(ctx context.Context, topic string, only int32, n int) ([]KafkaMessage, bool, error) {
	starts, err := s.adm.ListStartOffsets(ctx, topic)
	if err != nil {
		return nil, false, err
	}
	ends, err := s.adm.ListEndOffsets(ctx, topic)
	if err != nil {
		return nil, false, err
	}
	if e := ends.Error(); e != nil {
		return nil, false, e
	}
	from := map[int32]kgo.Offset{}
	until := map[int32]int64{}
	truncated := false
	ends.Each(func(o kadm.ListedOffset) {
		if only >= 0 && o.Partition != only {
			return
		}
		st, _ := starts.Lookup(topic, o.Partition)
		first := max(o.Offset-int64(n), st.Offset, 0)
		if first > st.Offset {
			truncated = true
		}
		if o.Offset > first {
			from[o.Partition] = kgo.NewOffset().At(first)
			until[o.Partition] = o.Offset
		}
	})
	if only >= 0 && len(from) == 0 {
		if _, ok := ends.Lookup(topic, only); !ok {
			return nil, false, fmt.Errorf("%s has no partition %d", topic, only)
		}
	}
	if len(from) == 0 {
		return []KafkaMessage{}, false, nil
	}
	cl, err := kafkaClient(s.addr, s.user, s.pw, "SCRAM-SHA-512", s.timeout,
		kgo.ConsumePartitions(map[string]map[int32]kgo.Offset{topic: from}),
		kgo.FetchMaxWait(500*time.Millisecond), kgo.FetchMaxPartitionBytes(4<<20))
	if err != nil {
		return nil, false, err
	}
	defer cl.Close()
	var msgs []KafkaMessage
	budget := tunnel.MaxStoreAnswer
	for len(until) > 0 {
		fs := cl.PollFetches(ctx)
		if ctx.Err() != nil {
			break
		}
		var ferr error
		fs.EachError(func(_ string, _ int32, err error) { ferr = err })
		if ferr != nil {
			return nil, false, ferr
		}
		fs.EachRecord(func(r *kgo.Record) {
			end, ok := until[r.Partition]
			if !ok || r.Offset >= end {
				return
			}
			m := KafkaMessage{Partition: r.Partition, Offset: r.Offset, Time: r.Timestamp.UTC().Format(time.RFC3339Nano)}
			m.Key, m.KeyB64, _ = kafkaText(r.Key, 1024)
			m.Value, m.ValueB64, m.Cut = kafkaText(r.Value, 64<<10)
			for _, h := range r.Headers {
				if m.Headers == nil {
					m.Headers = map[string]string{}
				}
				m.Headers[h.Key], _, _ = kafkaText(h.Value, 1024)
			}
			if budget -= len(m.Value) + len(m.Key) + 64; budget < 0 {
				truncated = true
				return
			}
			msgs = append(msgs, m)
			if r.Offset+1 >= end {
				delete(until, r.Partition)
			}
		})
	}
	// Newest first, across partitions, and no more than asked.
	sort.Slice(msgs, func(i, j int) bool {
		if msgs[i].Time != msgs[j].Time {
			return msgs[i].Time > msgs[j].Time
		}
		return msgs[i].Offset > msgs[j].Offset
	})
	if len(msgs) > n {
		msgs, truncated = msgs[:n], true
	}
	return msgs, truncated, nil
}

func (s *kafkaSession) publish(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if !tunnel.ValidKafkaTopic(req.Name) {
		return tunnel.StoreAnswer{Type: "error", Error: "no topic named"}, nil
	}
	if len(req.Payload)+len(req.RoutingKey) > tunnel.MaxCommand {
		return tunnel.StoreAnswer{Type: "error", Error: "the message is too long"}, nil
	}
	r := &kgo.Record{Topic: req.Name, Value: []byte(req.Payload)}
	if req.RoutingKey != "" {
		r.Key = []byte(req.RoutingKey)
	}
	keys := make([]string, 0, len(req.Headers))
	for k := range req.Headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		r.Headers = append(r.Headers, kgo.RecordHeader{Key: k, Value: []byte(req.Headers[k])})
	}
	text := "publish " + req.Name
	if req.RoutingKey != "" {
		text += " key=" + strconv.Quote(req.RoutingKey)
	}
	for _, k := range keys {
		text += " header " + strconv.Quote(k) + "=" + strconv.Quote(req.Headers[k])
	}
	text += " " + req.Payload
	if err := rec.stmt(req.ID, text); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	start := time.Now()
	res := s.cl.ProduceSync(ctx, r)
	elapsed := time.Since(start).Milliseconds()
	if err := res.FirstErr(); err != nil {
		if rerr := rec.fail(req.ID, kafkaErrText(err), elapsed); rerr != nil {
			return tunnel.StoreAnswer{}, rerr
		}
		return tunnel.StoreAnswer{Type: "error", Error: kafkaErrText(err), MS: elapsed}, nil
	}
	out := res[0].Record
	ans := map[string]any{"partition": out.Partition, "offset": out.Offset}
	if err := rec.done(req.ID, "PUBLISH 1", 1, elapsed, ans, false); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	return tunnel.StoreAnswer{Type: "result", Data: ans, MS: elapsed}, nil
}

func (s *kafkaSession) createTopic(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if !tunnel.ValidKafkaTopic(req.Name) {
		return tunnel.StoreAnswer{Type: "error", Error: "a topic name is letters, digits, . _ and -, at most 249 of them"}, nil
	}
	parts := int32(req.Count)
	if parts <= 0 {
		parts = 1
	}
	if parts > 1000 {
		return tunnel.StoreAnswer{Type: "error", Error: "at most 1000 partitions"}, nil
	}
	repl := int16(-1)
	if v := req.Properties["replication"]; v != "" {
		n, err := strconv.ParseInt(v, 10, 16)
		if err != nil || n < 1 {
			return tunnel.StoreAnswer{Type: "error", Error: "the replication factor is not a number"}, nil
		}
		repl = int16(n)
	}
	// How long messages are kept and how old ones go, the same settings
	// as creating a topic from the server's page (tunnel.KafkaCreate).
	spec := tunnel.KafkaCreate{Partitions: int(parts), Cleanup: req.Properties["cleanup"]}
	if v := req.Properties["retention"]; v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return tunnel.StoreAnswer{Type: "error", Error: "keep for is not a number of milliseconds"}, nil
		}
		spec.RetentionMS = n
	}
	if err := spec.Validate(); err != nil {
		return tunnel.StoreAnswer{Type: "error", Error: err.Error()}, nil
	}
	text := fmt.Sprintf("create topic %s partitions %d", req.Name, parts)
	if repl > 0 {
		text += fmt.Sprintf(" replication %d", repl)
	}
	if spec.RetentionMS != 0 {
		text += fmt.Sprintf(" retention.ms %d", spec.RetentionMS)
	}
	if spec.Cleanup != "" {
		text += " cleanup.policy " + spec.Cleanup
	}
	if err := rec.stmt(req.ID, text); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	start := time.Now()
	r, err := s.adm.CreateTopic(ctx, parts, repl, spec.Configs(), req.Name)
	if err == nil {
		err = r.Err
	}
	return s.finish(req.ID, rec, start, err, "CREATE TOPIC")
}

func (s *kafkaSession) deleteTopic(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if !tunnel.ValidKafkaTopic(req.Name) {
		return tunnel.StoreAnswer{Type: "error", Error: "no topic named"}, nil
	}
	text := "delete topic " + req.Name
	if req.Confirm != s.g.Host {
		why := "deleting a topic and every message in it needs you to type the server's name, " + s.g.Host + ", to confirm"
		if err := rec.refused(req.ID, text, why); err != nil {
			return tunnel.StoreAnswer{}, err
		}
		return tunnel.StoreAnswer{Type: "error", NeedsConfirm: true, Error: why}, nil
	}
	if err := rec.stmt(req.ID, text); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	start := time.Now()
	res, err := s.adm.DeleteTopics(ctx, req.Name)
	if err == nil {
		err = res.Error()
	}
	return s.finish(req.ID, rec, start, err, "DELETE TOPIC")
}

func (s *kafkaSession) finish(id uint32, rec *stmtRecorder, start time.Time, err error, tag string) (tunnel.StoreAnswer, error) {
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		if rerr := rec.fail(id, kafkaErrText(err), elapsed); rerr != nil {
			return tunnel.StoreAnswer{}, rerr
		}
		return tunnel.StoreAnswer{Type: "error", Error: kafkaErrText(err), MS: elapsed}, nil
	}
	if err := rec.done(id, tag, 0, elapsed, nil, false); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	return tunnel.StoreAnswer{Type: "result", MS: elapsed}, nil
}

// discoverKafka lists a cluster's topics as its administrator, so the add
// form can show what an entry's scope would cover.
func discoverKafka(ctx context.Context, d tunnel.GrantDiscover) tunnel.DiscoverResult {
	port := d.Port
	if port == 0 {
		port = 9092
	}
	s := &kafkaSession{addr: net.JoinHostPort(d.Host, strconv.Itoa(port)), file: d.ConfigPath}
	if s.file == "" {
		s.file = tunnel.KafkaAdminFile
	}
	cl, err := s.adminClient(ctx)
	if err != nil {
		return tunnel.DiscoverResult{Error: err.Error()}
	}
	defer cl.Close()
	td, err := kadm.NewClient(cl).ListTopics(ctx)
	if err != nil {
		return tunnel.DiscoverResult{Error: "Kafka would not list its topics: " + kafkaErrText(err)}
	}
	res := tunnel.DiscoverResult{OK: true}
	for _, t := range td.Sorted() {
		if t.IsInternal {
			continue
		}
		res.Databases = append(res.Databases, tunnel.Discovered{Name: t.Topic})
	}
	if len(res.Databases) > 1000 {
		res.Databases, res.Truncated = res.Databases[:1000], true
	}
	return res
}
