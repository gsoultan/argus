package tunnel

import (
	"errors"
	"strconv"
)

// RabbitQueue is a new queue's type and settings, for declare_queue. Each
// field becomes one x- argument RabbitMQ documents for that type; a field
// the type does not take is refused, not dropped, so what the person set is
// what the queue has.
type RabbitQueue struct {
	// Type is classic, quorum or stream; empty is classic.
	Type string `json:"type,omitempty"`
	// Classic and quorum.
	MaxLength          int64  `json:"maxLength,omitempty"`
	MessageTTLMS       int64  `json:"messageTtlMs,omitempty"`
	DeadLetterExchange string `json:"deadLetterExchange,omitempty"`
	// Quorum: deliveries before a message is dead-lettered or dropped.
	DeliveryLimit int `json:"deliveryLimit,omitempty"`
	// Stream (and max bytes for every type).
	MaxAgeS        int64 `json:"maxAgeS,omitempty"`
	MaxLengthBytes int64 `json:"maxLengthBytes,omitempty"`
	SegmentBytes   int64 `json:"segmentBytes,omitempty"`
	// Bind, when set, binds the new queue to this exchange with the
	// routing key, in the same step.
	BindExchange   string `json:"bindExchange,omitempty"`
	BindRoutingKey string `json:"bindRoutingKey,omitempty"`
}

// Arguments are the queue's x- arguments, and whether it must be durable.
func (q RabbitQueue) Arguments() (map[string]any, bool, error) {
	args := map[string]any{}
	if q.MaxLength < 0 || q.MessageTTLMS < 0 || q.DeliveryLimit < 0 || q.MaxAgeS < 0 || q.MaxLengthBytes < 0 || q.SegmentBytes < 0 {
		return nil, false, errors.New("a limit cannot be negative")
	}
	if len(q.DeadLetterExchange) > 255 || len(q.BindExchange) > 255 || len(q.BindRoutingKey) > 255 {
		return nil, false, errors.New("an exchange or routing key is at most 255 characters")
	}
	durable := false
	switch q.Type {
	case "", "classic":
		if q.DeliveryLimit > 0 || q.MaxAgeS > 0 || q.SegmentBytes > 0 {
			return nil, false, errors.New("a classic queue takes most messages, message TTL and a dead-letter exchange")
		}
	case "quorum":
		durable = true
		args["x-queue-type"] = "quorum"
		if q.MaxAgeS > 0 || q.SegmentBytes > 0 {
			return nil, false, errors.New("keep for and segment size are for streams")
		}
		if q.DeliveryLimit > 0 {
			args["x-delivery-limit"] = q.DeliveryLimit
		}
	case "stream":
		durable = true
		args["x-queue-type"] = "stream"
		if q.MaxLength > 0 || q.MessageTTLMS > 0 || q.DeadLetterExchange != "" || q.DeliveryLimit > 0 {
			return nil, false, errors.New("a stream keeps messages by age and size; most messages, TTL, dead-lettering and delivery limits are for queues")
		}
		if q.MaxAgeS > 0 {
			args["x-max-age"] = strconv.FormatInt(q.MaxAgeS, 10) + "s"
		}
		if q.SegmentBytes > 0 {
			args["x-stream-max-segment-size-bytes"] = q.SegmentBytes
		}
	default:
		return nil, false, errors.New("a queue is classic, quorum or stream")
	}
	if q.MaxLength > 0 {
		args["x-max-length"] = q.MaxLength
	}
	if q.MessageTTLMS > 0 {
		args["x-message-ttl"] = q.MessageTTLMS
	}
	if q.DeadLetterExchange != "" {
		args["x-dead-letter-exchange"] = q.DeadLetterExchange
	}
	if q.MaxLengthBytes > 0 {
		args["x-max-length-bytes"] = q.MaxLengthBytes
	}
	return args, durable, nil
}
