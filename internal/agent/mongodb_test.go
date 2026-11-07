//go:build !windows

package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
Against a real MongoDB: ARGUS_TEST_MONGODB_PORT names a mongod on 127.0.0.1
with access control on, and ARGUS_TEST_MONGODB_ADMIN a file (mode 600) with
user = and password = for an account with root on it. Without them these
tests skip; the parser and the file reading are tested regardless.
*/

type mongoServer struct {
	port  int
	admin string
	db    string
	c     *mongo.Client
}

func startMongo(t *testing.T) mongoServer {
	t.Helper()
	p := os.Getenv("ARGUS_TEST_MONGODB_PORT")
	file := os.Getenv("ARGUS_TEST_MONGODB_ADMIN")
	if p == "" || file == "" {
		t.Skip("ARGUS_TEST_MONGODB_PORT and ARGUS_TEST_MONGODB_ADMIN are not set")
	}
	port, _ := strconv.Atoi(p)
	a, err := readMongoAdmin(file)
	if err != nil {
		t.Fatal(err)
	}
	c, err := mongoClient("127.0.0.1", port, &options.Credential{Username: a.User, Password: a.Password, AuthSource: a.AuthSource}, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	db := "argus_t_" + strconv.FormatInt(time.Now().UnixNano()%1e9, 36)
	t.Cleanup(func() {
		_ = c.Database(db).Drop(context.Background())
		_ = c.Disconnect(context.Background())
	})
	return mongoServer{port: port, admin: file, db: db, c: c}
}

func (m mongoServer) users(t *testing.T) []string {
	t.Helper()
	var out struct {
		Users []struct {
			User string `bson:"user"`
		} `bson:"users"`
	}
	if err := m.c.Database(m.db).RunCommand(context.Background(), bson.D{{Key: "usersInfo", Value: 1}}).Decode(&out); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, u := range out.Users {
		names = append(names, u.User)
	}
	return names
}

func openMongo(t *testing.T, m mongoServer, svc TunnelService, level, dir string, maxRows int) *storeHarness {
	t.Helper()
	h, err := openStoreSession(t, svc, tunnel.MongoDBService, tunnel.GrantDB{ID: "m1", Engine: tunnel.EngineMongoDB,
		Name: m.db, Host: "127.0.0.1", Port: m.port, Level: level, ConfigPath: m.admin, MaxRows: maxRows}, dir)
	if err != nil {
		t.Fatalf("mongodb session refused: %v", err)
	}
	return h
}

func docsOf(t *testing.T, a tunnel.StoreAnswer) []map[string]any {
	t.Helper()
	b, _ := json.Marshal(a.Data)
	var out []map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("data: %s", b)
	}
	return out
}

func TestMongoDBLevelsAreMongoRoles(t *testing.T) {
	m := startMongo(t)
	ctx := context.Background()
	if _, err := m.c.Database(m.db).Collection("orders").InsertOne(ctx, bson.D{{Key: "n", Value: 1}, {Key: "status", Value: "paid"}}); err != nil {
		t.Fatal(err)
	}
	// Someone else's user in the same database, which Argus must leave alone.
	if err := m.c.Database(m.db).RunCommand(ctx, bson.D{{Key: "createUser", Value: "report"}, {Key: "pwd", Value: "x-report-1"},
		{Key: "roles", Value: bson.A{"read"}}}).Err(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	svc := MongoDB(StoreConfig{Verifier: &tunnel.GrantVerifier{Host: "cache-01"}, RecordDir: dir})

	read := openMongo(t, m, svc, tunnel.LevelRead, dir, 0)
	if read.ready.User != "argus-ana_sari-read" || read.ready.Engine != "mongodb" || read.ready.Version == "" {
		t.Fatalf("ready: %+v", read.ready)
	}
	if a := read.run(`db.orders.find({ status: "paid" })`); a.Type != "result" || len(docsOf(t, a)) != 1 || a.Tag != "find 1" {
		t.Fatalf("find at read: %+v", a)
	}
	if !read.recorded("stmt", `db.orders.find({ status: "paid" })`) {
		t.Fatal("the find is not in the recording")
	}
	// The refusal is MongoDB's own: the agent sent it, and the role said no.
	if a := read.run(`db.orders.insertOne({ n: 2 })`); a.Type != "error" || !strings.Contains(a.Error, "not authorized") {
		t.Fatalf("insert at read should be refused by MongoDB, got %+v", a)
	}
	if !read.recorded("stmt", `db.orders.insertOne({ n: 2 })`) {
		t.Fatal("the refused insert is not in the recording")
	}

	write := openMongo(t, m, svc, tunnel.LevelWrite, dir, 0)
	if a := write.run(`db.orders.insertMany([{ n: 2 }, { n: 3 }])`); a.Type != "result" || a.Tag != "insert 2" {
		t.Fatalf("insert at write: %+v", a)
	}
	if a := write.run(`db.orders.createIndex({ n: 1 })`); a.Type != "error" {
		t.Fatalf("createIndex is not readWrite's: %+v", a)
	}
	// Wiping a collection waits for the server's name.
	if a := write.run(`db.orders.deleteMany({})`); !a.NeedsConfirm {
		t.Fatalf("deleteMany({}) without the name: %+v", a)
	}
	if n, _ := m.c.Database(m.db).Collection("orders").CountDocuments(ctx, bson.D{}); n != 3 {
		t.Fatalf("documents went without a confirm: %d", n)
	}

	owner := openMongo(t, m, svc, tunnel.LevelOwner, dir, 0)
	if a := owner.run(`db.orders.createIndex({ n: 1 })`); a.Type != "result" {
		t.Fatalf("createIndex at owner: %+v", a)
	}
	if a := owner.run(`db.runCommand({ createUser: "sneaky", pwd: "x", roles: ["dbOwner"] })`); a.Type != "error" {
		t.Fatalf("owner may not make users: %+v", a)
	}
	if a := owner.run(`db.runCommand({ dropUser: "argus-ana_sari-read" })`); a.Type != "error" || !strings.Contains(a.Error, "Argus manages") {
		t.Fatalf("an argus- user is refused by the agent: %+v", a)
	}

	dba := openMongo(t, m, svc, tunnel.LevelDBA, dir, 0)
	if a := dba.ask(tunnel.StoreRequest{Op: "run", Command: `db.orders.deleteMany({})`, Confirm: "cache-01"}); a.Type != "result" || a.Tag != "delete 3" {
		t.Fatalf("confirmed deleteMany at dba: %+v", a)
	}

	for _, h := range []*storeHarness{read, write, owner, dba} {
		h.close()
	}
	users := m.users(t)
	for _, u := range users {
		if tunnel.IsStoreUser(u) {
			t.Fatalf("user %s is still there after its session closed", u)
		}
	}
	if len(users) != 1 || users[0] != "report" {
		t.Fatalf("someone else's user was touched: %v", users)
	}
}

func TestMongoDBBrowserAndLimits(t *testing.T) {
	m := startMongo(t)
	ctx := context.Background()
	var many []any
	for i := 0; i < 30; i++ {
		many = append(many, bson.D{{Key: "n", Value: i}})
	}
	if _, err := m.c.Database(m.db).Collection("events").InsertMany(ctx, many); err != nil {
		t.Fatal(err)
	}
	if err := m.c.Database(m.db).CreateCollection(ctx, "empty"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	svc := MongoDB(StoreConfig{Verifier: &tunnel.GrantVerifier{Host: "cache-01"}, RecordDir: dir})
	h := openMongo(t, m, svc, tunnel.LevelRead, dir, 10)
	defer h.close()

	a := h.ask(tunnel.StoreRequest{Op: "scan"})
	if a.Type != "result" || a.Total != 2 {
		t.Fatalf("scan: %+v", a)
	}
	counts := map[string]int64{}
	for _, k := range a.Keys {
		counts[k.Name] = k.Count
	}
	if counts["events"] != 30 {
		t.Fatalf("scan counts: %+v", a.Keys)
	}
	v := h.ask(tunnel.StoreRequest{Op: "value", Key: "events", Count: 200})
	if v.Type != "result" || len(docsOf(t, v)) != 10 || v.Total != 30 || !v.Truncated {
		t.Fatalf("value is held to the entry's limit: %+v", v)
	}
	if !h.recorded("stmt", `db.getCollection("events").find({}).limit(10)`) {
		t.Fatal("reading a collection is not recorded")
	}
	f := h.run(`db.events.find({}).sort({ n: -1 })`)
	if got := docsOf(t, f); len(got) != 10 || !f.Truncated || got[0]["n"] != float64(29) {
		t.Fatalf("find past the limit: %+v", f)
	}
	ag := h.run(`db.events.aggregate([{ $match: { n: { $gte: 5 } } }])`)
	if len(docsOf(t, ag)) != 10 || !ag.Truncated {
		t.Fatalf("aggregate past the limit: %+v", ag)
	}
	if c := h.run(`db.events.countDocuments({ n: { $lt: 4 } })`); c.Tag != "count 4" {
		t.Fatalf("count: %+v", c)
	}
	if bad := h.run(`db.events.find({ $where: function() { return true } })`); bad.Type != "error" {
		t.Fatalf("JavaScript is not read: %+v", bad)
	}

	res := discoverMongo(ctx, tunnel.GrantDiscover{Engine: tunnel.EngineMongoDB, Host: "127.0.0.1", Port: m.port, ConfigPath: m.admin})
	found := false
	for _, d := range res.Databases {
		found = found || d.Name == m.db
		if tunnel.IsMongoSystemDB(d.Name) {
			t.Fatalf("discover listed %s", d.Name)
		}
	}
	if !res.OK || !found {
		t.Fatalf("discover: %+v", res)
	}
}

func TestMongoDBRefusesARemoteHost(t *testing.T) {
	svc := MongoDB(StoreConfig{Verifier: &tunnel.GrantVerifier{Host: "cache-01"}, RecordDir: t.TempDir()})
	_, err := openStoreSession(t, svc, tunnel.MongoDBService, tunnel.GrantDB{ID: "m1", Engine: tunnel.EngineMongoDB,
		Name: "shop", Host: "mongo.internal", Port: 27017, Level: tunnel.LevelRead}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "must be on this server") {
		t.Fatalf("a remote host: %v", err)
	}
	_, err = openStoreSession(t, svc, tunnel.MongoDBService, tunnel.GrantDB{ID: "m1", Engine: tunnel.EngineMongoDB,
		Name: "admin", Host: "127.0.0.1", Port: 27017, Level: tunnel.LevelRead}, t.TempDir())
	if err == nil {
		t.Fatal("the admin database was opened")
	}
}

func TestReadMongoAdmin(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "mongodb.conf")
	if err := os.WriteFile(file, []byte("# Argus's MongoDB admin\nuser = argus-admin\npassword: \"s3cret = yes\"\nauthSource=admin\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := readMongoAdmin(file)
	if err != nil || a.User != "argus-admin" || a.Password != "s3cret = yes" || a.AuthSource != "admin" {
		t.Fatalf("read: %+v %v", a, err)
	}
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readMongoAdmin(file); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("a file others can read: %v", err)
	}
	if _, err := readMongoAdmin(filepath.Join(dir, "missing.conf")); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("a missing file: %v", err)
	}
	if _, err := readMongoAdmin("/etc/passwd"); err == nil {
		t.Fatal("a file that is not a .conf was read")
	}
	if got := mongoURI("/tmp/mongodb-27017.sock", 27017); got != "mongodb://%2Ftmp%2Fmongodb-27017.sock/?directConnection=true" {
		t.Fatalf("socket uri: %s", got)
	}
	if !mongoVersionOK("8.0.4") || !mongoVersionOK("5.0.0") || mongoVersionOK("4.4.29") {
		t.Fatal("version check")
	}
}
