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
	POST /api/v1/databases/{id}/owner                   change the database's owner
	POST /api/v1/databases/{id}/owner/password          give the owner user a new password

The owner may be a user made in the same step, for the app (see
plans/create-db-owner/design.md). Its password comes in the request, goes to
the agent in a frame of its own after the grant, and is never stored, logged
or put in Activity: every statement shown says [redacted] where it was.

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
	MSSQL    *tunnel.MSSQLCreate  `json:"mssql,omitempty"`

	// Owner is who owns the database; nil is Argus's. Password is a new
	// owner's password: sent to the agent once, kept nowhere.
	Owner    *tunnel.CreateOwner `json:"owner,omitempty"`
	Password string              `json:"password,omitempty"`

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
	// Connection is how the owner user connects, for the details shown
	// once; the password is the browser's own copy, never sent back.
	Connection *OwnerConnection `json:"connection,omitempty"`
	// Manual are lines to add by hand where the agent could not (PgBouncer).
	Manual []string `json:"manual,omitempty"`
}

// OwnerConnection is where the owner user connects.
type OwnerConnection struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Database string `json:"database"`
	User     string `json:"user"`
	// DirectPort is Postgres's own port when Port is PgBouncer's.
	DirectPort int `json:"directPort,omitempty"`
	// Pooled says Port is PgBouncer's, and PoolMode how it pools.
	Pooled   bool   `json:"pooled,omitempty"`
	PoolMode string `json:"poolMode,omitempty"`
	// AuthSource is MongoDB's: the database the user lives in.
	AuthSource string `json:"authSource,omitempty"`
}

// ownerConnection is the details for the owner c names, after res.
func ownerConnection(asset Asset, c tunnel.GrantCreate, res tunnel.CreateResult) *OwnerConnection {
	if c.Owner == nil || c.OwnerName() == "" {
		return nil
	}
	oc := &OwnerConnection{Host: asset.Hostname, Port: c.Port, Database: c.Name, User: c.Owner.Name}
	if c.Engine == tunnel.EngineMongoDB {
		oc.AuthSource = c.Owner.From
		if c.Owner.Kind == tunnel.OwnerNew {
			oc.AuthSource = c.Name
		}
	}
	if res.Pooled && c.PoolerPort > 0 {
		oc.Port, oc.DirectPort, oc.Pooled = c.PoolerPort, c.Port, true
	}
	if res.AppPort > 0 {
		oc.Port = res.AppPort
	}
	return oc
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
		ConfigPath: d.ConfigPath, Name: name, PoolerPort: d.PoolerPort,
		Postgres: in.Postgres, MySQL: in.MySQL, Mongo: in.Mongo, Rabbit: in.Rabbit, Kafka: in.Kafka, MSSQL: in.MSSQL}
	if op == tunnel.CreateOpCreate {
		c.Owner = in.Owner
	}
	if err := c.Validate(); err != nil {
		return c, d, ErrInvalidDatabase{err.Error()}
	}
	if op == tunnel.CreateOpCreate && c.NewOwner() && !in.DryRun {
		if why := tunnel.OwnerPasswordProblem(c, in.Password); why != "" {
			return c, d, ErrInvalidDatabase{why}
		}
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
	in.PoolerPort, _ = strconv.Atoi(q.Get("poolerPort"))
	c, _, err := probeCreate(in, tunnel.CreateOpOptions)
	if err != nil {
		a.dbErr(w, "create options", err)
		return
	}
	res, state, why := a.askCreate(r.Context(), actor, asset.Hostname, c, "")
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
	pw := in.Password
	in.Password = ""
	defer func() { pw = "" }()
	if in.Name = strings.TrimSpace(in.Name); in.Name == "" {
		writeErr(w, http.StatusBadRequest, "the name is required")
		return
	}
	in.Password = pw
	c, entry, err := probeCreate(in, tunnel.CreateOpCreate)
	in.Password = ""
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
	res, state, why := a.askCreate(ctx, actor, asset.Hostname, c, pw)
	ans.Manual = res.Manual
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
	if !tunnel.BrokerOwnerEngine(c.Engine) {
		a.auditOwnerUser(ctx, actor, asset.Hostname, c, res)
	}
	ans.Connection = ownerConnection(asset, c, res)

	if !addEntry {
		if ans.CoveredBy != nil {
			a.auditBrokerOwner(ctx, actor, asset.Hostname, c, res, ans.CoveredBy.ID)
		}
		ans.Steps = append(ans.Steps, tunnel.CreateStep{OK: true,
			Text: "Covered by the entry " + ans.CoveredBy.Name + ", so nothing new is added to Argus"})
		writeJSON(w, http.StatusOK, ans)
		return
	}
	if len(res.Manual) > 0 {
		// PgBouncer does not send it yet: sessions go straight to Postgres
		// until someone adds the lines.
		entry.PoolerPort, entry.PoolerHost = 0, ""
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
		a.auditBrokerOwner(ctx, actor, asset.Hostname, c, res, "")
		ans.State, ans.Error = "partial", "made on "+asset.Hostname+", but not added to Argus: "+msg+". Add it from Already on "+asset.Hostname+"."
		ans.Steps = append(ans.Steps, tunnel.CreateStep{Text: "Add it to Argus", Error: msg})
		writeJSON(w, http.StatusOK, ans)
		return
	}
	a.auditDB(ctx, "db.added", "notice", actor, db.Hostname,
		"Added "+engineName(db.Engine)+" "+db.Name+" ("+db.Label+") on "+db.Hostname+" at "+db.Host+":"+itoa(db.Port)+".")
	ans.Steps = append(ans.Steps, tunnel.CreateStep{Text: "Added it to Argus as " + db.Label, OK: true})
	a.auditBrokerOwner(ctx, actor, asset.Hostname, c, res, db.ID)
	a.applyRulesTo(ctx, db, actor)
	ans.Database = &db
	writeJSON(w, http.StatusOK, ans)
}

// OwnerRequest is a change of a database's owner. Owner is the older
// console's Postgres role name ("" for Argus's Owner level).
type OwnerRequest struct {
	Owner    string `json:"owner"`
	Kind     string `json:"kind,omitempty"`
	Name     string `json:"name,omitempty"`
	From     string `json:"from,omitempty"`
	Policy   bool   `json:"policy,omitempty"`
	Conn     int    `json:"connLimit,omitempty"`
	Password string `json:"password,omitempty"`
	// DryRun answers with the statements and changes nothing.
	DryRun bool `json:"dryRun,omitempty"`
}

func ownedGrant(db Database, op string) tunnel.GrantCreate {
	engine := db.Engine
	if engine == "" {
		engine = tunnel.EnginePostgres
	}
	return tunnel.GrantCreate{Op: op, Engine: engine, Host: db.Host, Port: db.Port, AdminUser: db.AdminUser,
		ConfigPath: db.ConfigPath, Name: db.Name, PoolerPort: db.PoolerPort}
}

// POST /api/v1/databases/{id}/owner
func (a *API) postDatabaseOwner(w http.ResponseWriter, r *http.Request, actor string) {
	ctx := r.Context()
	db, err := a.store.Database(ctx, r.PathValue("id"))
	if err != nil {
		a.dbErr(w, "database", err)
		return
	}
	var in OwnerRequest
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	pw := in.Password
	in.Password = ""
	c := ownedGrant(db, tunnel.CreateOpSetOwner)
	if in.Kind == "" && in.Name == "" {
		if c.Engine != tunnel.EnginePostgres {
			writeErr(w, http.StatusBadRequest, "name the user that is to own it")
			return
		}
		c.Postgres = &tunnel.PGCreate{Owner: strings.TrimSpace(in.Owner)}
	} else {
		c.Owner = &tunnel.CreateOwner{Kind: in.Kind, Name: in.Name, From: in.From, Policy: in.Policy, ConnLimit: in.Conn}
	}
	if err := c.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if c.NewOwner() && !in.DryRun {
		if why := tunnel.OwnerPasswordProblem(c, pw); why != "" {
			writeErr(w, http.StatusBadRequest, why)
			return
		}
	}
	if in.DryRun {
		writeJSON(w, http.StatusOK, CreateAnswer{State: "ok", Statements: tunnel.SetOwnerPlan(c), Steps: []tunnel.CreateStep{}})
		return
	}
	res, state, why := a.askCreate(ctx, actor, db.Hostname, c, pw)
	pw = ""
	if state == "ok" && !res.OK {
		state, why = "failed", res.Error
	}
	ans := CreateAnswer{State: state, Error: why, Statements: tunnel.SetOwnerPlan(c), Steps: res.Steps, Manual: res.Manual}
	if len(res.Statements) > 0 {
		ans.Statements = res.Statements
	}
	if ans.Steps == nil {
		ans.Steps = []tunnel.CreateStep{}
	}
	who := c.OwnerName()
	if who == "" {
		who = c.PGOwnerRole()
		if c.Engine == tunnel.EngineMSSQL {
			who = "Argus's admin login"
		}
	}
	if state != "ok" {
		a.auditDB(ctx, "db.owner.failed", "warning", actor, db.Hostname,
			"Could not give database "+db.Name+" ("+db.Hostname+") to "+who+": "+why+".")
		writeJSON(w, http.StatusOK, ans)
		return
	}
	a.auditDB(ctx, "db.owner", "notice", actor, db.Hostname,
		"Gave database "+db.Name+" ("+db.Hostname+") to "+who+": "+strings.Join(ans.Statements, "; ")+".")
	a.auditOwnerUser(ctx, actor, db.Hostname, c, res)
	if asset, err := a.store.AssetByID(ctx, db.AssetID); err == nil {
		ans.Connection = ownerConnection(asset, c, res)
	}
	ans.Database = &db
	writeJSON(w, http.StatusOK, ans)
}

// POST /api/v1/databases/{id}/owner/password
func (a *API) postOwnerPassword(w http.ResponseWriter, r *http.Request, actor string) {
	ctx := r.Context()
	db, err := a.store.Database(ctx, r.PathValue("id"))
	if err != nil {
		a.dbErr(w, "database", err)
		return
	}
	var in OwnerRequest
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	pw := in.Password
	in.Password = ""
	c := ownedGrant(db, tunnel.CreateOpSetPassword)
	c.Owner = &tunnel.CreateOwner{Kind: tunnel.OwnerExisting, Name: in.Name, From: in.From}
	if err := c.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if why := tunnel.OwnerPasswordProblem(c, pw); why != "" {
		writeErr(w, http.StatusBadRequest, why)
		return
	}
	res, state, why := a.askCreate(ctx, actor, db.Hostname, c, pw)
	pw = ""
	if state == "ok" && !res.OK {
		state, why = "failed", res.Error
	}
	ans := CreateAnswer{State: state, Error: why, Statements: tunnel.SetPasswordPlan(c), Steps: res.Steps}
	if ans.Steps == nil {
		ans.Steps = []tunnel.CreateStep{}
	}
	if state != "ok" {
		a.auditDB(ctx, "db.user.password.failed", "warning", actor, db.Hostname,
			"Could not give "+c.Owner.Name+", owner of "+db.Name+" ("+db.Hostname+"), a new password: "+why+".")
		writeJSON(w, http.StatusOK, ans)
		return
	}
	a.auditDB(ctx, "db.user.password", "notice", actor, db.Hostname,
		"Gave "+c.Owner.Name+", owner of "+db.Name+" ("+db.Hostname+"), a new password: "+strings.Join(ans.Statements, "; ")+".")
	if asset, err := a.store.AssetByID(ctx, db.AssetID); err == nil {
		ans.Connection = ownerConnection(asset, c, res)
	}
	ans.Database = &db
	writeJSON(w, http.StatusOK, ans)
}

// auditOwnerUser records a user made to own a database on its own line:
// it is a new way into the data that does not go through Argus.
func (a *API) auditOwnerUser(ctx context.Context, actor, hostname string, c tunnel.GrantCreate, res tunnel.CreateResult) {
	if !c.NewOwner() {
		return
	}
	signed := ""
	for _, s := range res.Steps {
		if s.OK && strings.Contains(s.Text, " signs in") {
			signed = " " + s.Text + "."
			break
		}
	}
	a.auditDB(ctx, "db.user.created", "notice", actor, hostname,
		"Made "+c.Owner.Name+" on "+hostname+" to own "+engineCreateNoun(c.Engine)+" "+c.Name+
			". It signs in straight to the database, not through Argus."+signed)
}

// auditBrokerOwner records the app user made with a vhost or topic as the
// Users tab's own broker.user.created, so its Made column finds it; without
// an entry, as a database owner's is.
func (a *API) auditBrokerOwner(ctx context.Context, actor, hostname string, c tunnel.GrantCreate, res tunnel.CreateResult, entryID string) {
	if !tunnel.BrokerOwnerEngine(c.Engine) || !c.NewOwner() {
		return
	}
	if entryID == "" {
		a.auditOwnerUser(ctx, actor, hostname, c, res)
		return
	}
	pre, post := tunnel.OwnerPlan(c, tunnel.PasswordRedacted)
	a.auditDB(ctx, "broker.user.created", "notice", actor, hostname,
		brokerUserDetail(c.Owner.Name, entryID)+oneLine(strings.Join(append(pre, post...), "; "))+".")
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
	case tunnel.EngineNATS:
		return "NATS account"
	case tunnel.EngineMSSQL:
		return "SQL Server database"
	}
	return engine
}

// askCreate sends one create request to the server's agent. The state is
// ok (the agent answered; res says how it went), offline, unsupported or
// failed, with why.
func (a *API) askCreate(ctx context.Context, actor, hostname string, c tunnel.GrantCreate, pw string) (tunnel.CreateResult, string, string) {
	var none tunnel.CreateResult
	if a.Tunnels == nil || a.Tunnels.GrantKey == nil {
		return none, "failed", "this control plane cannot reach agents: set signing_secret"
	}
	g := tunnel.NewGrant(actor, hostname, "create", "")
	g.Service = tunnel.CreateServiceFor(c)
	g.Create = &c
	tok, err := tunnel.SignGrant(a.Tunnels.grantKeyFor(ctx), g)
	if err != nil {
		return none, "failed", err.Error()
	}
	octx, cancel := context.WithTimeout(ctx, createWait)
	defer cancel()
	st, open, err := a.Tunnels.OpenWith(octx, hostname, tunnel.Header{Service: g.Service, Grant: tok})
	switch {
	case errors.Is(err, ErrNoTunnel):
		return none, "offline", "the agent on " + hostname + " is not connected; creating needs it"
	case tunnel.IsRefused(err) && strings.Contains(open.Error, "unknown service"):
		if g.Service == tunnel.CreateOwnedService {
			return none, "unsupported", "the agent on " + hostname + " is too old to make a database's owner user; update it from the server's Agent tab"
		}
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
	if c.NeedsPassword() {
		// Never in the grant: a frame of its own, read once by the agent.
		if err := tunnel.WriteFrame(st, tunnel.FrameCreatePassword, []byte(pw)); err != nil {
			return none, "failed", "could not send the password to the agent on " + hostname
		}
	}
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
