package control

/*
Monitoring's checks: one engine for every kind of thing Argus checks from
outside.

What every kind shares is a column of the checks table (migration 103):
the name, how often and how long to wait, the servers that also look, the
Telegram choice, where it stands, and the lease that keeps one round on one
replica. Rounds and outages are check_rounds and check_outages, so uptime,
outages and the 35 days of history are the same for every kind. What a kind
adds is its settings (JSON) and its secrets (sealed with a key derived from
signing_secret, never returned).

A kind is registered in checkKinds and in the checks_kind_check constraint,
and a test keeps the two the same. Website checks (websites.go) are the
first kind.
*/

// checkKind is one kind of check.
type checkKind struct {
	// Name is what the kind column holds.
	Name string
	// Label is what the console calls it.
	Label string
}

// checkKinds is every kind of check, in the order the console offers them.
var checkKinds = []checkKind{
	{Name: "website", Label: "Website"},
}
