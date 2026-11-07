package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
MongoDB in the browser, through the tunnel.

	browser ──wss──▶ argus-control ──"mongodb" stream──▶ agent ──▶ mongod on this server

The agent signs in as MongoDB's administrator with the credentials in a file on
this server (tunnel.MongoAdminFile, or the entry's), and uses that connection
for one thing: giving the person a user in the entry's database with their
level's roles there (tunnel.MongoRoles) and a one-time password, and dropping
it when their last session closes. Every command the person runs, runs as that
user, so what they may do is what MongoDB's roles say.

A mongod that answers without anyone signing in has no access control, and a
level would mean nothing there, so the agent refuses it.
*/

// MongoDBService is the tunnel service name.
const MongoDBService = tunnel.MongoDBService

// MongoDB is the mongodb service.
func MongoDB(cfg StoreConfig) TunnelService {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	f := &mongoFactory{cfg: cfg, uses: &userUse{}}
	return storeService(cfg, tunnel.MongoDBService, tunnel.EngineMongoDB, f.open)
}

type mongoFactory struct {
	cfg  StoreConfig
	uses *userUse
	// mu serializes giving and taking users.
	mu sync.Mutex
}

type mongoSession struct {
	f    *mongoFactory
	g    tunnel.Grant
	db   tunnel.GrantDB
	host string
	port int
	file string

	user, pw string
	key      string
	client   *mongo.Client
	version  string
	timeout  time.Duration
	maxItems int
}

// mongoAdmin is what the admin file holds.
type mongoAdmin struct {
	User, Password, AuthSource, Mechanism string
}

// readMongoAdmin reads the admin file: key = value lines (user, password,
// authSource, authMechanism), # comments. The file must be readable by its
// owner only, as a password file is.
func readMongoAdmin(path string) (mongoAdmin, error) {
	if !tunnel.ValidMongoAdminFile(path) {
		return mongoAdmin{}, fmt.Errorf("%s is not an admin file the agent will read", path)
	}
	st, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return mongoAdmin{}, fmt.Errorf("MongoDB needs its administrator to give people users, and %s does not exist: "+
				"put user = and password = for an account with userAdmin on this database in it, readable by root only", path)
		}
		return mongoAdmin{}, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return mongoAdmin{}, fmt.Errorf("%s can be read by others; chmod 600 it, since it holds MongoDB's admin password", path)
	}
	b, err := readFileLimited(path, 64<<10)
	if err != nil {
		return mongoAdmin{}, err
	}
	a := mongoAdmin{AuthSource: "admin"}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		// key = value or key: value, split at whichever comes first, so a
		// password may hold either.
		at := strings.IndexAny(line, "=:")
		if at < 0 {
			continue
		}
		k, v := line[:at], line[at+1:]
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "user", "username":
			a.User = v
		case "password", "pass":
			a.Password = v
		case "authsource":
			a.AuthSource = v
		case "authmechanism", "mechanism":
			a.Mechanism = v
		}
	}
	if a.User == "" || a.Password == "" {
		return mongoAdmin{}, fmt.Errorf("%s has no user and password", path)
	}
	return a, nil
}

// mongoURI is a mongodb:// address for a host and port, or a unix socket.
func mongoURI(host string, port int) string {
	if strings.HasPrefix(host, "/") {
		return "mongodb://" + strings.ReplaceAll(url.QueryEscape(host), "+", "%20") + "/?directConnection=true"
	}
	return "mongodb://" + host + ":" + strconv.Itoa(port) + "/?directConnection=true"
}

func mongoClient(host string, port int, cred *options.Credential, timeout time.Duration) (*mongo.Client, error) {
	o := options.Client().ApplyURI(mongoURI(host, port)).SetAppName("argus-agent").
		SetServerSelectionTimeout(5 * time.Second).SetConnectTimeout(5 * time.Second).SetMaxPoolSize(4)
	if cred != nil {
		o.SetAuth(*cred)
	}
	if timeout > 0 {
		o.SetTimeout(timeout)
	}
	return mongo.Connect(o)
}

func (f *mongoFactory) open(ctx context.Context, g tunnel.Grant, _ string) (storeEngine, error) {
	db := *g.DB
	if !tunnel.LocalPGHost(db.Host) {
		return nil, errors.New("MongoDB must be on this server; " + db.Host + " is not")
	}
	if !tunnel.ValidMongoDB(db.Name) {
		return nil, errors.New("the grant names no MongoDB database")
	}
	s := &mongoSession{f: f, g: g, db: db, host: db.Host, port: db.Port, file: db.ConfigPath,
		timeout: ms(db.StatementTimeoutMS, defaultStmtTimeout), maxItems: db.MaxRows}
	if s.file == "" {
		s.file = tunnel.MongoAdminFile
	}
	if s.port == 0 {
		s.port = 27017
	}
	if s.maxItems <= 0 || s.maxItems > tunnel.MaxStoreItems {
		s.maxItems = tunnel.DefaultStoreItems * 5
	}
	s.user = tunnel.StoreUser(g.Principal, db.Level)
	s.key = fmt.Sprintf("%d/%s/%s", s.port, db.Name, s.user)

	admin, err := s.adminClient(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = admin.Disconnect(context.WithoutCancel(ctx)) }()

	f.mu.Lock()
	err = s.giveUser(ctx, admin)
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := s.connect(ctx); err != nil {
		s.takeUser(context.WithoutCancel(ctx))
		return nil, err
	}
	return s, nil
}

// adminClient signs in as MongoDB's administrator, after making sure the
// server has access control: one that lists its databases to anyone is
// refused.
func (s *mongoSession) adminClient(ctx context.Context) (*mongo.Client, error) {
	anon, err := mongoClient(s.host, s.port, nil, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("could not reach MongoDB at %s: %w", mongoWhere(s.host, s.port), err)
	}
	open := anon.Database("admin").RunCommand(ctx, bson.D{{Key: "listDatabases", Value: 1}, {Key: "nameOnly", Value: true}}).Err()
	var info bson.M
	infoErr := anon.Database("admin").RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&info)
	_ = anon.Disconnect(context.WithoutCancel(ctx))
	if infoErr != nil {
		return nil, fmt.Errorf("could not reach MongoDB at %s: %w", mongoWhere(s.host, s.port), infoErr)
	}
	s.version, _ = info["version"].(string)
	if !mongoVersionOK(s.version) {
		return nil, fmt.Errorf("MongoDB %s is older than Argus supports; it needs 5.0 or later", s.version)
	}
	if open == nil {
		return nil, errors.New("MongoDB on this server runs without access control, so anyone on the server can do anything " +
			"and a person's level would mean nothing; turn on security.authorization before adding it to Argus")
	}
	a, err := readMongoAdmin(s.file)
	if err != nil {
		return nil, err
	}
	cred := options.Credential{Username: a.User, Password: a.Password, AuthSource: a.AuthSource, AuthMechanism: a.Mechanism}
	c, err := mongoClient(s.host, s.port, &cred, 15*time.Second)
	if err != nil {
		return nil, err
	}
	if err := c.Ping(ctx, nil); err != nil {
		_ = c.Disconnect(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("MongoDB refused the administrator in %s: %w", s.file, err)
	}
	return c, nil
}

func mongoWhere(host string, port int) string {
	if strings.HasPrefix(host, "/") {
		return host
	}
	return host + ":" + strconv.Itoa(port)
}

// mongoVersionOK reports whether a server version is 5.0 or later. An
// unknown version is let through; the commands themselves will say.
func mongoVersionOK(v string) bool {
	major, err := strconv.Atoi(strings.SplitN(v, ".", 2)[0])
	return err != nil || major >= 5
}

// giveUser makes the person's user for this level in the entry's database,
// or reuses it while another session holds it. Users Argus left behind in this
// database -- an agent that stopped mid-session -- are dropped first.
func (s *mongoSession) giveUser(ctx context.Context, admin *mongo.Client) error {
	adb := admin.Database(s.db.Name)
	var have struct {
		Users []struct {
			User string `bson:"user"`
		} `bson:"users"`
	}
	if err := adb.RunCommand(ctx, bson.D{{Key: "usersInfo", Value: 1}}).Decode(&have); err != nil {
		return fmt.Errorf("could not list MongoDB's users in %s as the administrator: %w", s.db.Name, err)
	}
	exists := false
	for _, u := range have.Users {
		key := fmt.Sprintf("%d/%s/%s", s.port, s.db.Name, u.User)
		if u.User == s.user {
			exists = true
		}
		if tunnel.IsStoreUser(u.User) && !s.f.uses.live(key) {
			if err := adb.RunCommand(ctx, bson.D{{Key: "dropUser", Value: u.User}}).Err(); err == nil && u.User == s.user {
				exists = false
			}
		}
	}
	pw, fresh, err := s.f.uses.password(s.key, sessionPassword)
	if err != nil {
		return err
	}
	s.pw = pw
	s.f.uses.add(s.key, 1)
	if !fresh && exists {
		return nil
	}
	roles := bson.A{}
	for _, r := range tunnel.MongoRoles(s.db.Level, s.db.Name) {
		roles = append(roles, bson.D{{Key: "role", Value: r.Role}, {Key: "db", Value: r.DB}})
	}
	cmd := bson.D{{Key: "createUser", Value: s.user}, {Key: "pwd", Value: pw}, {Key: "roles", Value: roles},
		{Key: "mechanisms", Value: bson.A{"SCRAM-SHA-256"}}}
	if exists {
		cmd = bson.D{{Key: "updateUser", Value: s.user}, {Key: "pwd", Value: pw}, {Key: "roles", Value: roles},
			{Key: "mechanisms", Value: bson.A{"SCRAM-SHA-256"}}}
	}
	if err := adb.RunCommand(ctx, cmd).Err(); err != nil {
		s.f.uses.add(s.key, -1)
		return fmt.Errorf("could not prepare your MongoDB user: %w", err)
	}
	return nil
}

// takeUser drops the person's user when this was its last session. MongoDB
// stops honouring a dropped user's connections.
func (s *mongoSession) takeUser(ctx context.Context) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if s.f.uses.add(s.key, -1) > 0 {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	admin, err := s.adminClient(cctx)
	if err != nil {
		s.f.cfg.Log.Error("mongodb: could not drop a user after its session", "user", s.user, "error", err,
			"detail", "it is dropped at the next session on this database")
		return
	}
	defer func() { _ = admin.Disconnect(cctx) }()
	if err := admin.Database(s.db.Name).RunCommand(cctx, bson.D{{Key: "dropUser", Value: s.user}}).Err(); err != nil {
		s.f.cfg.Log.Error("mongodb: could not drop a user after its session", "user", s.user, "error", err)
	}
}

// connect signs in as the person's user.
func (s *mongoSession) connect(ctx context.Context) error {
	cred := options.Credential{Username: s.user, Password: s.pw, AuthSource: s.db.Name, AuthMechanism: "SCRAM-SHA-256"}
	c, err := mongoClient(s.host, s.port, &cred, s.timeout)
	if err == nil {
		err = c.Ping(ctx, nil)
		if err != nil {
			_ = c.Disconnect(context.WithoutCancel(ctx))
		}
	}
	if err != nil {
		return fmt.Errorf("could not sign in to MongoDB as %s: %w", s.user, err)
	}
	s.client = c
	return nil
}

func (s *mongoSession) ready() tunnel.StoreAnswer {
	return tunnel.StoreAnswer{Engine: tunnel.EngineMongoDB, User: s.user, Version: s.version,
		Ops: []string{"run", "scan", "value"}}
}

func (s *mongoSession) close(ctx context.Context) {
	if s.client != nil {
		_ = s.client.Disconnect(context.WithoutCancel(ctx))
	}
	s.takeUser(ctx)
}

func (s *mongoSession) do(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	switch req.Op {
	case "run":
		return s.run(ctx, req, rec)
	case "scan":
		return s.scan(ctx, req)
	case "value":
		return s.value(ctx, req, rec)
	}
	return tunnel.StoreAnswer{Type: "error", Error: "unknown operation " + req.Op + " for MongoDB"}, nil
}

func (s *mongoSession) run(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if len(req.Command) > tunnel.MaxCommand {
		return tunnel.StoreAnswer{Type: "error", Error: "the command is too long"}, nil
	}
	text := strings.TrimSpace(req.Command)
	call, err := parseMongoLine(text)
	if err != nil {
		return tunnel.StoreAnswer{Type: "error", Error: err.Error()}, nil
	}
	if call.Refuse != "" {
		if err := rec.refused(req.ID, text, call.Refuse); err != nil {
			return tunnel.StoreAnswer{}, err
		}
		return tunnel.StoreAnswer{Type: "error", Error: call.Refuse}, nil
	}
	if call.Confirm != "" && req.Confirm != s.g.Host {
		why := strings.ToUpper(call.Confirm[:1]) + call.Confirm[1:] + " needs you to type the server's name, " + s.g.Host + ", to confirm"
		if err := rec.refused(req.ID, text, why); err != nil {
			return tunnel.StoreAnswer{}, err
		}
		return tunnel.StoreAnswer{Type: "error", NeedsConfirm: true, Error: why}, nil
	}
	return s.exec(ctx, req.ID, text, call, rec)
}

// bounded caps what a reading command may return at one more document than
// the session shows, so the answer can say there were more.
func (s *mongoSession) bounded(call mongoCall) bson.D {
	cmd := append(bson.D{}, call.Cmd...)
	most := int64(s.maxItems) + 1
	switch cmd[0].Key {
	case "find":
		batch := most
		if call.Limit > 0 && call.Limit < batch {
			batch = call.Limit
		}
		cmd = append(cmd, bson.E{Key: "batchSize", Value: batch}, bson.E{Key: "singleBatch", Value: true})
	case "aggregate", "listCollections", "listIndexes":
		out := bson.D{}
		for _, e := range cmd {
			if e.Key != "cursor" {
				out = append(out, e)
			}
		}
		cmd = append(out, bson.E{Key: "cursor", Value: bson.D{{Key: "batchSize", Value: most}}})
	}
	return cmd
}

// exec records a command, runs it as the person's user and answers with what
// it returned: a cursor's documents, or the reply itself.
func (s *mongoSession) exec(ctx context.Context, id uint32, text string, call mongoCall, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	if err := rec.stmt(id, text); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	start := time.Now()
	raw, err := s.client.Database(s.db.Name).RunCommand(ctx, s.bounded(call)).Raw()
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		why := mongoErrText(err, s.timeout)
		if rerr := rec.fail(id, why, elapsed); rerr != nil {
			return tunnel.StoreAnswer{}, rerr
		}
		if ctx.Err() == nil && errors.Is(err, mongo.ErrClientDisconnected) {
			return tunnel.StoreAnswer{}, errors.New("the connection to MongoDB ended")
		}
		return tunnel.StoreAnswer{Type: "error", Error: why, MS: elapsed}, nil
	}
	docs, cursorID, isCursor := mongoBatch(raw)
	if isCursor && cursorID != 0 {
		ns, _ := raw.Lookup("cursor", "ns").StringValueOK()
		coll := call.Coll
		if _, c, ok := strings.Cut(ns, "."); ok {
			coll = c
		}
		kctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_ = s.client.Database(s.db.Name).RunCommand(kctx, bson.D{{Key: "killCursors", Value: coll}, {Key: "cursors", Value: bson.A{cursorID}}}).Err()
		cancel()
	}
	if !isCursor {
		docs = []bson.Raw{raw}
	}
	truncated := isCursor && (cursorID != 0 || len(docs) > s.maxItems)
	if len(docs) > s.maxItems {
		docs = docs[:s.maxItems]
	}
	out, cut := mongoJSON(docs)
	truncated = truncated || cut
	tag := mongoTag(call, raw, len(out), isCursor)
	if we := mongoWriteError(raw); we != "" {
		if err := rec.fail(id, we, elapsed); err != nil {
			return tunnel.StoreAnswer{}, err
		}
		return tunnel.StoreAnswer{Type: "error", Error: we, Data: out, Tag: tag, MS: elapsed}, nil
	}
	if err := rec.done(id, tag, int64(len(out)), elapsed, out, truncated); err != nil {
		return tunnel.StoreAnswer{}, err
	}
	return tunnel.StoreAnswer{Type: "result", Data: out, Tag: tag, MS: elapsed, Truncated: truncated}, nil
}

// mongoErrText is a failed command's reason, in MongoDB's words.
func mongoErrText(err error, timeout time.Duration) string {
	var ce mongo.CommandError
	switch {
	case errors.As(err, &ce):
		if ce.Code == 50 || ce.HasErrorLabel("ExceededTimeLimit") {
			return fmt.Sprintf("MongoDB stopped the command after %s, the limit for this database", timeout)
		}
		return ce.Message
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded) || mongo.IsTimeout(err):
		return fmt.Sprintf("MongoDB did not answer within %s; the command was stopped", timeout)
	}
	return err.Error()
}

// mongoBatch reads a cursor reply's first batch.
func mongoBatch(raw bson.Raw) (docs []bson.Raw, id int64, ok bool) {
	cur, isDoc := raw.Lookup("cursor").DocumentOK()
	if !isDoc {
		return nil, 0, false
	}
	batch, isArr := cur.Lookup("firstBatch").ArrayOK()
	if !isArr {
		return nil, 0, false
	}
	vals, _ := batch.Values()
	for _, v := range vals {
		if d, ok := v.DocumentOK(); ok {
			docs = append(docs, d)
		}
	}
	id, _ = cur.Lookup("id").Int64OK()
	return docs, id, true
}

// mongoJSON is each document as relaxed Extended JSON, within the bytes one
// answer may carry.
func mongoJSON(docs []bson.Raw) ([]json.RawMessage, bool) {
	out := make([]json.RawMessage, 0, len(docs))
	total := 0
	for _, d := range docs {
		b, err := bson.MarshalExtJSON(d, false, false)
		if err != nil {
			b, _ = json.Marshal(map[string]string{"$argusError": "this document could not be shown: " + err.Error()})
		}
		if total+len(b) > tunnel.MaxStoreAnswer {
			return out, true
		}
		total += len(b)
		out = append(out, b)
	}
	return out, false
}

// mongoWriteError is the first write error a write command reported.
func mongoWriteError(raw bson.Raw) string {
	if arr, ok := raw.Lookup("writeErrors").ArrayOK(); ok {
		vals, _ := arr.Values()
		for _, v := range vals {
			if d, ok := v.DocumentOK(); ok {
				msg, _ := d.Lookup("errmsg").StringValueOK()
				return msg
			}
		}
	}
	if d, ok := raw.Lookup("writeConcernError").DocumentOK(); ok {
		msg, _ := d.Lookup("errmsg").StringValueOK()
		return msg
	}
	return ""
}

func mongoInt(v bson.RawValue) int64 {
	if n, ok := v.AsInt64OK(); ok {
		return n
	}
	if f, ok := v.DoubleOK(); ok {
		return int64(f)
	}
	return 0
}

// mongoTag says what a command did, as the recording and the console show it.
func mongoTag(call mongoCall, raw bson.Raw, docs int, cursor bool) string {
	name := call.Cmd[0].Key
	switch name {
	case "count":
		return fmt.Sprintf("count %d", mongoInt(raw.Lookup("n")))
	case "insert":
		return fmt.Sprintf("insert %d", mongoInt(raw.Lookup("n")))
	case "delete":
		return fmt.Sprintf("delete %d", mongoInt(raw.Lookup("n")))
	case "update":
		return fmt.Sprintf("update %d matched %d changed", mongoInt(raw.Lookup("n")), mongoInt(raw.Lookup("nModified")))
	case "distinct":
		if arr, ok := raw.Lookup("values").ArrayOK(); ok {
			vals, _ := arr.Values()
			return fmt.Sprintf("distinct %d", len(vals))
		}
	}
	if cursor {
		return fmt.Sprintf("%s %d", name, docs)
	}
	return name
}

// scan lists the collections the person may see, with the server's estimate
// of each one's size. Not recorded -- it reads names, as the Postgres tree
// reads table names -- and it runs as the person's user.
func (s *mongoSession) scan(ctx context.Context, req tunnel.StoreRequest) (tunnel.StoreAnswer, error) {
	raw, err := s.client.Database(s.db.Name).RunCommand(ctx, bson.D{{Key: "listCollections", Value: 1},
		{Key: "authorizedCollections", Value: true}, {Key: "nameOnly", Value: false},
		{Key: "cursor", Value: bson.D{{Key: "batchSize", Value: 1000}}}}).Raw()
	if err != nil {
		return tunnel.StoreAnswer{Type: "error", Error: mongoErrText(err, s.timeout)}, nil
	}
	docs, _, _ := mongoBatch(raw)
	match := strings.ToLower(strings.Trim(strings.TrimSpace(req.Pattern), "*"))
	ans := tunnel.StoreAnswer{Type: "result", Keys: []tunnel.RedisKey{}, Cursor: "0"}
	for _, d := range docs {
		name, _ := d.Lookup("name").StringValueOK()
		kind, _ := d.Lookup("type").StringValueOK()
		if name == "" || strings.HasPrefix(name, "system.") || (match != "" && !strings.Contains(strings.ToLower(name), match)) {
			continue
		}
		k := tunnel.RedisKey{Name: name, Type: kind, TTL: -1}
		if kind == "collection" || kind == "timeseries" {
			var n struct {
				N int64 `bson:"n"`
			}
			cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			if s.client.Database(s.db.Name).RunCommand(cctx, bson.D{{Key: "count", Value: name}}).Decode(&n) == nil {
				k.Count = n.N
			}
			cancel()
		}
		ans.Keys = append(ans.Keys, k)
	}
	ans.Total = int64(len(ans.Keys))
	return ans, nil
}

// value reads a collection's first documents. It is recorded, because reading
// documents is reading the data.
func (s *mongoSession) value(ctx context.Context, req tunnel.StoreRequest, rec *stmtRecorder) (tunnel.StoreAnswer, error) {
	coll := req.Key
	if coll == "" || len(coll) > 255 || strings.ContainsAny(coll, "\x00$") {
		return tunnel.StoreAnswer{Type: "error", Error: "no collection to read"}, nil
	}
	n := int64(req.Count)
	if n <= 0 {
		n = tunnel.DefaultStoreItems
	}
	n = min(n, int64(s.maxItems))
	call := mongoCall{Cmd: bson.D{{Key: "find", Value: coll}, {Key: "filter", Value: bson.D{}}, {Key: "limit", Value: n}},
		Coll: coll, Method: "find", Limit: n}
	text := fmt.Sprintf("db.getCollection(%s).find({}).limit(%d)", strconv.Quote(coll), n)
	ans, err := s.exec(ctx, req.ID, text, call, rec)
	if err != nil || ans.Type != "result" {
		return ans, err
	}
	var total struct {
		N int64 `bson:"n"`
	}
	if s.client.Database(s.db.Name).RunCommand(ctx, bson.D{{Key: "count", Value: coll}}).Decode(&total) == nil {
		ans.Total = total.N
		ans.Truncated = total.N > n
	}
	ans.KeyType = "collection"
	return ans, nil
}

// discoverMongo lists the databases on a mongod as its administrator, with
// their size on disk, leaving out MongoDB's own.
func discoverMongo(ctx context.Context, d tunnel.GrantDiscover) tunnel.DiscoverResult {
	s := &mongoSession{host: d.Host, port: d.Port, file: d.ConfigPath}
	if s.file == "" {
		s.file = tunnel.MongoAdminFile
	}
	admin, err := s.adminClient(ctx)
	if err != nil {
		return tunnel.DiscoverResult{Error: err.Error(), Version: s.version}
	}
	defer func() { _ = admin.Disconnect(context.WithoutCancel(ctx)) }()
	var out struct {
		Databases []struct {
			Name string `bson:"name"`
			Size int64  `bson:"sizeOnDisk"`
		} `bson:"databases"`
	}
	if err := admin.Database("admin").RunCommand(ctx, bson.D{{Key: "listDatabases", Value: 1}}).Decode(&out); err != nil {
		return tunnel.DiscoverResult{Error: "MongoDB would not list its databases: " + err.Error(), Version: s.version}
	}
	res := tunnel.DiscoverResult{OK: true, Version: s.version}
	for _, db := range out.Databases {
		if tunnel.IsMongoSystemDB(db.Name) {
			continue
		}
		res.Databases = append(res.Databases, tunnel.Discovered{Name: db.Name, Bytes: db.Size})
	}
	return res
}
