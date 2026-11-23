package tunnel

import "testing"

func TestRedisStreamGroupEdits(t *testing.T) {
	for _, c := range []struct {
		args []string
		ok   bool
		key  string
	}{
		{[]string{"XGROUP", "CREATE", "orders", "g", "$", "MKSTREAM"}, true, "orders"},
		{[]string{"xgroup", "setid", "orders", "g", "0"}, true, "orders"},
		{[]string{"XGROUP", "DESTROY", "orders", "g"}, true, "orders"},
		{[]string{"XGROUP", "DELCONSUMER", "orders", "g", "c"}, true, "orders"},
		{[]string{"XGROUP", "CREATECONSUMER", "orders", "g", "c"}, true, "orders"},
		{[]string{"XACK", "orders", "g", "1-0"}, true, "orders"},
		{[]string{"XCLAIM", "orders", "g", "c", "60000", "1-0"}, true, "orders"},
		{[]string{"XGROUP", "HELP", "orders", "g"}, false, ""},
		{[]string{"XGROUP", "CREATE", "orders", "g"}, false, ""},
		{[]string{"XACK", "orders", "g"}, false, ""},
		{[]string{"XCLAIM", "orders", "g", "c", "0"}, false, ""},
	} {
		if RedisEditable(c.args) != c.ok {
			t.Errorf("%v editable: %v", c.args, !c.ok)
		}
		if c.ok && RedisEditKey(c.args) != c.key {
			t.Errorf("%v key: %s", c.args, RedisEditKey(c.args))
		}
	}
	if RedisOpLevel(RedisOpStreamGroups) != LevelRead {
		t.Error("stream groups are Read")
	}
}
