package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Create new: an admin makes a database, vhost or topic on a server, and it is
added to Argus in the same step, so its label, the server's rules and its
backups apply at once. The design is plans/create-databases/design.md.

	GET  /api/v1/assets/{id}/databases/create-options   what the dialog may offer, from the agent
	POST /api/v1/assets/{id}/databases/create           make it (dryRun: only say what would run)
	POST /api/v1/databases/{id}/owner                   Postgres: change the database's owner

Admins only, like adding one. The statements are tunnel.CreatePlan's, built
from fields both sides check; the agent never runs over a name that is
there. Every create and owner change is in Activity with what ran, and so is
every failure, with how far it got.
*/

const createWait = 75 * time.Second

// CreateRequest is the dialog's form.
type CreateRequest struct {
	Engine     string `json:"engine"`
	Host       string `json:"host,omitempty"`
	Port       int    `json:"port,omitempty"`
	PoolerPort int    `json:"poolerPort,omitempty"`
	ConfigPath string `json:"configPath,omitempty"`
	Name       string `json:"name"`
	Label      string `json:"label,omitempty"`

	Postgres *tunnel.PGCreate     `json:"postgres,omitempty"`
	MySQL    *tunnel.MySQLCreate  `json:"mysql,omitempty"`
	Mongo    *tunnel.MongoCreate  `json:"mongo,omitempty"`
	Rabbit   *tunnel.RabbitCreate `json:"rabbit,omitempty"`
	Kafka    *tunnel.KafkaCreate  `json:"kafka,omitempty"`

	// AddEntry adds a Kafka topic as its own entry even when an existing
	// entry's scope covers it. Every other engine is always added.
	AddEntry bool `json:"addEntry,omitempty"`
	// DryRun answers with the statements and changes nothing.
	DryRun bool `json:"dryRun,omitempty"`
}

// CreateAnswer is what the dialog shows after Create.
type CreateAnswer struct {
	// State is ok, partial (made, but a later step failed), exists,
	// offline, unsupported or failed.
	State      string              `json:"state"`
	Error      string              `json:"error,omitempty"`
	Statements []string            `json:"statements"`
	Steps      []tunnel.CreateStep `json:"steps"`
	Database   *Database           `json:"database,omitempty"`
	// CoveredBy is the Kafka entry whose scope already covers the topic.
	CoveredBy *Database `json:"coveredBy,omitempty"`
}

// CreateOptionsAnswer is the dialog's choices.
type CreateOptionsAnswer struct {
	State   string                `json:"state"`
	Error   string                `json:"error,omitempty"`
	Options *tunnel.CreateOptions `json:"options,omitempty"`
}

// probeCreate fills the entry's defaults for where the engine listens, the
// same way adding one does, and builds the grant.
func probeCreate(in CreateRequest, op string) (tunnel.GrantCreate, Database, error) {
	name := in.Name
	if op == tunnel.CreateOpOptions || name == "" {
		name = discoverPlaceholder(in.Engine)
	}
	d := Database{Engine: in.Engine, Host: strings.TrimSpace(in.Host), Port: in.Port, ConfigPath: strings.TrimSpace(in.ConfigPath),
		PoolerPort: in.PoolerPort, Name: name, Label: in.Label}
	if in.Engine == tunnel.EngineRedis {
		return tunnel.GrantCreate{}, d, ErrInvalidDatabase{tunnel.RedisNothingToCreate}
	}
	if in.Engine == tunnel.EngineKafka {
		// The entry's name is a scope; the topic is checked below.
		d.Name = tunnel.KafkaAllTopics
	}
	if err := d.validate(); err != nil {
		return tunnel.GrantCreate{}, d, err
	}
	if in.Engine == tunnel.EngineKafka {
		d.Name = name
	}
	c := tunnel.GrantCreate{Op: op, Engine: d.Engine, Host: d.Host, Port: d.Port, AdminUser: d.AdminUser,
		ConfigPath: d.ConfigPath, Name: name,
		Postgres: in.Postgres, MySQL: in.MySQL, Mongo: in.Mongo, Rabbit: in.Rabbit, Kafka: in.Kafka}
	if err := c.Validate(); err != nil {
		return c, d, ErrInvalidDatabase{err.Error()}
	}
	return c, d, nil
}

// GET /api/v1/assets/{id}/databases/create-options
func (a *API) getCreateOptions(w http.ResponseWriter, r *http.Request, actor string) {
	asset, err := a.store.AssetByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such server")
		return
	}
	q := r.URL.Query()
	in := CreateRequest{Engine: q.Get("engine"), Host: q.Get("host"), ConfigPath: q.Get("configPath")}
	in.Port, _ = strconv.Atoi(q.Get("port"))
	c, _, err := probeCreate(in, tunnel.CreateOpOptions)
	if err != nil {
		a.dbErr(w, "create options", err)
		return
	}
	res, state, why := a.askCreate(r.Context(), actor, asset.Hostname, c)
	ans := CreateOptionsAnswer{State: state, Error: why}
	if state == "ok" {
		if !res.OK {
			ans.State, ans.Error = "failed", res.Error
		} else {
			ans.Options = res.Options
		}
	}
	writeJSON(w, http.StatusOK, ans)
}

// POST /api/v1/assets/{id}/databases/create
func (a *API) postCreateDatabase(w http.ResponseWriter, r *http.Request, actor string) {
	ctx := r.Context()
	asset, err := a.store.AssetByID(ctx, r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such server")
		return
	}
	var in CreateRequest
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Name = strings.TrimSpace(in.Name); in.Name == "" {
		writeErr(w, http.StatusBadRequest, "the name is required")
		return
	}
	c, entry, err := probeCreate(in, tunnel.CreateOpCreate)
	if err != nil {
		a.dbErr(w, "create database", err)
		return
	}
	entry.AssetID, entry.Hostname = asset.ID, asset.Hostname
	ans := CreateAnswer{Statements: tunnel.CreatePlan(c), Steps: []tunnel.CreateStep{}}

	// A Kafka topic an existing entry's scope covers needs no entry of its
	// own, unless asked for.
	addEntry := true
	if c.Engine == tunnel.EngineKafka {
		if existing, err := a.store.Databases(ctx, asset.ID); err == nil {
			for _, e := range existing {
				if e.Engine == tunnel.EngineKafka && e.Port == c.Port && tunnel.InKafkaScope(e.Name, c.Name) {
					cov := e
					ans.CoveredBy = &cov
					addEntry = in.AddEntry
					break
				}
			}
		}
	}
	if in.DryRun {
		ans.State = "ok"
		writeJSON(w, http.StatusOK, ans)
		return
	}

	what := engineCreateNoun(c.Engine) + " " + c.Name + " on " + asset.Hostname
	res, state, why := a.askCreate(ctx, actor, asset.Hostname, c)
	if state != "ok" {
		ans.State, ans.Error = state, why
		a.auditDB(ctx, "db.create.failed", "warning", actor, asset.Hostname, "Could not create "+what+": "+why+".")
		writeJSON(w, http.StatusOK, ans)
		return
	}
	ans.Steps = append(ans.Steps, res.Steps...)
	if len(res.Statements) > 0 {
		ans.Statements = res.Statements
	}
	made := res.OK || (res.Error != "" && len(res.Statements) > 0)
	switch {
	case res.Exists:
		ans.State, ans.Error = "exists", res.Error
		writeJSON(w, http.StatusOK, ans)
		return
	case !made:
		ans.State, ans.Error = "failed", res.Error
		a.auditDB(ctx, "db.create.failed", "warning", actor, asset.Hostname, "Could not create "+what+": "+res.Error+".")
		writeJSON(w, http.StatusOK, ans)
		return
	}
	ans.State = "ok"
	if !res.OK || res.Error != "" {
		ans.State, ans.Error = "partial", res.Error
	}
	a.auditDB(ctx, "db.created", "notice", actor, asset.Hostname,
		"Created "+what+": "+strings.Join(res.Statements, "; ")+".")

	if !addEntry {
		ans.Steps = append(ans.Steps, tunnel.CreateStep{OK: true,
			Text: "Covered by the entry " + ans.CoveredBy.Name + ", so nothing new is added to Argus"})
		writeJSON(w, http.StatusOK, ans)
		return
	}
	db, err := a.store.CreateDatabase(ctx, entry, actor)
	if err != nil {
		var invalid ErrInvalidDatabase
		msg := err.Error()
		if errors.As(err, &invalid) {
			msg = invalid.Reason
		} else {
			a.log.Error("create: made on the server but not added to Argus", "name", c.Name, "error", err)
		}
		ans.State, ans.Error = "partial", "made on "+asset.Hostname+", but not added to Argus: "+msg+". Add it from Already on "+asset.Hostname+"."
		ans.Steps = append(ans.Steps, tunnel.CreateStep{Text: "Add it to Argus", Error: msg})
		writeJSON(w, http.StatusOK, ans)
		return
	}
	a.auditDB(ctx, "db.added", "notice", actor, db.Hostname,
		"Added "+engineName(db.Engine)+" "+db.Name+" ("+db.Label+") on "+db.Hostname+" at "+db.Host+":"+itoa(db.Port)+".")
	ans.Steps = append(ans.Steps, tunnel.CreateStep{Text: "Added it to Argus as " + db.Label, OK: true})
	a.applyRulesTo(ctx, db, actor)
	ans.Database = &db
	writeJSON(w, http.StatusOK, ans)
}

// POST /api/v1/databases/{id}/owner
func (a *API) postDatabaseOwner(w http.ResponseWriter, r *http.Request, actor string) {
	ctx := r.Context()
	db, err := a.store.Database(ctx, r.PathValue("id"))
	if err != nil {
		a.dbErr(w, "database", err)
		return
	}
	var in struct {
		Owner string `json:"owner"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if db.Engine != tunnel.EnginePostgres && db.Engine != "" {
		writeErr(w, http.StatusBadRequest, "only a Postgres database has an owner to change")
		return
	}
	c := tunnel.GrantCreate{Op: tunnel.CreateOpSetOwner, Engine: tunnel.EnginePostgres, Host: db.Host, Port: db.Port,
		AdminUser: db.AdminUser, Name: db.Name, Postgres: &tunnel.PGCreate{Owner: strings.TrimSpace(in.Owner)}}
	if err := c.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res, state, why := a.askCreate(ctx, actor, db.Hostname, c)
	if state == "ok" && !res.OK {
		state, why = "failed", res.Error
	}
	ans := CreateAnswer{State: state, Error: why, Statements: tunnel.SetOwnerPlan(c), Steps: res.Steps}
	if ans.Steps == nil {
		ans.Steps = []tunnel.CreateStep{}
	}
	if state != "ok" {
		a.auditDB(ctx, "db.owner.failed", "warning", actor, db.Hostname,
			"Could not give database "+db.Name+" ("+db.Hostname+") to "+c.PGOwnerRole()+": "+why+".")
		writeJSON(w, http.StatusOK, ans)
		return
	}
	a.auditDB(ctx, "db.owner", "notice", actor, db.Hostname,
		"Gave database "+db.Name+" ("+db.Hostname+") to "+c.PGOwnerRole()+": "+strings.Join(res.Statements, "; ")+".")
	ans.Database = &db
	writeJSON(w, http.StatusOK, ans)
}

func engineCreateNoun(engine string) string {
	switch engine {
	case tunnel.EnginePostgres:
		return "Postgres database"
	case tunnel.EngineMySQL:
		return "MySQL database"
	case tunnel.EngineMongoDB:
		return "MongoDB database"
	case tunnel.EngineRabbitMQ:
		return "RabbitMQ vhost"
	case tunnel.EngineKafka:
		return "Kafka topic"
	}
	return engine
}

// askCreate sends one create request to the server's agent. The state is
// ok (the agent answered; res says how it went), offline, unsupported or
// failed, with why.
func (a *API) askCreate(ctx context.Context, actor, hostname string, c tunnel.GrantCreate) (tunnel.CreateResult, string, string) {
	var none tunnel.CreateResult
	if a.Tunnels == nil || a.Tunnels.GrantKey == nil {
		return none, "failed", "this control plane cannot reach agents: set signing_secret"
	}
	g := tunnel.NewGrant(actor, hostname, "create", "")
	g.Service = tunnel.CreateService
	g.Create = &c
	tok, err := tunnel.SignGrant(a.Tunnels.grantKeyFor(ctx), g)
	if err != nil {
		return none, "failed", err.Error()
	}
	octx, cancel := context.WithTimeout(ctx, createWait)
	defer cancel()
	st, open, err := a.Tunnels.OpenWith(octx, hostname, tunnel.Header{Service: tunnel.CreateService, Grant: tok})
	switch {
	case errors.Is(err, ErrNoTunnel):
		return none, "offline", "the agent on " + hostname + " is not connected; creating needs it"
	case tunnel.IsRefused(err) && strings.Contains(open.Error, "unknown service"):
		return none, "unsupported", "the agent on " + hostname + " is too old to create databases; update it from the server's Agent tab"
	case tunnel.IsRefused(err):
		return none, "failed", open.Error
	case err != nil:
		return none, "failed", "could not reach the agent on " + hostname + ": " + err.Error()
	}
	defer st.Close()
	go func() {
		<-octx.Done()
		_ = st.Close()
	}()
	fr := tunnel.NewFrameReader(st)
	for {
		typ, p, err := fr.Next()
		if err != nil {
			return none, "failed", "the agent went away before it answered; check the server before trying again"
		}
		if typ != tunnel.FrameCreateResult {
			continue
		}
		var res tunnel.CreateResult
		if err := json.Unmarshal(p, &res); err != nil {
			return none, "failed", "the agent's answer could not be read"
		}
		return res, "ok", ""
	}
}
