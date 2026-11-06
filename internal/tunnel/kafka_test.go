package tunnel

import (
	"strings"
	"testing"
)

func TestKafkaNames(t *testing.T) {
	for _, ok := range []string{"*", "orders", "orders.", "a-b_c.d", strings.Repeat("x", 249)} {
		if !ValidKafkaScope(ok) {
			t.Errorf("%q should be a scope", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", "orders/*", "a b", strings.Repeat("x", 250), "ü"} {
		if ValidKafkaScope(bad) {
			t.Errorf("%q should not be a scope", bad)
		}
	}
	if ValidKafkaTopic("*") {
		t.Error("* is not a topic")
	}
	if !InKafkaScope("*", "anything") || !InKafkaScope("orders.", "orders.eu") || InKafkaScope("orders.", "payments") {
		t.Error("scope")
	}
}

func TestKafkaLevels(t *testing.T) {
	has := func(ops []string, op string) bool {
		for _, o := range ops {
			if o == op {
				return true
			}
		}
		return false
	}
	if has(KafkaTopicOps(LevelRead), "WRITE") || !has(KafkaTopicOps(LevelWrite), "WRITE") || has(KafkaTopicOps(LevelWrite), "DELETE") {
		t.Error("topic operations by level")
	}
	if has(KafkaStoreOps(LevelRead), "publish") || !has(KafkaStoreOps(LevelWrite), "publish") ||
		has(KafkaStoreOps(LevelWrite), "delete_topic") || !has(KafkaStoreOps(LevelDBA), "delete_topic") {
		t.Error("store operations by level")
	}
	if KafkaTopicOps("root") != nil || KafkaStoreOps("root") != nil {
		t.Error("an unknown level holds operations")
	}
	if !ValidKafkaAdminFile("/etc/argus/kafka.conf") || ValidKafkaAdminFile("/etc/../passwd.conf") || ValidKafkaAdminFile("kafka.conf") {
		t.Error("admin file")
	}
}
