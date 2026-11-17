package tunnel

import "strings"

/*
Which machines can connect to a MongoDB: net.bindIp (which of the server's
addresses mongod listens on; read at start, so a change takes a restart) and
the server's firewall. Argus only adds a MongoDB whose access control is on,
so every machine that reaches it still has to sign in as a user.

mongod keeps the setting in its configuration file (mongod.conf). Argus
changes net.bindIp or net.bindIpAll there and nothing else, and refuses when
mongod was started with --bind_ip on its command line, which the file does
not override.

The requests are store requests on the mongodb session (Op network, and
network_set with Reach, Addresses, Sources, Confirm and Restart), at DBA.
*/

const (
	MongoOpNetwork    = "network"
	MongoOpNetworkSet = "network_set"
)

// MongoBindFor is net.bindIp for a change: the loopback, every address
// ("*", written as bindIpAll), or the loopback and the addresses named.
func MongoBindFor(reach string, addresses []string) string {
	switch reach {
	case ReachAny:
		return "*"
	case ReachSelected, ReachNetwork:
		return strings.Join(append([]string{"127.0.0.1"}, addresses...), ",")
	}
	return "127.0.0.1"
}

// MongoNetOp is a network_set as the shared network rules take it.
func MongoNetOp(req StoreRequest) PGAdminOp {
	return PGAdminOp{Kind: AdminNetworkSet, Reach: req.Reach, Addresses: req.Addresses, Sources: req.Sources,
		Confirm: req.Confirm, Restart: req.Restart}
}

// MongoNetworkText is a network_set on MongoDB as the lines an approval is
// given for.
func MongoNetworkText(req StoreRequest) string {
	bind := MongoBindFor(req.Reach, req.Addresses)
	set := "SET net.bindIp = '" + bind + "' (from the next start)"
	if bind == "*" {
		set = "SET net.bindIpAll = true (from the next start)"
	}
	lines := []string{set, FirewallText(req.Reach, req.Sources)}
	if req.Restart {
		lines = append(lines, "RESTART MONGOD")
	}
	return strings.Join(lines, "\n")
}
