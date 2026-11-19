package control

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

// Everything one person may reach in this organization, in one list.
//
// The "what can they do" window, and the step after adding someone from
// another organization, read this registry rather than knowing each kind of
// thing. A feature that gives people or teams access to something new adds
// one accessKind here, and TestEveryGrantTableHasAnAccessKind fails until it
// does: a grant table (a user_email or team_id column) that no kind reads is
// access the console cannot show.
//
// Nothing here is about organizations. A person shared in from elsewhere has
// a users row here like anyone else, every grant is a row in this
// organization's tables, and what they see after switching is decided by the
// same checks as for everyone. That is why a new feature works for them
// without doing anything about sharing.

// AccessItem is one thing a person may reach.
type AccessItem struct {
	ID string `json:"id"`
	// Name is the thing; Where what it is on, if anything.
	Name  string `json:"name"`
	Where string `json:"where,omitempty"`
	// What they may do, in words.
	What string `json:"what"`
	// Own is a grant of their own; Teams the teams that give them something.
	Own   bool     `json:"own"`
	Teams []string `json:"teams"`
	// Until is when a time-limited grant ends.
	Until *time.Time `json:"until,omitempty"`
}

// AccessSection is one kind of thing, and the person's items of it.
type AccessSection struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
	// Give is the console page where this kind is given, for the step after
	// adding someone.
	Give  string       `json:"give"`
	Empty string       `json:"empty"`
	Items []AccessItem `json:"items"`
}

type accessKind struct {
	kind, label, give, empty string
	// tables are the grant tables this kind reads; see the test.
	tables []string
	list   func(ctx context.Context, s *Store, email string) ([]AccessItem, error)
}

// accessKinds is every kind of thing a person can be given, in the order the
// console shows them.
var accessKinds = []accessKind{
	{
		kind: "servers", label: "Servers", give: "/assets",
		empty:  "No servers yet. Give one to a team of theirs, or to them, on the server's page.",
		tables: []string{"asset_assignments", "asset_team_assignments"},
		list:   serverAccess,
	},
	{
		kind: "databases", label: "Databases", give: "/databases",
		empty:  "No databases yet. Give one to a team of theirs, or to them, on the database's page.",
		tables: []string{"database_assignments", "database_access_rules"},
		list: func(ctx context.Context, s *Store, e string) ([]AccessItem, error) {
			return datastoreAccess(ctx, s, e, false)
		},
	},
	{
		kind: "brokers", label: "Brokers", give: "/brokers",
		empty:  "No brokers yet. Give one to a team of theirs, or to them, on the broker's page.",
		tables: []string{"database_assignments", "database_access_rules"},
		list: func(ctx context.Context, s *Store, e string) ([]AccessItem, error) {
			return datastoreAccess(ctx, s, e, true)
		},
	},
	{
		kind: "cloud", label: "Cloud", give: "/cloud",
		empty:  "No cloud access open now. They ask for it from Cloud, for a while, and someone approves it.",
		tables: []string{"cloud_access"},
		list:   cloudAccess,
	},
}

// notGrantTables name people or teams without giving anyone access to
// anything, and say why, so the test can tell them from a grant table that
// was forgotten.
var notGrantTables = map[string]string{
	"team_members": "who is in a team; the kinds above read it to follow team grants",
	"sessions":     "what someone did, not what they may do",
	"users":        "the people themselves",
}

// PersonAccess is every section for email, empty ones included, so the
// console can offer to give each kind.
func (s *Store) PersonAccess(ctx context.Context, email string) ([]AccessSection, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	out := make([]AccessSection, 0, len(accessKinds))
	for _, k := range accessKinds {
		items, err := k.list(ctx, s, email)
		if err != nil {
			return nil, err
		}
		if items == nil {
			items = []AccessItem{}
		}
		out = append(out, AccessSection{Kind: k.kind, Label: k.label, Give: k.give, Empty: k.empty, Items: items})
	}
	return out, nil
}

func serverAccess(ctx context.Context, s *Store, email string) ([]AccessItem, error) {
	list, err := s.PersonServers(ctx, email)
	if err != nil {
		return nil, err
	}
	out := make([]AccessItem, 0, len(list))
	for _, p := range list {
		var parts []string
		if p.PersonalAccount {
			own := "their own account"
			if p.Sudo {
				own += " with sudo"
			}
			parts = append(parts, own)
		}
		if len(p.Principals) > 0 {
			parts = append(parts, "as "+strings.Join(p.Principals, ", "))
		}
		if p.PersonalAccount && p.DirectSSH == DirectSSHAllow {
			parts = append(parts, "direct SSH")
		}
		if p.PersonalAccount && p.DirectSSH == DirectSSHApprove {
			parts = append(parts, "direct SSH after approval")
		}
		what := strings.Join(parts, " · ")
		if what == "" {
			what = "no access of their own here"
		}
		teams := p.Teams
		if p.OwnSetting {
			teams = []string{}
		}
		out = append(out, AccessItem{ID: p.AssetID, Name: p.Hostname, What: what, Own: p.OwnSetting, Teams: teams})
	}
	return out, nil
}

var levelWords = map[string]string{
	tunnel.LevelRead: "Read", tunnel.LevelWrite: "Write", tunnel.LevelOwner: "Owner", tunnel.LevelDBA: "DBA",
}
var brokerLevelWords = map[string]string{
	tunnel.LevelRead: "Read", tunnel.LevelWrite: "Write", tunnel.LevelOwner: "Manage", tunnel.LevelDBA: "Admin",
}

var engineWords = map[string]string{
	tunnel.EnginePostgres: "Postgres", tunnel.EngineMySQL: "MySQL", tunnel.EngineMongoDB: "MongoDB",
	tunnel.EngineRedis: "Redis", tunnel.EngineRabbitMQ: "RabbitMQ", tunnel.EngineKafka: "Kafka",
	tunnel.EngineMSSQL: "SQL Server",
}

func isBrokerEngine(e string) bool { return e == tunnel.EngineRabbitMQ || e == tunnel.EngineKafka }

func datastoreAccess(ctx context.Context, s *Store, email string, brokers bool) ([]AccessItem, error) {
	list, err := s.PersonDatabases(ctx, email)
	if err != nil {
		return nil, err
	}
	out := []AccessItem{}
	for _, d := range list {
		if isBrokerEngine(d.Engine) != brokers {
			continue
		}
		words := levelWords
		if brokers {
			words = brokerLevelWords
		}
		now, ask := words[d.Level], ""
		if d.Approvable != "" {
			ask = words[d.Approvable] + " with an approved request"
		}
		what := now
		switch {
		case now != "" && ask != "":
			what = now + ", or " + ask
		case now == "":
			what = ask
		}
		engine := engineWords[d.Engine]
		if engine == "" {
			engine = engineWords[tunnel.EnginePostgres]
		}
		out = append(out, AccessItem{ID: d.ID, Name: d.Name, Where: d.Hostname + " · " + engine,
			What: what, Own: d.Own, Teams: d.Teams})
	}
	return out, nil
}

// cloudAccess is the cloud grants open for email now. Cloud access is asked
// for and approved for a while, never given standing, so an empty list is the
// normal answer.
func cloudAccess(ctx context.Context, s *Store, email string) ([]AccessItem, error) {
	rows, err := s.db(ctx).Query(ctx, `
		SELECT g.id::text, a.name, g.level, g.environment, g.ends_at
		  FROM cloud_access g JOIN cloud_accounts a ON a.id = g.account_id
		 WHERE lower(g.requested_by) = $1 AND g.state = 'active' AND g.ends_at > now()
		 ORDER BY a.name, g.ends_at`, email)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AccessItem{}
	for rows.Next() {
		var it AccessItem
		var level, env string
		var until time.Time
		if err := rows.Scan(&it.ID, &it.Name, &level, &env, &until); err != nil {
			return nil, err
		}
		verb := "Read"
		if level == "operate" {
			verb = "Operate"
		}
		it.Where = env
		it.What = verb + ", approved for a while"
		it.Own = true
		it.Teams = []string{}
		it.Until = &until
		out = append(out, it)
	}
	return out, rows.Err()
}

// GET /api/v1/users/{email}/access: every section, for the window that
// answers "what can they do here?".
func (a *API) getPersonAccess(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden, "seeing what someone can do requires the admin, owner or auditor role")
		return
	}
	list, err := a.store.PersonAccess(r.Context(), normaliseEmail(r.PathValue("email")))
	if err != nil {
		a.fail(w, "person access", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}
