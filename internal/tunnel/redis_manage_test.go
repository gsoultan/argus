package tunnel

import "testing"

func TestRedisApprovalText(t *testing.T) {
	cases := []struct {
		req  StoreRequest
		text string
		wait bool
	}{
		{StoreRequest{Op: RedisOpBulk, Action: "delete", Max: 100, Filter: RedisFilter{Pattern: "s:*"}}, "", false},
		{StoreRequest{Op: RedisOpBulk, Action: "expire", Seconds: 60, Max: 101, Undo: true,
			Filter: RedisFilter{Pattern: "session:2024*", IdleDays: 30, Type: "hash"}},
			"BULK EXPIRE 60 MATCH session:2024* TYPE hash IDLEDAYS 30 KEEPCOPY", true},
		{StoreRequest{Op: RedisOpConfigSet, Rewrite: true, Config: []RedisConfigItem{{"maxmemory", "6gb"}}},
			"CONFIG SET maxmemory 6gb\nCONFIG REWRITE", true},
		{StoreRequest{Op: RedisOpACLSet, User: "app", State: "on", AddHashes: []string{"AB"}, Rules: []string{"~app:*", "+@read"}},
			"ACL SETUSER app on #ab resetkeys resetchannels -@all ~app:* +@read", true},
		{StoreRequest{Op: RedisOpACLDel, User: "app"}, "ACL DELUSER app", true},
		{StoreRequest{Op: RedisOpEdit}, "", false},
	}
	for _, c := range cases {
		a, wait := RedisApproval(c.req)
		if wait != c.wait || a.Text != c.text {
			t.Errorf("%s: got %q %v, want %q %v", c.req.Op, a.Text, wait, c.text, c.wait)
		}
	}
}

func TestValidRedisRules(t *testing.T) {
	ok := [][]string{{"~*", "+@read"}, {"%R~*", "~cart:*", "&notify:*", "-@all", "+get", "+client|id"}, {"+@all", "-@admin", "~*"}}
	for _, r := range ok {
		if why := ValidRedisRules(r, LevelWrite); why != "" {
			t.Errorf("%v: %s", r, why)
		}
	}
	bad := [][]string{{">pw"}, {"nopass"}, {"reset"}, {"~*", "+@admin"}, {"+@all"}, {"+config"}, {"(~x +get)"}, {"~"}, {"#abc"}}
	for _, r := range bad {
		if ValidRedisRules(r, LevelOwner) == "" {
			t.Errorf("%v passed below DBA", r)
		}
	}
	if why := ValidRedisRules([]string{"+@all"}, LevelDBA); why != "" {
		t.Errorf("+@all at DBA: %s", why)
	}
	if why := ValidRedisRules([]string{"+@admin"}, ""); why != "" {
		t.Errorf("a server user may have +@admin: %s", why)
	}
}

func TestRedisSettings(t *testing.T) {
	if !RedisSameSetting("maxmemory", "4gb", "4294967296") || RedisSameSetting("maxmemory", "4gb", "4mb") {
		t.Error("sizes")
	}
	if !RedisSameSetting("appendonly", "YES", "yes") || !RedisSameSetting("save", "3600 1  300 100", "3600 1 300 100") {
		t.Error("words")
	}
	f := RedisConfFile("# c\nsave 3600 1\nsave 300 100\nmaxmemory 4gb\nrequirepass \"a b\"\n")
	if f["save"] != "3600 1 300 100" || f["maxmemory"] != "4gb" || f["requirepass"] != "a b" {
		t.Errorf("file: %v", f)
	}
	for _, n := range []string{"dir", "dbfilename", "requirepass", "tls-cert-file", "enable-debug-command"} {
		if RedisConfigSettable(n) {
			t.Errorf("%s is settable", n)
		}
	}
	if ValidRedisConfigItems([]RedisConfigItem{{"maxmemory", "1gb\nslaveof x"}}) == "" {
		t.Error("a two-line value passed")
	}
	if RedisHashRef("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef") != "01234567…89abcdef" ||
		!ValidRedisHashRef("01234567…89abcdef") || ValidRedisHashRef("0123…cdef") {
		t.Error("hash refs")
	}
}
