package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
A Kafka entry's topic settings, partitions, message deletion and consumer
groups (tunnel/kafkachange.go has the rules). Every change runs as the
person's own Kafka user, whose ACLs at Manage cover ALTER_CONFIGS, ALTER and
DELETE on the scope's topics and READ and DELETE on consumer groups, so
Kafka's authorizer decides too.

Deleting messages and moving a group take the exact offsets a dry run
worked out, so what is recorded and approved is what runs.
*/

// KafkaConfig is one topic setting as Kafka reports it.
type KafkaConfig struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	Source    string `json:"source"`
	Default   bool   `json:"default"`
	Sensitive bool   `json:"sensitive,omitempty"`
	// Editable says Argus offers to change it.
	Editable bool `json:"editable,omitempty"`
}

// KafkaOffsetPlan is one partition of a dry run: where it is, where it
// would be, and how many messages that is.
type KafkaOffsetPlan struct {
	Topic     string `json:"topic"`
	Partition int32  `json:"partition"`
	Start     int64  `json:"start"`
	End       int64  `json:"end"`
	// Now is the group's committed offset (group_reset, -1 for none), or
	// the partition's start (delete_records).
	Now   int64 `json:"now"`
	After int64 `json:"after"`
}

// KafkaDryRun is a dry run's answer: the exact offsets a run would take,
// and the groups still to read messages being deleted.
type KafkaDryRun struct {
	Plan    []KafkaOffsetPlan          `json:"plan"`
	Offsets map[string]map[int32]int64 `json:"offsets"`
	// Unread names the consumer groups with messages to read among those
	// to be deleted, and how many.
	Unread map[string]int64 `json:"unread,omitempty"`
}

// KafkaGroupDetail is one consumer group: its members and, per partition of
// the entry's topics, where it is.
type KafkaGroupDetail struct {
	Group   string            `json:"group"`
	State   string            `json:"state"`
	Members []KafkaMember     `json:"members"`
	Offsets []KafkaOffsetPlan `json:"offsets"`
	// Outside counts topics outside the entry the group has offsets on,
	// where the person's user can see them; a delete asks the administrator.
	Outside int `json:"outside"`
}

// KafkaMember is one member of a group.
type KafkaMember struct {
	ID     string   `json:"id"`
	Client string   `json:"client"`
	Host   string   `json:"host"`
	Topics []string `json:"topics,omitempty"`
}

func (s *kafkaSession) changeOp(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if why := tunnel.ValidKafkaChange(s.db.Name, req); why != "" {
		return tunnel.StoreAnswer{Type: "error", Error: why}, nil
	}
	c := req.KafkaChange
	if c == nil {
		c = &tunnel.KafkaChange{}
	}
	switch req.Op {
	case tunnel.KafkaOpTopicConfigs:
		return s.topicConfigs(ctx, req.Name)
	case tunnel.KafkaOpGroup:
		d, err := s.groupDetail(ctx, c.Group)
		if err != nil {
			return kafkaErrAnswer(err), nil
		}
		return tunnel.StoreAnswer{Type: "result", Data: d}, nil
	}
	if c.DryRun {
		var d KafkaDryRun
		var err error
		if req.Op == tunnel.KafkaOpDeleteRecords {
			d, err = s.planDelete(ctx, req.Name, c)
		} else {
			d, err = s.planReset(ctx, req.Name, c)
		}
		if err != nil {
			return tunnel.StoreAnswer{Type: "error", Error: kafkaErrText(err)}, nil
		}
		return tunnel.StoreAnswer{Type: "result", Data: d}, nil
	}
	text := tunnel.KafkaChangeText(req)
	refuse := func(why string, extra ...func(*tunnel.StoreAnswer)) (tunnel.StoreAnswer, error) {
		if err := rec.refused(req.ID, text, why); err != nil {
			return tunnel.StoreAnswer{}, err
		}
		a := tunnel.StoreAnswer{Type: "error", Error: why}
		for _, f := range extra {
			f(&a)
		}
		return a, nil
	}
	if req.Op == tunnel.KafkaOpDeleteRecords && req.Confirm != s.g.Host {
		return refuse("deleting messages needs you to type the server's name, "+s.g.Host+", to confirm",
			func(a *tunnel.StoreAnswer) { a.NeedsConfirm = true })
	}
	if s.db.ChangesNeedApproval && req.Approval == "" {
		return refuse("on "+s.g.Host+" this change runs once someone else approves it",
			func(a *tunnel.StoreAnswer) { a.NeedsApproval = &tunnel.RedisApprovalNeed{Text: text} })
	}
	// What only the cluster knows: a group must be empty, and must not
	// reach past the entry to be deleted.
	if req.Op == tunnel.KafkaOpGroupReset || req.Op == tunnel.KafkaOpDeleteGroup {
		d, err := s.groupDetail(ctx, c.Group)
		if err != nil {
			return refuse(kafkaErrText(err))
		}
		if d.State != "Empty" && d.State != "Dead" {
			return refuse(fmt.Sprintf("%s is %s with %d member%s; Kafka moves or deletes a group only once its consumers stop",
				c.Group, d.State, len(d.Members), plural(len(d.Members))))
		}
		if req.Op == tunnel.KafkaOpDeleteGroup && s.db.Name != tunnel.KafkaAllTopics {
			// The person's user sees only the scope's topics, so ask as the
			// administrator what else the group has offsets on.
			n, err := s.groupOutside(ctx, c.Group)
			if err != nil {
				return refuse(kafkaErrText(err))
			}
			d.Outside = n
		}
		if req.Op == tunnel.KafkaOpDeleteGroup && d.Outside > 0 {
			return refuse(fmt.Sprintf("%s also has offsets on %d topic%s outside this entry; delete it from an entry that covers every topic (*)",
				c.Group, d.Outside, plural(d.Outside)))
		}
	}
	if err := rec.stmt(req.ID, text); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	start := time.Now()
	var err error
	tag := ""
	switch req.Op {
	case tunnel.KafkaOpAlterConfigs:
		var alter []kadm.AlterConfig
		keys := make([]string, 0, len(c.Set))
		for k := range c.Set {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			alter = append(alter, kadm.AlterConfig{Op: kadm.SetConfig, Name: k, Value: kadm.StringPtr(c.Set[k])})
		}
		for _, k := range c.Reset {
			alter = append(alter, kadm.AlterConfig{Op: kadm.DeleteConfig, Name: k})
		}
		var res kadm.AlterConfigsResponses
		if res, err = s.adm.AlterTopicConfigs(ctx, alter, req.Name); err == nil {
			for _, r := range res {
				if r.Err != nil {
					err = r.Err
				}
			}
		}
		tag = "ALTER TOPIC"
	case tunnel.KafkaOpAddPartitions:
		var res kadm.CreatePartitionsResponses
		if res, err = s.adm.CreatePartitions(ctx, c.Add, req.Name); err == nil {
			for _, r := range res {
				if r.Err != nil {
					err = r.Err
				}
			}
		}
		tag = "ADD PARTITIONS"
	case tunnel.KafkaOpDeleteRecords:
		var os kadm.Offsets
		for p, o := range c.Offsets[req.Name] {
			os.AddOffset(req.Name, p, o, -1)
		}
		var res kadm.DeleteRecordsResponses
		if res, err = s.adm.DeleteRecords(ctx, os); err == nil {
			err = res.Error()
		}
		tag = "DELETE MESSAGES"
	case tunnel.KafkaOpGroupReset:
		var os kadm.Offsets
		for t, parts := range c.Offsets {
			for p, o := range parts {
				os.AddOffset(t, p, o, -1)
			}
		}
		var res kadm.OffsetResponses
		if res, err = s.adm.CommitOffsets(ctx, c.Group, os); err == nil {
			err = res.Error()
		}
		tag = "MOVE GROUP"
	case tunnel.KafkaOpDeleteGroup:
		var res kadm.DeleteGroupResponses
		if res, err = s.adm.DeleteGroups(ctx, c.Group); err == nil {
			_, err = res.On(c.Group, nil)
			if err == nil {
				err = res[c.Group].Err
			}
		}
		tag = "DELETE GROUP"
	}
	return s.finish(req.ID, rec, start, err, tag)
}

func (s *kafkaSession) topicConfigs(ctx context.Context, topic string) (tunnel.StoreAnswer, error) {
	rc, err := s.adm.DescribeTopicConfigs(ctx, topic)
	if err != nil {
		return kafkaErrAnswer(err), nil
	}
	out := []KafkaConfig{}
	for _, r := range rc {
		if r.Err != nil {
			return kafkaErrAnswer(r.Err), nil
		}
		for _, c := range r.Configs {
			kc := KafkaConfig{Key: c.Key, Source: c.Source.String(), Sensitive: c.Sensitive,
				Default: c.Source.String() != "DYNAMIC_TOPIC_CONFIG"}
			if c.Value != nil && !c.Sensitive {
				kc.Value = *c.Value
			}
			_, kc.Editable = tunnel.KafkaSettingFor(c.Key)
			out = append(out, kc)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return tunnel.StoreAnswer{Type: "result", Data: map[string]any{"configs": out, "settings": tunnel.KafkaSettings}}, nil
}

func (s *kafkaSession) offsetsOf(ctx context.Context, topics ...string) (starts, ends kadm.ListedOffsets, err error) {
	if starts, err = s.adm.ListStartOffsets(ctx, topics...); err != nil {
		return
	}
	if err = starts.Error(); err != nil {
		return
	}
	if ends, err = s.adm.ListEndOffsets(ctx, topics...); err != nil {
		return
	}
	err = ends.Error()
	return
}

// planDelete works out each partition's new start.
func (s *kafkaSession) planDelete(ctx context.Context, topic string, c *tunnel.KafkaChange) (KafkaDryRun, error) {
	starts, ends, err := s.offsetsOf(ctx, topic)
	if err != nil {
		return KafkaDryRun{}, err
	}
	var at kadm.ListedOffsets
	if c.To == "time" {
		if at, err = s.adm.ListOffsetsAfterMilli(ctx, c.At, topic); err != nil {
			return KafkaDryRun{}, err
		}
	}
	d := KafkaDryRun{Offsets: map[string]map[int32]int64{topic: {}}, Unread: map[string]int64{}}
	found := false
	ends.Each(func(e kadm.ListedOffset) {
		if c.Partition != nil && e.Partition != *c.Partition {
			return
		}
		found = true
		st, _ := starts.Lookup(topic, e.Partition)
		after := st.Offset
		switch c.To {
		case "all":
			after = e.Offset
		case "offset":
			after = min(max(c.Offset, st.Offset), e.Offset)
		case "time":
			if o, ok := at.Lookup(topic, e.Partition); ok && o.Err == nil && o.Offset >= 0 {
				after = min(max(o.Offset, st.Offset), e.Offset)
			} else {
				after = e.Offset
			}
		}
		if after == st.Offset {
			return
		}
		d.Plan = append(d.Plan, KafkaOffsetPlan{Topic: topic, Partition: e.Partition, Start: st.Offset, End: e.Offset, Now: st.Offset, After: after})
		d.Offsets[topic][e.Partition] = after
	})
	if !found {
		return d, fmt.Errorf("%s has no such partition", topic)
	}
	sortPlan(d.Plan)
	// Groups that have not read what goes.
	if listed, err := s.adm.ListGroups(ctx); err == nil && len(d.Plan) > 0 {
		if lags, err := s.adm.Lag(ctx, listed.Groups()...); err == nil {
			for _, l := range lags.Sorted() {
				for _, p := range d.Plan {
					gl, ok := l.Lag.Lookup(topic, p.Partition)
					if !ok || gl.Commit.At < 0 {
						continue
					}
					if gl.Commit.At < p.After {
						d.Unread[l.Group] += p.After - max(gl.Commit.At, p.Start)
					}
				}
			}
		}
	}
	return d, nil
}

// planReset works out a group's new committed offsets.
func (s *kafkaSession) planReset(ctx context.Context, topic string, c *tunnel.KafkaChange) (KafkaDryRun, error) {
	g, err := s.groupDetail(ctx, c.Group)
	if err != nil {
		return KafkaDryRun{}, err
	}
	topics := slices.Clone(c.Topics)
	if topic != "" {
		topics = []string{topic}
	}
	if len(topics) == 0 {
		for _, o := range g.Offsets {
			if !slices.Contains(topics, o.Topic) {
				topics = append(topics, o.Topic)
			}
		}
	}
	if len(topics) == 0 {
		return KafkaDryRun{}, errors.New(c.Group + " has no offsets on this entry's topics; name a topic")
	}
	starts, ends, err := s.offsetsOf(ctx, topics...)
	if err != nil {
		return KafkaDryRun{}, err
	}
	var at kadm.ListedOffsets
	if c.To == "time" {
		if at, err = s.adm.ListOffsetsAfterMilli(ctx, c.At, topics...); err != nil {
			return KafkaDryRun{}, err
		}
	}
	now := map[string]map[int32]int64{}
	for _, o := range g.Offsets {
		if now[o.Topic] == nil {
			now[o.Topic] = map[int32]int64{}
		}
		now[o.Topic][o.Partition] = o.Now
	}
	d := KafkaDryRun{Offsets: map[string]map[int32]int64{}}
	ends.Each(func(e kadm.ListedOffset) {
		if c.Partition != nil && e.Partition != *c.Partition {
			return
		}
		st, _ := starts.Lookup(e.Topic, e.Partition)
		cur, has := now[e.Topic][e.Partition]
		if !has {
			cur = -1
		}
		var after int64
		switch c.To {
		case "earliest":
			after = st.Offset
		case "latest":
			after = e.Offset
		case "time":
			after = e.Offset
			if o, ok := at.Lookup(e.Topic, e.Partition); ok && o.Err == nil && o.Offset >= 0 {
				after = o.Offset
			}
		case "shift":
			base := cur
			if base < 0 {
				base = e.Offset
			}
			after = base + c.By
		case "offset":
			after = c.Offset
		}
		after = min(max(after, st.Offset), e.Offset)
		d.Plan = append(d.Plan, KafkaOffsetPlan{Topic: e.Topic, Partition: e.Partition, Start: st.Offset, End: e.Offset, Now: cur, After: after})
		if d.Offsets[e.Topic] == nil {
			d.Offsets[e.Topic] = map[int32]int64{}
		}
		d.Offsets[e.Topic][e.Partition] = after
	})
	sortPlan(d.Plan)
	return d, nil
}

func sortPlan(p []KafkaOffsetPlan) {
	sort.Slice(p, func(i, j int) bool {
		if p[i].Topic != p[j].Topic {
			return p[i].Topic < p[j].Topic
		}
		return p[i].Partition < p[j].Partition
	})
}

// groupOutside counts the topics outside the entry a group has offsets on,
// asked as Kafka's administrator.
func (s *kafkaSession) groupOutside(ctx context.Context, group string) (int, error) {
	cl, err := s.adminClient(ctx)
	if err != nil {
		return 0, err
	}
	defer cl.Close()
	fetched, err := kadm.NewClient(cl).FetchOffsets(ctx, group)
	if err != nil {
		return 0, err
	}
	if err := fetched.Error(); err != nil {
		return 0, err
	}
	outside := map[string]bool{}
	fetched.Each(func(o kadm.OffsetResponse) {
		if !tunnel.InKafkaScope(s.db.Name, o.Topic) {
			outside[o.Topic] = true
		}
	})
	return len(outside), nil
}

func (s *kafkaSession) groupDetail(ctx context.Context, group string) (KafkaGroupDetail, error) {
	d := KafkaGroupDetail{Group: group, Members: []KafkaMember{}, Offsets: []KafkaOffsetPlan{}}
	described, err := s.adm.DescribeGroups(ctx, group)
	if err != nil {
		return d, err
	}
	dg, ok := described[group]
	if !ok {
		return d, errors.New("there is no consumer group " + group)
	}
	if dg.Err != nil && !errors.Is(dg.Err, kerr.GroupIDNotFound) {
		return d, dg.Err
	}
	d.State = dg.State
	for _, m := range dg.Members {
		km := KafkaMember{ID: m.MemberID, Client: m.ClientID, Host: m.ClientHost}
		if a, ok := m.Assigned.AsConsumer(); ok {
			for _, t := range a.Topics {
				km.Topics = append(km.Topics, t.Topic)
			}
		}
		d.Members = append(d.Members, km)
	}
	fetched, err := s.adm.FetchOffsets(ctx, group)
	if err != nil {
		return d, err
	}
	if err := fetched.Error(); err != nil {
		return d, err
	}
	var topics []string
	outside := map[string]bool{}
	fetched.Each(func(o kadm.OffsetResponse) {
		if !tunnel.InKafkaScope(s.db.Name, o.Topic) {
			outside[o.Topic] = true
			return
		}
		if !slices.Contains(topics, o.Topic) {
			topics = append(topics, o.Topic)
		}
	})
	d.Outside = len(outside)
	if len(topics) == 0 {
		return d, nil
	}
	starts, ends, err := s.offsetsOf(ctx, topics...)
	if err != nil {
		return d, err
	}
	fetched.Each(func(o kadm.OffsetResponse) {
		if !tunnel.InKafkaScope(s.db.Name, o.Topic) {
			return
		}
		st, _ := starts.Lookup(o.Topic, o.Partition)
		e, _ := ends.Lookup(o.Topic, o.Partition)
		d.Offsets = append(d.Offsets, KafkaOffsetPlan{Topic: o.Topic, Partition: o.Partition, Start: st.Offset, End: e.Offset, Now: o.At, After: o.At})
	})
	sortPlan(d.Offsets)
	return d, nil
}
