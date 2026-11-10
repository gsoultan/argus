package agent

import (
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func extJSON(t *testing.T, d bson.D) string {
	t.Helper()
	b, err := bson.MarshalExtJSON(d, false, false)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseMongoLine(t *testing.T) {
	for _, c := range []struct{ line, want string }{
		{`db.orders.find({ status: "paid" }).sort({ paid_at: -1 }).limit(20)`,
			`{"find":"orders","filter":{"status":"paid"},"sort":{"paid_at":-1},"limit":20}`},
		{`db.orders.find()`, `{"find":"orders","filter":{}}`},
		{`db.orders.findOne({_id: ObjectId("65a1b2c3d4e5f60718293a4b")}, {email: 1});`,
			`{"find":"orders","filter":{"_id":{"$oid":"65a1b2c3d4e5f60718293a4b"}},"projection":{"email":1},"limit":1}`},
		{`db.getCollection("order items").countDocuments({ 'total.cents': { $gt: 1000 } })`,
			`{"count":"order items","query":{"total.cents":{"$gt":1000}}}`},
		{`db["order items"].estimatedDocumentCount()`, `{"count":"order items"}`},
		{`db.orders.find({email: /@example\.id$/i}).count()`,
			`{"count":"orders","query":{"email":{"$regularExpression":{"pattern":"@example\\.id$","options":"i"}}}}`},
		{`db.orders.aggregate([{ $match: { status: "paid" } }, { $group: { _id: "$carrier", n: { $sum: 1 } } }])`,
			`{"aggregate":"orders","pipeline":[{"$match":{"status":"paid"}},{"$group":{"_id":"$carrier","n":{"$sum":1}}}],"cursor":{}}`},
		{`db.orders.insertOne({ n: NumberLong("9007199254740993"), d: NumberDecimal("1.10"), at: ISODate("2026-11-06T05:58:11Z"), ok: true, x: null, })`,
			`{"insert":"orders","documents":[{"n":9007199254740993,"d":{"$numberDecimal":"1.10"},"at":{"$date":"2026-11-06T05:58:11Z"},"ok":true,"x":null}]}`},
		{`db.orders.updateMany({ status: "on_hold" }, { $set: { status: "paid" } }, { upsert: false, w: 0 })`,
			`{"update":"orders","updates":[{"q":{"status":"on_hold"},"u":{"$set":{"status":"paid"}},"multi":true,"upsert":false}]}`},
		{`db.orders.deleteOne({ n: 1.5e3 })`, `{"delete":"orders","deletes":[{"q":{"n":1500.0},"limit":1}]}`},
		{`db.orders.createIndex({ email: 1, paid_at: -1 }, { unique: true })`,
			`{"createIndexes":"orders","indexes":[{"key":{"email":1,"paid_at":-1},"name":"email_1_paid_at_-1","unique":true}]}`},
		{`db.runCommand({ dbStats: 1 })`, `{"dbStats":1}`},
		{`{ "listCollections": 1, nameOnly: true } // just names`, `{"listCollections":1,"nameOnly":true}`},
		{`show collections`, `{"listCollections":1,"nameOnly":true}`},
		{`db.orders.find({ k: 'it\'s', u: UUID("0e5a4f5c-63e1-4bd4-9a54-2b67e0c6f2a1") })`,
			`{"find":"orders","filter":{"k":"it's","u":{"$binary":{"base64":"DlpPXGPhS9SaVCtn4MbyoQ==","subType":"04"}}}}`},
	} {
		call, err := parseMongoLine(c.line)
		if err != nil {
			t.Errorf("%s: %v", c.line, err)
			continue
		}
		if got := extJSON(t, call.Cmd); got != c.want {
			t.Errorf("%s\n got %s\nwant %s", c.line, got, c.want)
		}
	}
}

func TestParseMongoLineRefusesWhatIsNotALiteral(t *testing.T) {
	for _, line := range []string{
		``,
		`db.orders.find({ $where: function() { return true } })`,
		`var x = 1`,
		`db.orders.find({ a: x })`,
		`db.orders.find({ a: 1 }`,
		`db.orders.find({ a: "open }) `,
		`db.adminCommand({ shutdown: 1 })`,
		`db.orders.mapReduce()`,
		`db.orders.find({}).limit(-1)`,
		`db.orders.find({}) db.orders.drop()`,
		`db.orders.findOne({}).limit(2)`,
		`db.orders.find(` + strings.Repeat("[", 200) + strings.Repeat("]", 200) + `)`,
	} {
		if _, err := parseMongoLine(line); err == nil {
			t.Errorf("%q should not parse", line)
		}
	}
}

func TestClassifyMongo(t *testing.T) {
	for _, c := range []struct {
		line           string
		confirm, refus bool
	}{
		{`db.orders.drop()`, true, false},
		{`db.dropDatabase()`, true, false},
		{`db.runCommand({ dropDatabase: 1 })`, true, false},
		{`db.orders.deleteMany({})`, true, false},
		{`db.orders.deleteMany({ status: "void" })`, false, false},
		{`db.orders.deleteOne({})`, false, false},
		{`db.orders.updateMany({}, { $set: { a: 1 } })`, true, false},
		{`db.orders.updateOne({}, { $set: { a: 1 } })`, false, false},
		{`db.runCommand({ dropUser: "argus-ana_sari-read" })`, false, true},
		{`db.runCommand({ grantRolesToUser: "argus-ana_sari-read", roles: ["dbOwner"] })`, false, true},
		{`db.runCommand({ dropAllUsersFromDatabase: 1 })`, false, true},
		{`db.runCommand({ createUser: "report", pwd: "x", roles: [] })`, false, false},
		{`db.orders.find({})`, false, false},
	} {
		call, err := parseMongoLine(c.line)
		if err != nil {
			t.Fatalf("%s: %v", c.line, err)
		}
		if (call.Confirm != "") != c.confirm || (call.Refuse != "") != c.refus {
			t.Errorf("%s: confirm %q refuse %q", c.line, call.Confirm, call.Refuse)
		}
	}
}

// The lines the console's New collection and New index forms write
// (web/src/components/mongo/MongoNew.tsx) parse to the commands meant.
func TestParseMongoLineFromTheNewForms(t *testing.T) {
	for line, want := range map[string]string{
		`db.createCollection("events", { timeseries: { timeField: "ts", metaField: "sensor", granularity: "minutes" }, expireAfterSeconds: 2592000 })`: `{"create": "events","timeseries": {"timeField": "ts","metaField": "sensor","granularity": "minutes"},"expireAfterSeconds": {"$numberInt":"2592000"}}`,
		`db.createCollection("log", { capped: true, size: 104857600, max: 1000 })`:                                                                     `{"create": "log","capped": true,"size": {"$numberInt":"104857600"},"max": {"$numberInt":"1000"}}`,
		`db.createCollection("users", { validator: {"$jsonSchema":{"required":["email"]}} })`:                                                          `{"create": "users","validator": {"$jsonSchema": {"required": ["email"]}}}`,
		`db.getCollection("order items").createIndex({ "email": 1, "at": -1 }, { unique: true })`:                                                      `{"createIndexes": "order items","indexes": [{"key": {"email": {"$numberInt":"1"},"at": {"$numberInt":"-1"}},"name": "email_1_at_-1","unique": true}]}`,
		`db.orders.createIndex({ "body": "text" })`:                                                                                                    `{"createIndexes": "orders","indexes": [{"key": {"body": "text"},"name": "body_text"}]}`,
	} {
		c, err := parseMongoLine(line)
		if err != nil {
			t.Fatalf("%s: %v", line, err)
		}
		got, _ := bson.MarshalExtJSON(c.Cmd, true, false)
		if norm(string(got)) != norm(want) {
			t.Errorf("%s:\n got %s\nwant %s", line, got, want)
		}
	}
}

func norm(s string) string { return strings.NewReplacer(" ", "", "\n", "").Replace(s) }
