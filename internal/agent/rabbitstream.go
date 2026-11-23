package agent

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/gsoultan/argus/internal/tunnel"
)

// StreamMessage is one message read from a stream.
type StreamMessage struct {
	Offset int64 `json:"offset"`
	// Time is the message's timestamp property, or the time its chunk was
	// written, in Unix milliseconds; 0 when neither is known.
	Time       int64          `json:"time,omitempty"`
	Properties map[string]any `json:"properties,omitempty"`
	Headers    map[string]any `json:"headers,omitempty"`
	Body       string         `json:"body"`
	// Encoding is string, or base64 for a body that is not UTF-8.
	Encoding  string `json:"encoding"`
	Size      int    `json:"size"`
	Truncated bool   `json:"truncated,omitempty"`
}

// StreamReadResult is one page of a stream.
type StreamReadResult struct {
	Messages []StreamMessage `json:"messages"`
	// First and Last are the stream's first and last offsets now; Empty
	// when it holds no message.
	First int64 `json:"first"`
	Last  int64 `json:"last"`
	Empty bool  `json:"empty,omitempty"`
}

// streamWait is how long a read waits for a message it expects. A stream
// hands over a chunk at a time, so a gap this long means none is coming.
const streamWait = 1500 * time.Millisecond

// edgeWait is the wait when finding a stream's ends, which come in one
// chunk the server already has.
const edgeWait = 500 * time.Millisecond

// amqpDial opens one AMQP connection to the vhost as the person's user, on
// the loopback; with only TLS there, without checking its certificate, as
// the management API on the loopback is not checked either.
func (s *rabbitSession) amqpDial(ctx context.Context) (*amqp.Connection, error) {
	plain, tlsPort := s.amqpPorts(ctx)
	scheme, port := "amqp", plain
	if plain == 0 && tlsPort != 0 {
		scheme, port = "amqps", tlsPort
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	uri := (&url.URL{Scheme: scheme, User: url.UserPassword(s.user, s.pw), Host: addr, Path: "/" + s.vhost}).String()
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cfg := amqp.Config{Vhost: s.vhost, Heartbeat: 10 * time.Second, Properties: amqp.Table{"connection_name": "argus " + s.user},
		Dial: func(network, a string) (net.Conn, error) { return (&net.Dialer{}).DialContext(dctx, network, a) }}
	if scheme == "amqps" {
		cfg.TLSClientConfig = s.http.Transport.(*http.Transport).TLSClientConfig
	}
	conn, err := amqp.DialConfig(uri, cfg)
	if err != nil {
		return nil, fmt.Errorf("could not open an AMQP connection on %s: %w", addr, err)
	}
	return conn, nil
}

// streamRead reads up to count messages of stream name as a reader would:
// basic.consume with x-stream-offset, then cancel. Nothing is taken.
func (s *rabbitSession) streamRead(ctx context.Context, name string, count int, r *tunnel.RabbitStreamRead) (StreamReadResult, error) {
	res := StreamReadResult{Messages: []StreamMessage{}}
	var q struct {
		Type string `json:"type"`
	}
	if _, err := s.call(ctx, http.MethodGet, "/api/queues/"+s.v()+"/"+esc(name)+"?columns=type", nil, &q); err != nil {
		return res, err
	}
	if q.Type != "stream" {
		return res, errors.New(name + " is not a stream; peek a queue instead")
	}
	conn, err := s.amqpDial(ctx)
	if err != nil {
		return res, err
	}
	defer func() { _ = conn.Close() }()

	// The ends first: where the stream starts, and where it ends now.
	first, ok, err := s.streamEdge(ctx, conn, name, "first")
	if err != nil {
		return res, err
	}
	if !ok {
		res.Empty = true
		return res, nil
	}
	last, _, err := s.streamEdge(ctx, conn, name, "last")
	if err != nil {
		return res, err
	}
	res.First, res.Last = first, last

	var at any
	from := first
	switch r.From {
	case "first":
		at = first
	case "last":
		from = max(first, last-int64(count)+1)
		at = from
	case "offset":
		from = max(first, r.Offset)
		at = from
	case "time":
		at = time.UnixMilli(r.At)
		from = first
	}
	if from > last {
		return res, nil
	}
	err = s.consumeStream(ctx, conn, name, at, streamWait, func(d amqp.Delivery, off int64) bool {
		if off < from {
			return true
		}
		res.Messages = append(res.Messages, streamMessage(d, off))
		return len(res.Messages) < count && off < last
	})
	return res, err
}

// streamEdge is the offset of the first message of the stream, or of its
// last one.
func (s *rabbitSession) streamEdge(ctx context.Context, conn *amqp.Connection, name, edge string) (int64, bool, error) {
	var off int64
	seen := false
	err := s.consumeStream(ctx, conn, name, edge, edgeWait, func(_ amqp.Delivery, o int64) bool {
		off, seen = o, true
		// first stops at once; last reads its chunk to its end.
		return edge == "last"
	})
	return off, seen, err
}

// consumeStream consumes stream name from at on a channel of its own,
// handing each message and its offset to take until take says stop or no
// message comes for wait.
func (s *rabbitSession) consumeStream(ctx context.Context, conn *amqp.Connection, name string, at any, wait time.Duration,
	take func(amqp.Delivery, int64) bool) error {
	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	defer func() { _ = ch.Close() }()
	// A stream needs a prefetch, and an acknowledgement to send more.
	if err := ch.Qos(100, 0, false); err != nil {
		return err
	}
	tag := "argus-read-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	msgs, err := ch.ConsumeWithContext(ctx, name, tag, false, false, false, false, amqp.Table{"x-stream-offset": at})
	if err != nil {
		return streamErr(err)
	}
	defer func() { _ = ch.Cancel(tag, false) }()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		case d, open := <-msgs:
			if !open {
				return errors.New("RabbitMQ closed the channel while the stream was read")
			}
			_ = d.Ack(false)
			off, ok := streamOffset(d.Headers["x-stream-offset"])
			if !ok {
				continue
			}
			if !take(d, off) {
				return nil
			}
			timer.Reset(wait)
		}
	}
}

func streamErr(err error) error {
	var ae *amqp.Error
	if errors.As(err, &ae) && ae.Code == amqp.AccessRefused {
		return errors.New("your user may not read this stream: " + ae.Reason)
	}
	return err
}

func streamOffset(v any) (int64, bool) {
	switch o := v.(type) {
	case int64:
		return o, true
	case int32:
		return int64(o), true
	case int:
		return int64(o), true
	case uint64:
		return int64(o), true //nolint:gosec // offsets fit
	}
	return 0, false
}

func streamMessage(d amqp.Delivery, off int64) StreamMessage {
	m := StreamMessage{Offset: off, Size: len(d.Body)}
	if !d.Timestamp.IsZero() {
		m.Time = d.Timestamp.UnixMilli()
	} else if ts, ok := d.Headers["x-stream-chunk-timestamp"]; ok {
		if t, ok := ts.(time.Time); ok {
			m.Time = t.UnixMilli()
		} else if n, ok := streamOffset(ts); ok {
			m.Time = n
		}
	}
	props := map[string]any{}
	for k, v := range map[string]string{"content_type": d.ContentType, "content_encoding": d.ContentEncoding,
		"message_id": d.MessageId, "correlation_id": d.CorrelationId, "reply_to": d.ReplyTo, "type": d.Type,
		"app_id": d.AppId, "user_id": d.UserId, "expiration": d.Expiration} {
		if v != "" {
			props[k] = v
		}
	}
	if d.DeliveryMode != 0 {
		props["delivery_mode"] = d.DeliveryMode
	}
	if d.RoutingKey != "" {
		props["routing_key"] = d.RoutingKey
	}
	if len(props) > 0 {
		m.Properties = props
	}
	headers := map[string]any{}
	for k, v := range d.Headers {
		if strings.HasPrefix(k, "x-stream-") {
			continue
		}
		if t, ok := v.(time.Time); ok {
			v = t.UTC().Format(time.RFC3339)
		}
		headers[k] = v
	}
	if len(headers) > 0 {
		m.Headers = headers
	}
	body := d.Body
	if len(body) > tunnel.MaxCell {
		body, m.Truncated = body[:tunnel.MaxCell], true
	}
	if utf8.Valid(body) || m.Truncated && utf8.Valid(body[:max(0, len(body)-3)]) {
		m.Body, m.Encoding = strings.ToValidUTF8(string(body), ""), "string"
	} else {
		m.Body, m.Encoding = base64.StdEncoding.EncodeToString(body), "base64"
	}
	return m
}

// StreamReader is one reader of a stream: a stream-protocol consumer with
// its place, or an AMQP consumer, which keeps none on the server.
type StreamReader struct {
	Name       string `json:"name"`
	Protocol   string `json:"protocol"`
	Offset     *int64 `json:"offset,omitempty"`
	Behind     *int64 `json:"behind,omitempty"`
	Connection string `json:"connection,omitempty"`
	From       string `json:"from,omitempty"`
	User       string `json:"user,omitempty"`
}

// StreamReaders is who reads a stream and who writes to it.
type StreamReaders struct {
	Readers    []StreamReader `json:"readers"`
	Publishers []StreamReader `json:"publishers"`
	// NoPlugin is set when rabbitmq_stream_management is not enabled, so
	// stream-protocol readers cannot be listed.
	NoPlugin bool `json:"noPlugin,omitempty"`
}

// streamReaders lists the readers of stream name: stream-protocol ones from
// the stream management plugin, AMQP ones from the vhost's consumers.
func (s *rabbitSession) streamReaders(ctx context.Context, name string) (StreamReaders, error) {
	out := StreamReaders{Readers: []StreamReader{}, Publishers: []StreamReader{}}
	type streamRow struct {
		Stream     map[string]any `json:"stream"`
		Queue      map[string]any `json:"queue"`
		Properties map[string]any `json:"properties"`
		Offset     *int64         `json:"offset"`
		OffsetLag  *int64         `json:"offset_lag"`
		Reference  string         `json:"reference"`
		Conn       map[string]any `json:"connection_details"`
	}
	var consumers, publishers []streamRow
	_, err := s.call(ctx, http.MethodGet, "/api/stream/consumers/"+s.v(), nil, &consumers)
	var re errRabbit
	switch {
	case errors.As(err, &re) && (re.status == http.StatusNotFound || re.status == http.StatusMethodNotAllowed):
		out.NoPlugin = true
	case err != nil:
		return out, err
	default:
		if _, err := s.call(ctx, http.MethodGet, "/api/stream/publishers/"+s.v(), nil, &publishers); err != nil {
			return out, err
		}
	}
	str := func(m map[string]any, k string) string {
		v, _ := m[k].(string)
		return v
	}
	rowStream := func(r streamRow) string {
		if n := str(r.Queue, "name"); n != "" {
			return n
		}
		return str(r.Stream, "name")
	}
	from := func(c map[string]any) string {
		peer := str(c, "peer_host")
		if p, ok := c["peer_port"].(float64); ok && peer != "" {
			peer = net.JoinHostPort(peer, strconv.Itoa(int(p)))
		}
		return peer
	}
	for _, r := range consumers {
		if rowStream(r) != name {
			continue
		}
		rd := StreamReader{Name: str(r.Properties, "name"), Protocol: "stream", Offset: r.Offset, Behind: r.OffsetLag,
			Connection: str(r.Conn, "name"), From: from(r.Conn), User: str(r.Conn, "user")}
		if rd.Name == "" {
			rd.Name = "(no name)"
		}
		out.Readers = append(out.Readers, rd)
	}
	for _, r := range publishers {
		if rowStream(r) != name {
			continue
		}
		p := StreamReader{Name: r.Reference, Protocol: "stream", Connection: str(r.Conn, "name"), From: from(r.Conn),
			User: str(r.Conn, "user")}
		if p.Name == "" {
			p.Name = "(no reference)"
		}
		out.Publishers = append(out.Publishers, p)
	}
	var amqpConsumers []struct {
		Tag   string         `json:"consumer_tag"`
		Queue map[string]any `json:"queue"`
		Ch    map[string]any `json:"channel_details"`
	}
	if _, err := s.call(ctx, http.MethodGet, "/api/consumers/"+s.v(), nil, &amqpConsumers); err != nil {
		return out, err
	}
	for _, c := range amqpConsumers {
		if str(c.Queue, "name") != name || strings.HasPrefix(c.Tag, "argus-read-") {
			continue
		}
		out.Readers = append(out.Readers, StreamReader{Name: c.Tag, Protocol: "amqp", Connection: str(c.Ch, "connection_name"),
			From: from(c.Ch), User: str(c.Ch, "user")})
	}
	return out, nil
}

// SuperStream is one super stream on the vhost.
type SuperStream struct {
	Name       string                 `json:"name"`
	Partitions []SuperStreamPartition `json:"partitions"`
	Messages   int64                  `json:"messages"`
}

// SuperStreamPartition is one partition of a super stream.
type SuperStreamPartition struct {
	Stream     string `json:"stream"`
	RoutingKey string `json:"routingKey"`
	Order      int    `json:"order"`
	Messages   int64  `json:"messages"`
	Missing    bool   `json:"missing,omitempty"`
}

// superStreams lists the vhost's super streams: exchanges with
// x-super-stream, and the streams bound to them.
func (s *rabbitSession) superStreams(ctx context.Context) ([]SuperStream, error) {
	var exchanges []struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if _, err := s.call(ctx, http.MethodGet, "/api/exchanges/"+s.v()+"?columns=name,arguments", nil, &exchanges); err != nil {
		return nil, err
	}
	var bindings []struct {
		Source    string         `json:"source"`
		Dest      string         `json:"destination"`
		DestType  string         `json:"destination_type"`
		Key       string         `json:"routing_key"`
		Arguments map[string]any `json:"arguments"`
	}
	if _, err := s.call(ctx, http.MethodGet, "/api/bindings/"+s.v(), nil, &bindings); err != nil {
		return nil, err
	}
	var queues []struct {
		Name     string `json:"name"`
		Messages int64  `json:"messages"`
	}
	if _, err := s.call(ctx, http.MethodGet, "/api/queues/"+s.v()+"?columns=name,messages", nil, &queues); err != nil {
		return nil, err
	}
	counts := map[string]int64{}
	for _, q := range queues {
		counts[q.Name] = q.Messages
	}
	out := []SuperStream{}
	for _, e := range exchanges {
		if v, ok := e.Arguments["x-super-stream"]; !ok || v == false {
			continue
		}
		ss := SuperStream{Name: e.Name, Partitions: []SuperStreamPartition{}}
		for _, b := range bindings {
			if b.Source != e.Name || b.DestType != "queue" {
				continue
			}
			order := len(ss.Partitions)
			if o, ok := b.Arguments["x-super-stream-partition-order"].(float64); ok {
				order = int(o)
			}
			n, ok := counts[b.Dest]
			ss.Partitions = append(ss.Partitions, SuperStreamPartition{Stream: b.Dest, RoutingKey: b.Key, Order: order,
				Messages: n, Missing: !ok})
			ss.Messages += n
		}
		sortPartitions(ss.Partitions)
		out = append(out, ss)
	}
	return out, nil
}

func sortPartitions(p []SuperStreamPartition) {
	for i := 1; i < len(p); i++ {
		for j := i; j > 0 && p[j].Order < p[j-1].Order; j-- {
			p[j], p[j-1] = p[j-1], p[j]
		}
	}
}

// declareSuperStream makes the exchange, one stream per partition and the
// bindings, as rabbitmq-streams add_super_stream does. What it made is
// deleted again when a later step fails.
func (s *rabbitSession) declareSuperStream(ctx context.Context, name string, spec tunnel.RabbitSuperStream) error {
	var existing struct {
		Name string `json:"name"`
	}
	if _, err := s.call(ctx, http.MethodGet, "/api/exchanges/"+s.v()+"/"+esc(name), nil, &existing); err == nil {
		return errors.New("an exchange named " + name + " is here already")
	}
	keys := spec.SuperStreamKeys()
	for _, k := range keys {
		if _, err := s.call(ctx, http.MethodGet, "/api/queues/"+s.v()+"/"+esc(name+"-"+k)+"?columns=name", nil, &existing); err == nil {
			return errors.New("a queue named " + name + "-" + k + " is here already")
		}
	}
	var made []string
	undo := func(err error) error {
		uctx := context.WithoutCancel(ctx)
		for _, q := range made {
			_, _ = s.call(uctx, http.MethodDelete, "/api/queues/"+s.v()+"/"+esc(q), nil, nil)
		}
		_, _ = s.call(uctx, http.MethodDelete, "/api/exchanges/"+s.v()+"/"+esc(name), nil, nil)
		return err
	}
	if _, err := s.call(ctx, http.MethodPut, "/api/exchanges/"+s.v()+"/"+esc(name), map[string]any{"type": "direct",
		"durable": true, "auto_delete": false, "internal": false, "arguments": map[string]any{"x-super-stream": true}}, nil); err != nil {
		return err
	}
	args := map[string]any{"x-queue-type": "stream"}
	if spec.MaxAgeS > 0 {
		args["x-max-age"] = strconv.FormatInt(spec.MaxAgeS, 10) + "s"
	}
	if spec.MaxLengthBytes > 0 {
		args["x-max-length-bytes"] = spec.MaxLengthBytes
	}
	for i, k := range keys {
		q := name + "-" + k
		if _, err := s.call(ctx, http.MethodPut, "/api/queues/"+s.v()+"/"+esc(q),
			map[string]any{"durable": true, "auto_delete": false, "arguments": args}, nil); err != nil {
			return undo(err)
		}
		made = append(made, q)
		if _, err := s.call(ctx, http.MethodPost, "/api/bindings/"+s.v()+"/e/"+esc(name)+"/q/"+esc(q),
			map[string]any{"routing_key": k, "arguments": map[string]any{"x-super-stream-partition-order": i}}, nil); err != nil {
			return undo(err)
		}
	}
	return nil
}

// deleteSuperStream deletes a super stream's partition streams, then its
// exchange.
func (s *rabbitSession) deleteSuperStream(ctx context.Context, name string) (int, error) {
	all, err := s.superStreams(ctx)
	if err != nil {
		return 0, err
	}
	for _, ss := range all {
		if ss.Name != name {
			continue
		}
		n := 0
		for _, p := range ss.Partitions {
			if p.Missing {
				continue
			}
			if _, err := s.call(ctx, http.MethodDelete, "/api/queues/"+s.v()+"/"+esc(p.Stream), nil, nil); err != nil {
				return n, fmt.Errorf("deleted %d of its streams, then %s failed: %w", n, p.Stream, err)
			}
			n++
		}
		_, err := s.call(ctx, http.MethodDelete, "/api/exchanges/"+s.v()+"/"+esc(name), nil, nil)
		return n, err
	}
	return 0, errors.New(name + " is not a super stream here")
}
