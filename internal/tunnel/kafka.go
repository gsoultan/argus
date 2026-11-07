package tunnel

import (
	"strings"
)

/*
Kafka, as a broker beside RabbitMQ, through the same store frames: a
StoreRequest toward the agent, a StoreAnswer back, and a PGRecord line before
anything that reads messages or changes the cluster.

One entry is the topics of a Kafka cluster that one scope names: "*" for every
topic, or a prefix such as "orders." for the topics that start with it. Each
person gets a SCRAM user, StoreUser(role, level), with ACLs on those topics for
their level only (KafkaTopicOps) and a one-time password; the last session to
close deletes the user and its ACLs. Argus never touches a user whose name does
not start with StoreUserPrefix.

The agent signs in as Kafka's administrator with the credentials in a file on
the server (KafkaAdminFile). A cluster without an authorizer is refused,
because a person's level would mean nothing on it.
*/

// Kafka engine and service.
const (
	EngineKafka  = "kafka"
	KafkaService = "kafka"
)

// KafkaAdminFile is where the agent looks for the admin credentials when the
// entry names no file.
const KafkaAdminFile = "/etc/argus/kafka.conf"

// KafkaAllTopics is the scope that covers every topic.
const KafkaAllTopics = "*"

// ValidKafkaScope reports whether s can be an entry's scope: "*", or a topic
// name or prefix of the characters Kafka allows in one (letters, digits, .
// _ and -), at most 249 of them.
func ValidKafkaScope(s string) bool {
	if s == KafkaAllTopics {
		return true
	}
	return ValidKafkaTopic(s)
}

// ValidKafkaTopic reports whether t is a name Kafka allows for a topic.
func ValidKafkaTopic(t string) bool {
	if t == "" || len(t) > 249 || t == "." || t == ".." {
		return false
	}
	for _, c := range t {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// InKafkaScope reports whether topic is one an entry's scope covers.
func InKafkaScope(scope, topic string) bool {
	return scope == KafkaAllTopics || strings.HasPrefix(topic, scope)
}

// KafkaTopicOps are the ACL operations a level holds on the scope's topics,
// least to most. Each includes the one below it. Every level may also
// describe consumer groups, to see who reads a topic and how far behind.
func KafkaTopicOps(level string) []string {
	read := []string{"READ", "DESCRIBE", "DESCRIBE_CONFIGS"}
	switch level {
	case LevelRead:
		return read
	case LevelWrite:
		return append(read, "WRITE")
	case LevelOwner, LevelDBA:
		return append(read, "WRITE", "CREATE", "DELETE", "ALTER", "ALTER_CONFIGS")
	}
	return nil
}

// KafkaStoreOps are the operations the console may ask for at a level.
func KafkaStoreOps(level string) []string {
	ops := []string{"overview", "topics", "topic", "groups", "peek"}
	switch level {
	case LevelWrite:
		ops = append(ops, "publish")
	case LevelOwner, LevelDBA:
		ops = append(ops, "publish", "create_topic", "delete_topic")
	case LevelRead:
	default:
		return nil
	}
	return ops
}

// ValidKafkaAdminFile reports whether p may be read for the admin sign-in:
// an absolute path to a .conf file with no way out of where it says.
func ValidKafkaAdminFile(p string) bool {
	return strings.HasPrefix(p, "/") && !strings.Contains(p, "..") && strings.HasSuffix(p, ".conf") &&
		!strings.ContainsAny(p, "\x00\n")
}
