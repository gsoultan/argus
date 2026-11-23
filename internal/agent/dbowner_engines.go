package agent

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
The owner of a new or existing database on SQL Server, MySQL and MongoDB:
making the user, giving it the database, a new password, and the sign-in
check. Postgres's is in postgres_create.go and dbowner.go. Every statement
comes from tunnel.OwnerPlan, with the password's hash or literal where the
shown text says [redacted].
*/

func step(ok bool, text string, err error, msg func(error) string) tunnel.CreateStep {
	if err != nil {
		return tunnel.CreateStep{Text: text, Error: msg(err)}
	}
	return tunnel.CreateStep{Text: text, OK: ok}
}

// ---------------------------------------------------------------- SQL Server

func mssqlLoginInfo(ctx context.Context, db *sql.DB, login string) (exists, sysadmin bool, err error) {
	var n, sa int
	err = db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(IS_SRVROLEMEMBER('sysadmin', name)), 0)
		FROM sys.server_principals WHERE name = @p1 AND type IN ('S', 'U', 'G')`, login).Scan(&n, &sa)
	return n > 0, sa == 1, err
}

// mssqlOwnerBefore makes a new login, or checks an existing one, before the
// database. made says a login was made that must go if the database is not.
func mssqlOwnerBefore(ctx context.Context, admin *mssqlAdmin, c tunnel.GrantCreate, pw string, res *tunnel.CreateResult) (made, ok bool) {
	o := c.Owner
	if c.OwnerName() == "" {
		return false, true
	}
	exists, sa, err := mssqlLoginInfo(ctx, admin.db, o.Name)
	if err != nil {
		res.Error = "SQL Server did not answer: " + mssqlMessage(err)
		return false, false
	}
	if o.Kind == tunnel.OwnerExisting {
		switch {
		case !exists:
			res.Error = "there is no login named " + o.Name + " on this server"
			return false, false
		case sa:
			res.Error = o.Name + " is a sysadmin and already has every database; pick a login of the app's own"
			return false, false
		}
		res.Steps = append(res.Steps, tunnel.CreateStep{Text: "The owner " + o.Name + " is a login on this server", OK: true})
		return false, true
	}
	if exists {
		res.Error = "a login named " + o.Name + " is already on this server; pick it under an existing login, or another name"
		return false, false
	}
	hash, err := tunnel.MSSQLPasswordHash(pw)
	if err != nil {
		res.Error = err.Error()
		return false, false
	}
	pre, _ := tunnel.OwnerPlan(c, hash)
	shown, _ := tunnel.OwnerPlan(c, tunnel.PasswordRedacted)
	for _, q := range pre {
		if _, err := admin.db.ExecContext(ctx, q); err != nil {
			res.Error = mssqlMessage(err)
			return false, false
		}
	}
	res.Statements = append(res.Statements, shown...)
	res.Steps = append(res.Steps, tunnel.CreateStep{Text: "Made the login " + o.Name + ", which can sign in", OK: true})
	return true, true
}

func mssqlUndoLogin(ctx context.Context, admin *mssqlAdmin, login string) tunnel.CreateStep {
	_, err := admin.db.ExecContext(ctx, "DROP LOGIN "+tunnel.MSSQLIdent(login))
	return step(true, "Removed the login "+login+" again, since the database was not made", err, mssqlMessage)
}

// mssqlOwnerAfter gives the new database to its owner and checks the owner
// signs in. It reports whether everything worked; what was made stays.
func mssqlOwnerAfter(ctx context.Context, admin *mssqlAdmin, c tunnel.GrantCreate, pw string, res *tunnel.CreateResult) bool {
	if c.OwnerName() == "" {
		return true
	}
	_, post := tunnel.OwnerPlan(c, tunnel.PasswordRedacted)
	for _, q := range post {
		if _, err := admin.db.ExecContext(ctx, q); err != nil {
			res.Steps = append(res.Steps, tunnel.CreateStep{Text: "Give " + c.Name + " to " + c.Owner.Name, Error: mssqlMessage(err)})
			res.Error = "the database was made, but not given to " + c.Owner.Name + ": " + mssqlMessage(err)
			return false
		}
		res.Statements = append(res.Statements, q)
	}
	res.Steps = append(res.Steps, tunnel.CreateStep{Text: "Gave " + c.Name + " to " + c.Owner.Name + " (its dbo)", OK: true})
	mssqlSignInStep(ctx, c, pw, res)
	return true
}

func mssqlSignInStep(ctx context.Context, c tunnel.GrantCreate, pw string, res *tunnel.CreateResult) {
	if pw == "" {
		return
	}
	db, err := sql.Open("sqlserver", mssqlDSN(c.Host, c.Port, c.Owner.Name, pw, c.Name))
	if err == nil {
		err = db.PingContext(ctx)
		_ = db.Close()
	}
	res.Steps = append(res.Steps, step(true, c.Owner.Name+" signs in and lands in "+c.Name, err, mssqlMessage))
}

func mssqlOwnerUsers(ctx context.Context, db *sql.DB) []tunnel.OwnerUser {
	rows, err := db.QueryContext(ctx, `SELECT p.name, IS_SRVROLEMEMBER('sysadmin', p.name),
		  (SELECT COUNT(*) FROM sys.databases d WHERE d.owner_sid = p.sid AND d.database_id > 4)
		FROM sys.server_principals p
		WHERE p.type IN ('S', 'U') AND p.is_disabled = 0 AND p.name NOT LIKE '##%'
		  AND p.name NOT LIKE 'NT %' AND p.name NOT LIKE 'argus%'
		ORDER BY 2, 1`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []tunnel.OwnerUser
	for rows.Next() {
		var name string
		var sa sql.NullInt64
		var owns int
		if rows.Scan(&name, &sa, &owns) != nil || len(out) == 500 {
			continue
		}
		u := tunnel.OwnerUser{Name: name, All: sa.Int64 == 1}
		switch {
		case u.All:
			u.Reach = "sysadmin: every database"
		case owns > 0:
			u.Reach = fmt.Sprintf("owns %d database(s)", owns)
		}
		out = append(out, u)
	}
	return out
}

// ownMSSQL changes an existing database's owner, or gives the owning
// login a new password.
func ownMSSQL(ctx context.Context, c tunnel.GrantCreate, pw string) tunnel.CreateResult {
	admin, err := mssqlAdminConn(ctx, c.Host, c.Port, c.ConfigPath)
	if err != nil {
		return tunnel.CreateResult{Error: err.Error()}
	}
	defer admin.close()
	res := tunnel.CreateResult{Version: admin.version}
	var owner sql.NullString
	if err := admin.db.QueryRowContext(ctx, `SELECT SUSER_SNAME(owner_sid) FROM sys.databases WHERE name = @p1`, c.Name).Scan(&owner); err != nil {
		res.Error = "this server has no database named " + c.Name
		return res
	}
	if c.Op == tunnel.CreateOpSetPassword {
		o := c.Owner
		_, sa, err := mssqlLoginInfo(ctx, admin.db, o.Name)
		switch {
		case err != nil:
			res.Error = mssqlMessage(err)
		case !strings.EqualFold(owner.String, o.Name):
			res.Error = o.Name + " does not own " + c.Name + "; only the owner's password is set here"
		case sa:
			res.Error = o.Name + " is a sysadmin; its password is not changed from here"
		}
		if res.Error != "" {
			return res
		}
		hash, err := tunnel.MSSQLPasswordHash(pw)
		if err == nil {
			_, err = admin.db.ExecContext(ctx, tunnel.SetPasswordPlanWith(c, hash)[0])
		}
		if err != nil {
			res.Error = mssqlMessage(err)
			return res
		}
		res.Statements = tunnel.SetPasswordPlan(c)
		res.Steps = append(res.Steps, tunnel.CreateStep{Text: "Gave " + o.Name + " a new password", OK: true})
		mssqlSignInStep(ctx, c, pw, &res)
		res.OK = true
		return res
	}
	if c.OwnerName() == "" {
		// Back to the admin login Argus made it with.
		q := "ALTER AUTHORIZATION ON DATABASE::" + tunnel.MSSQLIdent(c.Name) + " TO " + tunnel.MSSQLIdent(admin.user)
		if admin.user == "" {
			q = "ALTER AUTHORIZATION ON DATABASE::" + tunnel.MSSQLIdent(c.Name) + " TO [sa]"
		}
		if _, err := admin.db.ExecContext(ctx, q); err != nil {
			res.Error = mssqlMessage(err)
			return res
		}
		res.Statements = []string{q}
		res.Steps = append(res.Steps, tunnel.CreateStep{Text: "Gave " + c.Name + " back to Argus's admin login", OK: true})
		res.OK = true
		return res
	}
	made, ok := mssqlOwnerBefore(ctx, admin, c, pw, &res)
	if !ok {
		return res
	}
	if !mssqlOwnerAfter(ctx, admin, c, pw, &res) {
		if made {
			res.Steps = append(res.Steps, mssqlUndoLogin(ctx, admin, c.Owner.Name))
		}
		return res
	}
	res.OK = true
	return res
}

// ---------------------------------------------------------------- MySQL

func mysqlUserInfo(ctx context.Context, admin *mysqlAdmin, o *tunnel.CreateOwner) (exists, super bool, err error) {
	var n int
	var s sql.NullString
	err = admin.db.QueryRowContext(ctx, `SELECT COUNT(*), MAX(Super_priv) FROM mysql.user WHERE User = ? AND Host = ?`, o.Name, o.From).Scan(&n, &s)
	return n > 0, s.String == "Y", err
}

func mysqlOwnerBefore(ctx context.Context, admin *mysqlAdmin, c tunnel.GrantCreate, pw string, res *tunnel.CreateResult) (made, ok bool) {
	o := c.Owner
	if c.OwnerName() == "" {
		return false, true
	}
	who := "'" + o.Name + "'@'" + o.From + "'"
	exists, super, err := mysqlUserInfo(ctx, admin, o)
	if err != nil {
		res.Error = "MySQL did not answer: " + mysqlMessage(err)
		return false, false
	}
	if o.Kind == tunnel.OwnerExisting {
		switch {
		case !exists:
			res.Error = "there is no user " + who + " on this server"
			return false, false
		case super:
			res.Error = who + " already has every database; pick a user of the app's own"
			return false, false
		}
		res.Steps = append(res.Steps, tunnel.CreateStep{Text: "The owner " + who + " is a user on this server", OK: true})
		return false, true
	}
	if exists {
		res.Error = "the user " + who + " is already on this server; pick it under an existing user, or another name"
		return false, false
	}
	pre, _ := tunnel.OwnerPlan(c, tunnel.MySQLString(pw))
	shown, _ := tunnel.OwnerPlan(c, tunnel.PasswordRedacted)
	for _, q := range pre {
		if _, err := admin.db.ExecContext(ctx, q); err != nil {
			res.Error = mysqlMessage(err)
			return false, false
		}
	}
	res.Statements = append(res.Statements, shown...)
	res.Steps = append(res.Steps, tunnel.CreateStep{Text: "Made the user " + who + ", which can sign in", OK: true})
	return true, true
}

func mysqlUndoUser(ctx context.Context, admin *mysqlAdmin, o *tunnel.CreateOwner) tunnel.CreateStep {
	_, err := admin.db.ExecContext(ctx, "DROP USER IF EXISTS "+tunnel.MySQLString(o.Name)+"@"+tunnel.MySQLString(o.From))
	return step(true, "Removed the user "+o.Name+" again, since the database was not made", err, mysqlMessage)
}

func mysqlOwnerAfter(ctx context.Context, admin *mysqlAdmin, c tunnel.GrantCreate, pw string, res *tunnel.CreateResult) bool {
	if c.OwnerName() == "" {
		return true
	}
	_, post := tunnel.OwnerPlan(c, tunnel.PasswordRedacted)
	for _, q := range post {
		if _, err := admin.db.ExecContext(ctx, q); err != nil {
			res.Steps = append(res.Steps, tunnel.CreateStep{Text: "Give " + c.Name + " to " + c.Owner.Name, Error: mysqlMessage(err)})
			res.Error = "the database was made, but " + c.Owner.Name + " was not given it: " + mysqlMessage(err)
			return false
		}
		res.Statements = append(res.Statements, q)
	}
	res.Steps = append(res.Steps, tunnel.CreateStep{Text: "Gave " + c.Owner.Name + " every privilege on " + c.Name, OK: true})
	mysqlSignInStep(ctx, admin, c, pw, res)
	return true
}

// mysqlSignInStep signs in as the new user the way its host part allows
// from this server: over the socket for localhost, else 127.0.0.1.
func mysqlSignInStep(ctx context.Context, admin *mysqlAdmin, c tunnel.GrantCreate, pw string, res *tunnel.CreateResult) {
	if pw == "" {
		return
	}
	o := c.Owner
	w := mysqlWay{host: "127.0.0.1", port: c.Port}
	switch o.From {
	case "localhost":
		if admin.way.socket == "" {
			return
		}
		w = mysqlWay{socket: admin.way.socket}
	case "%", "127.0.0.1", "127.%", "127.0.0.%":
	default:
		res.Steps = append(res.Steps, tunnel.CreateStep{OK: true,
			Text: o.Name + " signs in only from " + o.From + ", so the agent did not try it from this server"})
		return
	}
	db, err := sql.Open("mysql", w.dsn(o.Name, pw, c.Name))
	if err == nil {
		err = db.PingContext(ctx)
		_ = db.Close()
	}
	res.Steps = append(res.Steps, step(true, o.Name+" signs in to "+c.Name, err, mysqlMessage))
}

func mysqlOwnerUsers(ctx context.Context, admin *mysqlAdmin) []tunnel.OwnerUser {
	rows, err := admin.db.QueryContext(ctx, `SELECT u.User, u.Host, u.Super_priv = 'Y',
		  (SELECT GROUP_CONCAT(DISTINCT d.Db ORDER BY d.Db SEPARATOR ', ') FROM mysql.db d WHERE d.User = u.User AND d.Host = u.Host)
		FROM mysql.user u
		WHERE u.User <> '' AND u.User NOT LIKE 'argus%'
		  AND u.User NOT IN ('mysql.sys', 'mysql.session', 'mysql.infoschema', 'mariadb.sys', 'debian-sys-maint')
		ORDER BY 3, 1, 2 LIMIT 500`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []tunnel.OwnerUser
	for rows.Next() {
		var u tunnel.OwnerUser
		var dbs sql.NullString
		if rows.Scan(&u.Name, &u.From, &u.All, &dbs) != nil {
			continue
		}
		switch {
		case u.All:
			u.Reach = "every database already"
		case dbs.String != "":
			u.Reach = "on " + strings.ReplaceAll(dbs.String, `\_`, "_")
		}
		out = append(out, u)
	}
	return out
}

func ownMySQL(ctx context.Context, sockets []string, c tunnel.GrantCreate, pw string) tunnel.CreateResult {
	admin, err := adminConn(ctx, sockets, tunnel.GrantDB{Host: c.Host, Port: c.Port, ConfigPath: c.ConfigPath})
	if err != nil {
		return tunnel.CreateResult{Error: err.Error()}
	}
	defer admin.close()
	res := tunnel.CreateResult{Version: admin.version}
	var n int
	if err := admin.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?`, c.Name).Scan(&n); err != nil || n == 0 {
		res.Error = "this server has no database named " + c.Name
		return res
	}
	if c.Op == tunnel.CreateOpSetPassword {
		o := c.Owner
		exists, super, err := mysqlUserInfo(ctx, admin, o)
		if err == nil && exists {
			err = admin.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mysql.db WHERE User = ? AND Host = ? AND Db IN (?, ?) AND Create_priv = 'Y'`,
				o.Name, o.From, c.Name, strings.NewReplacer(`_`, `\_`, `%`, `\%`).Replace(c.Name)).Scan(&n)
		}
		switch {
		case err != nil:
			res.Error = mysqlMessage(err)
		case !exists:
			res.Error = "there is no user '" + o.Name + "'@'" + o.From + "' on this server"
		case super:
			res.Error = o.Name + " has every database; its password is not changed from here"
		case n == 0:
			res.Error = o.Name + " does not own " + c.Name + "; only the owner's password is set here"
		}
		if res.Error != "" {
			return res
		}
		if _, err := admin.db.ExecContext(ctx, tunnel.SetPasswordPlanWith(c, tunnel.MySQLString(pw))[0]); err != nil {
			res.Error = mysqlMessage(err)
			return res
		}
		res.Statements = tunnel.SetPasswordPlan(c)
		res.Steps = append(res.Steps, tunnel.CreateStep{Text: "Gave " + o.Name + " a new password", OK: true})
		mysqlSignInStep(ctx, admin, c, pw, &res)
		res.OK = true
		return res
	}
	made, ok := mysqlOwnerBefore(ctx, admin, c, pw, &res)
	if !ok {
		return res
	}
	if !mysqlOwnerAfter(ctx, admin, c, pw, &res) {
		if made {
			res.Steps = append(res.Steps, mysqlUndoUser(ctx, admin, c.Owner))
		}
		return res
	}
	res.OK = true
	return res
}

// ---------------------------------------------------------------- MongoDB

type mongoUser struct {
	User  string `bson:"user"`
	DB    string `bson:"db"`
	Roles []struct {
		Role string `bson:"role"`
		DB   string `bson:"db"`
	} `bson:"roles"`
}

func mongoFindUser(ctx context.Context, admin *mongo.Client, db, user string) (*mongoUser, error) {
	var out struct {
		Users []mongoUser `bson:"users"`
	}
	if err := admin.Database(db).RunCommand(ctx, bson.D{{Key: "usersInfo", Value: user}}).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Users) == 0 {
		return nil, nil
	}
	return &out.Users[0], nil
}

var mongoAllRoles = map[string]bool{"root": true, "dbOwner@admin": true, "readWriteAnyDatabase": true,
	"dbAdminAnyDatabase": true, "userAdminAnyDatabase": true, "__system": true}

func (u mongoUser) all() bool {
	for _, r := range u.Roles {
		if mongoAllRoles[r.Role] || (r.Role == "dbOwner" && r.DB == "admin") {
			return true
		}
	}
	return false
}

func (u mongoUser) owns(db string) bool {
	for _, r := range u.Roles {
		if r.Role == "dbOwner" && r.DB == db {
			return true
		}
	}
	return false
}

// mongoOwnerBefore checks the owner before anything is made: a new user's
// name is free in the new database, an existing user is there.
func mongoOwnerBefore(ctx context.Context, admin *mongo.Client, c tunnel.GrantCreate, res *tunnel.CreateResult) bool {
	o := c.Owner
	if c.OwnerName() == "" {
		return true
	}
	db := o.From
	if o.Kind == tunnel.OwnerNew {
		db = c.Name
	}
	u, err := mongoFindUser(ctx, admin, db, o.Name)
	switch {
	case err != nil:
		res.Error = "MongoDB would not list its users: " + err.Error()
		return false
	case o.Kind == tunnel.OwnerNew && u != nil:
		res.Error = "a user named " + o.Name + " is already in " + db
		return false
	case o.Kind == tunnel.OwnerExisting && u == nil:
		res.Error = "there is no user " + o.Name + " in " + db
		return false
	case o.Kind == tunnel.OwnerExisting && u.all():
		res.Error = o.Name + " already has every database; pick a user of the app's own"
		return false
	}
	return true
}

func mongoOwnerAfter(ctx context.Context, admin *mongo.Client, c tunnel.GrantCreate, pw string, res *tunnel.CreateResult) bool {
	o := c.Owner
	if c.OwnerName() == "" {
		return true
	}
	role := bson.A{bson.D{{Key: "role", Value: "dbOwner"}, {Key: "db", Value: c.Name}}}
	var err error
	if o.Kind == tunnel.OwnerNew {
		err = admin.Database(c.Name).RunCommand(ctx, bson.D{{Key: "createUser", Value: o.Name}, {Key: "pwd", Value: pw},
			{Key: "roles", Value: role}}).Err()
	} else {
		err = admin.Database(o.From).RunCommand(ctx, bson.D{{Key: "grantRolesToUser", Value: o.Name}, {Key: "roles", Value: role}}).Err()
	}
	if err != nil {
		res.Steps = append(res.Steps, tunnel.CreateStep{Text: "Give " + c.Name + " to " + o.Name, Error: err.Error()})
		res.Error = "the database was made, but " + o.Name + " was not given it: " + err.Error()
		return false
	}
	_, post := tunnel.OwnerPlan(c, tunnel.PasswordRedacted)
	res.Statements = append(res.Statements, post...)
	if o.Kind == tunnel.OwnerNew {
		res.Steps = append(res.Steps, tunnel.CreateStep{Text: "Made the user " + o.Name + " in " + c.Name + ", with dbOwner there", OK: true})
	} else {
		res.Steps = append(res.Steps, tunnel.CreateStep{Text: "Gave " + o.Name + " dbOwner on " + c.Name, OK: true})
	}
	mongoSignInStep(ctx, c, pw, c.Name, res)
	return true
}

func mongoSignInStep(ctx context.Context, c tunnel.GrantCreate, pw, source string, res *tunnel.CreateResult) {
	if pw == "" {
		return
	}
	cl, err := mongoClient(c.Host, c.Port, &options.Credential{Username: c.Owner.Name, Password: pw, AuthSource: source}, 10e9)
	if err == nil {
		err = cl.Ping(ctx, nil)
		_ = cl.Disconnect(context.WithoutCancel(ctx))
	}
	res.Steps = append(res.Steps, step(true, c.Owner.Name+" signs in (authSource "+source+")", err, func(e error) string { return e.Error() }))
}

func mongoOwnerUsers(ctx context.Context, admin *mongo.Client) []tunnel.OwnerUser {
	var out struct {
		Users []mongoUser `bson:"users"`
	}
	if err := admin.Database("admin").RunCommand(ctx, bson.D{{Key: "usersInfo", Value: bson.D{{Key: "forAllDBs", Value: true}}}}).Decode(&out); err != nil {
		return nil
	}
	var users []tunnel.OwnerUser
	for _, u := range out.Users {
		if strings.HasPrefix(strings.ToLower(u.User), "argus") || len(users) == 500 {
			continue
		}
		ou := tunnel.OwnerUser{Name: u.User, From: u.DB, All: u.all()}
		if ou.All {
			ou.Reach = "every database already"
		} else {
			var dbs []string
			for _, r := range u.Roles {
				dbs = append(dbs, r.Role+" on "+r.DB)
			}
			ou.Reach = strings.Join(dbs, ", ")
		}
		users = append(users, ou)
	}
	return users
}

func ownMongo(ctx context.Context, c tunnel.GrantCreate, pw string) tunnel.CreateResult {
	s := &mongoSession{host: c.Host, port: c.Port, file: c.ConfigPath}
	if s.file == "" {
		s.file = tunnel.MongoAdminFile
	}
	admin, err := s.adminClient(ctx)
	if err != nil {
		return tunnel.CreateResult{Error: err.Error(), Version: s.version}
	}
	defer func() { _ = admin.Disconnect(context.WithoutCancel(ctx)) }()
	res := tunnel.CreateResult{Version: s.version}
	if c.Op == tunnel.CreateOpSetPassword {
		o := c.Owner
		u, err := mongoFindUser(ctx, admin, o.From, o.Name)
		switch {
		case err != nil:
			res.Error = err.Error()
		case u == nil:
			res.Error = "there is no user " + o.Name + " in " + o.From
		case u.all():
			res.Error = o.Name + " has every database; its password is not changed from here"
		case !u.owns(c.Name):
			res.Error = o.Name + " does not own " + c.Name + "; only the owner's password is set here"
		}
		if res.Error != "" {
			return res
		}
		if err := admin.Database(o.From).RunCommand(ctx, bson.D{{Key: "updateUser", Value: o.Name}, {Key: "pwd", Value: pw}}).Err(); err != nil {
			res.Error = "MongoDB refused: " + err.Error()
			return res
		}
		res.Statements = tunnel.SetPasswordPlan(c)
		res.Steps = append(res.Steps, tunnel.CreateStep{Text: "Gave " + o.Name + " a new password", OK: true})
		mongoSignInStep(ctx, c, pw, o.From, &res)
		res.OK = true
		return res
	}
	if !mongoOwnerBefore(ctx, admin, c, &res) {
		return res
	}
	res.OK = mongoOwnerAfter(ctx, admin, c, pw, &res)
	return res
}
