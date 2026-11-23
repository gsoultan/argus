package tunnel

import "strings"

/*
Which machines can connect to a RabbitMQ: the addresses its AMQP listener
binds (listeners.tcp in rabbitmq.conf, read when the node starts, so a
change takes a restart) and the server's firewall. The management API's
port is not changed: Argus reaches it on the loopback.

Argus writes its own listeners.tcp lines, in place of every listeners.tcp
line rabbitmq.conf had, and refuses to open past the loopback while the
guest user still signs in with its default password from anywhere.

The requests are store requests on the rabbitmq session (Op network, and
network_set with Reach, Addresses, Sources, Confirm and Restart), at DBA.
*/

const (
	RabbitOpNetwork    = "network"
	RabbitOpNetworkSet = "network_set"
)

// RabbitBindFor is the addresses the AMQP listener binds for a change: the
// loopback, every address ("*"), or the loopback and the addresses named.
func RabbitBindFor(reach string, addresses []string) string {
	return MongoBindFor(reach, addresses)
}

// RabbitNetworkText is a network_set on RabbitMQ as the lines an approval
// is given for.
func RabbitNetworkText(req StoreRequest) string {
	lines := []string{"SET listeners.tcp = " + RabbitBindFor(req.Reach, req.Addresses) + " (from the next start)",
		FirewallText(req.Reach, req.Sources)}
	if req.Restart {
		lines = append(lines, "RESTART RABBITMQ")
	}
	return strings.Join(lines, "\n")
}

// StoreNetworkText is a store engine's network_set as the lines an
// approval is given for, or "" for an engine that has none.
func StoreNetworkText(engine string, req StoreRequest) string {
	switch engine {
	case EngineMongoDB:
		return MongoNetworkText(req)
	case EngineRabbitMQ:
		return RabbitNetworkText(req)
	case EngineKafka:
		return KafkaNetworkText(req)
	case EngineNATS:
		return NATSNetworkText(req)
	}
	return ""
}

// KafkaNetworkText is a network_set on Kafka as the lines an approval is
// given for: the firewall alone, since Argus leaves Kafka's listeners as
// the server's server.properties says.
func KafkaNetworkText(req StoreRequest) string {
	return FirewallText(req.Reach, req.Sources)
}
