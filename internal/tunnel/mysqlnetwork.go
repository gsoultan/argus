package tunnel

import "strings"

/*
Which machines can connect to a MySQL or MariaDB: bind_address (which of the
server's addresses mysqld listens on; read at start, so a change takes a
restart) and the server's firewall. Who may sign in from where is each
user's host part ('app'@'10.0.0.%'), which stays the person's to set: Argus
says which users can sign in from another machine, and refuses to open to
anywhere while one of them needs no password.

MySQL keeps the setting with SET PERSIST_ONLY; MariaDB, which has no
PERSIST, in an option file of Argus's own, read after the server's.
*/

// MySQLBindFor is bind_address for a change: the loopback, every address,
// or the loopback and the addresses named.
func MySQLBindFor(op PGAdminOp) string {
	switch op.Reach {
	case ReachAny:
		return "*"
	case ReachSelected, ReachNetwork:
		return strings.Join(append([]string{"127.0.0.1"}, op.Addresses...), ",")
	}
	return "127.0.0.1"
}

// MySQLNetworkText is a network.set on MySQL as the lines an approval is
// given for.
func MySQLNetworkText(op PGAdminOp) string {
	lines := []string{"SET bind_address = '" + MySQLBindFor(op) + "' (from the next start)", FirewallText(op.Reach, op.Sources)}
	if op.Restart {
		lines = append(lines, "RESTART MYSQL")
	}
	return strings.Join(lines, "\n")
}

// NetworkText is a network.set as the lines an approval is given for, on
// the engine it is for.
func NetworkText(engine string, op PGAdminOp) string {
	if engine == EngineMySQL {
		return MySQLNetworkText(op)
	}
	return PGNetworkText(op)
}
