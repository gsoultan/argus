package tunnel

import "testing"

func TestRemotePGHost(t *testing.T) {
	for host, want := range map[string]bool{
		"10.0.0.5": true, "db-02.internal": true, "fd00::5": true,
		"": false, "127.0.0.1": false, "/var/run/postgresql": false, "localhost": false,
		"db internal": false, "-oProxyCommand": false, "db/x": false, "a\x00b": false,
	} {
		if got := RemotePGHost(host); got != want {
			t.Errorf("RemotePGHost(%q) = %v, want %v", host, got, want)
		}
	}
}
