package tunnel

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

/*
SQL Server replication for one database, the work SSMS's Always On,
Replication and Log Shipping nodes do: the availability group the database
is in and how far behind each replica is; its transactional and snapshot
publications, their articles and subscriptions; and the log shipping it
takes part in.

Like the DBA operations (mssqldba.go) these are fixed operations: the text
built here is what is recorded, shown and approved, and the control plane
and the agent compute the same text. The text never holds a password. Where
SQL Server on Linux needs one (a replication agent signs in with SQL
authentication there), the text holds a placeholder the agent fills in as it
runs the statement: {admin login} and {admin password} are the admin file's
login, {subscriber password} is the one the DBA typed for another server's
subscriber. A placeholder left unfilled is a syntax error, never a value.

The reach is this database. A publication, article or subscription is this
database's; an availability group change adds or removes this database, or
fails over the group this database is in, from the replica this session is
on; a log shipping job is the one that ships this database. Setting this
server up as its own distributor is the one server-wide change, and it always
waits for approval.
*/

// SQL Server replication operation kinds.
const (
	// AdminMSReplication reads the database's replication: availability
	// group, publications, log shipping, and what the server has set up.
	AdminMSReplication = "ms.replication"

	AdminMSAGAdd      = "ms.ag.add"
	AdminMSAGRemove   = "ms.ag.remove"
	AdminMSAGSuspend  = "ms.ag.suspend"
	AdminMSAGResume   = "ms.ag.resume"
	AdminMSAGFailover = "ms.ag.failover"

	AdminMSReplDistributor = "ms.repl.distributor"
	AdminMSReplPublish     = "ms.repl.publish"
	AdminMSReplPubAdd      = "ms.repl.pub.add"
	AdminMSReplPubDrop     = "ms.repl.pub.drop"
	AdminMSReplArticleAdd  = "ms.repl.article.add"
	AdminMSReplArticleDrop = "ms.repl.article.drop"
	AdminMSReplSubAdd      = "ms.repl.sub.add"
	AdminMSReplSubDrop     = "ms.repl.sub.drop"
	AdminMSReplSnapshot    = "ms.repl.snapshot"

	AdminMSLSBackup  = "ms.ls.backup"
	AdminMSLSCopy    = "ms.ls.copy"
	AdminMSLSRestore = "ms.ls.restore"
)

// The placeholders the agent fills in; see above.
const (
	MSAdminLogin    = "{admin login}"
	MSAdminPassword = "{admin password}"
	MSSubPassword   = "{subscriber password}"
)

// MSDistributionDB is the distribution database Argus makes.
const MSDistributionDB = "distribution"

// MSSQLReplSpec says what a replication operation works on. Each kind reads
// only the fields it needs.
type MSSQLReplSpec struct {
	// Group is the availability group.
	Group string `json:"group,omitempty"`
	// Force fails over with possible data loss, for a group whose replicas
	// are not synchronized or that has no cluster.
	Force bool `json:"force,omitempty"`
	// Enabled turns publishing on or off, for publish.
	Enabled *bool `json:"enabled,omitempty"`
	// Publication is the publication; Kind is transactional (the default)
	// or snapshot, for pub.add.
	Publication string `json:"publication,omitempty"`
	Kind        string `json:"kind,omitempty"`
	// Schema and Table are an article's table.
	Schema string `json:"schema,omitempty"`
	Table  string `json:"table,omitempty"`
	// Article is the article to drop; its table's name by default.
	Article string `json:"article,omitempty"`
	// Subscriber is the server a subscription delivers to, empty for this
	// one; SubscriberDB is the database there.
	Subscriber   string `json:"subscriber,omitempty"`
	SubscriberDB string `json:"subscriberDb,omitempty"`
	// Login and Password sign the distribution agent in at another
	// server's subscriber. For this server the admin file's login is used.
	Login    string `json:"login,omitempty"`
	Password string `json:"password,omitempty"`
	// WorkingDir is the snapshot folder, for distributor.
	WorkingDir string `json:"workingDir,omitempty"`
}

// MSSQLReplKind says whether kind is a SQL Server replication operation.
func MSSQLReplKind(kind string) bool {
	switch kind {
	case AdminMSReplication, AdminMSAGAdd, AdminMSAGRemove, AdminMSAGSuspend, AdminMSAGResume, AdminMSAGFailover,
		AdminMSReplDistributor, AdminMSReplPublish, AdminMSReplPubAdd, AdminMSReplPubDrop, AdminMSReplArticleAdd,
		AdminMSReplArticleDrop, AdminMSReplSubAdd, AdminMSReplSubDrop, AdminMSReplSnapshot,
		AdminMSLSBackup, AdminMSLSCopy, AdminMSLSRestore:
		return true
	}
	return false
}

// reMSServer is a server name a subscription names: host, host\instance or
// host,port.
var reMSServer = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,126}([\\][A-Za-z0-9_$-]{1,16})?(,[0-9]{1,5})?$`)

// reMSDir is a snapshot folder: an absolute path, Linux or Windows.
var reMSDir = regexp.MustCompile(`^(/|[A-Za-z]:\\)[^'\x00-\x1f]{0,240}$`)

// MSSQLReplSQL is a replication operation's statements, joined by ";\n" as
// one batch; empty for the read. database is the session's database.
func MSSQLReplSQL(op *PGAdminOp, database string) (string, error) {
	if op == nil {
		return "", errors.New("no operation")
	}
	r := op.MSRepl
	if r == nil {
		r = &MSSQLReplSpec{}
	}
	db, err := mssqlName("the database", database)
	if err != nil {
		return "", err
	}
	// The agent fills in {placeholders}; no name or path may carry one.
	for _, v := range []string{database, r.Group, r.Publication, r.Schema, r.Table, r.Article, r.Subscriber, r.SubscriberDB, r.Login, r.WorkingDir} {
		if strings.ContainsAny(v, "{}") {
			return "", errors.New("names here cannot contain { or }")
		}
	}
	in := db + ".sys."
	lit := MSSQLString
	pub := func() (string, error) {
		if _, err := mssqlName("the publication", r.Publication); err != nil {
			return "", err
		}
		return "@publication = " + lit(r.Publication), nil
	}
	join := func(s ...string) string { return strings.Join(s, ";\n") }

	switch op.Kind {
	case AdminMSReplication:
		return "", nil

	case AdminMSAGAdd, AdminMSAGRemove, AdminMSAGFailover:
		g, err := mssqlName("the availability group", r.Group)
		if err != nil {
			return "", err
		}
		switch op.Kind {
		case AdminMSAGAdd:
			return "ALTER AVAILABILITY GROUP " + g + " ADD DATABASE " + db, nil
		case AdminMSAGRemove:
			return "ALTER AVAILABILITY GROUP " + g + " REMOVE DATABASE " + db, nil
		}
		if r.Force {
			return "ALTER AVAILABILITY GROUP " + g + " FORCE_FAILOVER_ALLOW_DATA_LOSS", nil
		}
		return "ALTER AVAILABILITY GROUP " + g + " FAILOVER", nil
	case AdminMSAGSuspend:
		return "ALTER DATABASE " + db + " SET HADR SUSPEND", nil
	case AdminMSAGResume:
		return "ALTER DATABASE " + db + " SET HADR RESUME", nil

	case AdminMSReplDistributor:
		if !reMSDir.MatchString(r.WorkingDir) {
			return "", errors.New("the snapshot folder is an absolute path on the server")
		}
		dist := lit(MSDistributionDB)
		return join(
			"DECLARE @s sysname = @@SERVERNAME",
			"EXEC master.sys.xp_create_subdir "+lit(r.WorkingDir),
			"EXEC master.sys.sp_adddistributor @distributor = @s",
			"EXEC master.sys.sp_adddistributiondb @database = "+dist+", @security_mode = 0, @login = "+MSAdminLogin+", @password = "+MSAdminPassword,
			"EXEC master.sys.sp_adddistpublisher @publisher = @s, @distribution_db = "+dist+", @security_mode = 0, @login = "+MSAdminLogin+
				", @password = "+MSAdminPassword+", @working_directory = "+lit(r.WorkingDir)+", @trusted = N'false'",
		), nil

	case AdminMSReplPublish:
		if r.Enabled == nil {
			return "", errors.New("turn publishing on, or off?")
		}
		v := "false"
		if *r.Enabled {
			v = "true"
		}
		return "EXEC master.sys.sp_replicationdboption @dbname = " + lit(database) + ", @optname = N'publish', @value = N'" + v + "'", nil

	case AdminMSReplPubAdd:
		p, err := pub()
		if err != nil {
			return "", err
		}
		freq, sync := "N'continuous'", "N'concurrent'"
		switch strings.ToLower(r.Kind) {
		case "", "transactional":
		case "snapshot":
			freq, sync = "N'snapshot'", "N'native'"
		default:
			return "", errors.New("a publication is transactional or snapshot")
		}
		return join(
			"EXEC "+in+"sp_addpublication "+p+", @status = N'active', @repl_freq = "+freq+", @sync_method = "+sync+
				", @independent_agent = N'true', @immediate_sync = N'true', @allow_push = N'true', @allow_pull = N'true'",
			"EXEC "+in+"sp_addpublication_snapshot "+p+", @frequency_type = 1, @publisher_security_mode = 0, @publisher_login = "+
				MSAdminLogin+", @publisher_password = "+MSAdminPassword,
		), nil

	case AdminMSReplPubDrop:
		p, err := pub()
		if err != nil {
			return "", err
		}
		return join(
			"EXEC "+in+"sp_dropsubscription "+p+", @article = N'all', @subscriber = N'all'",
			"EXEC "+in+"sp_droppublication "+p,
		), nil

	case AdminMSReplArticleAdd:
		p, err := pub()
		if err != nil {
			return "", err
		}
		if _, err := mssqlName("the schema", r.Schema); err != nil {
			return "", err
		}
		if _, err := mssqlName("the table", r.Table); err != nil {
			return "", err
		}
		art := r.Article
		if art == "" {
			art = r.Table
		}
		if _, err := mssqlName("the article", art); err != nil {
			return "", err
		}
		return "EXEC " + in + "sp_addarticle " + p + ", @article = " + lit(art) + ", @source_owner = " + lit(r.Schema) +
			", @source_object = " + lit(r.Table) + ", @destination_owner = " + lit(r.Schema) + ", @destination_table = " + lit(r.Table) +
			", @force_invalidate_snapshot = 1", nil

	case AdminMSReplArticleDrop:
		p, err := pub()
		if err != nil {
			return "", err
		}
		if _, err := mssqlName("the article", r.Article); err != nil {
			return "", err
		}
		return "EXEC " + in + "sp_droparticle " + p + ", @article = " + lit(r.Article) + ", @force_invalidate_snapshot = 1", nil

	case AdminMSReplSubAdd, AdminMSReplSubDrop:
		p, err := pub()
		if err != nil {
			return "", err
		}
		if _, err := mssqlName("the subscriber's database", r.SubscriberDB); err != nil {
			return "", err
		}
		local := r.Subscriber == ""
		sub := "@s"
		var decl []string
		if local {
			if strings.EqualFold(r.SubscriberDB, database) {
				return "", errors.New("a database cannot subscribe to itself")
			}
			if IsMSSQLSystemDB(r.SubscriberDB) {
				return "", errors.New("a system database cannot subscribe")
			}
			decl = []string{"DECLARE @s sysname = @@SERVERNAME"}
		} else {
			if !reMSServer.MatchString(r.Subscriber) {
				return "", fmt.Errorf("%q is not a server name: host, host\\instance or host,port", r.Subscriber)
			}
			sub = lit(r.Subscriber)
		}
		dest := lit(r.SubscriberDB)
		if op.Kind == AdminMSReplSubDrop {
			return join(append(decl, "EXEC "+in+"sp_dropsubscription "+p+", @article = N'all', @subscriber = "+sub+", @destination_db = "+dest)...), nil
		}
		login, pw := MSAdminLogin, MSAdminPassword
		if !local {
			if _, err := mssqlName("the subscriber's login", r.Login); err != nil {
				return "", err
			}
			if r.Password == "" {
				return "", errors.New("the subscriber's login needs its password")
			}
			login, pw = lit(r.Login), MSSubPassword
		}
		return join(append(decl,
			"EXEC "+in+"sp_addsubscription "+p+", @subscriber = "+sub+", @destination_db = "+dest+
				", @subscription_type = N'Push', @sync_type = N'automatic', @article = N'all', @update_mode = N'read only', @subscriber_type = 0",
			"EXEC "+in+"sp_addpushsubscription_agent "+p+", @subscriber = "+sub+", @subscriber_db = "+dest+
				", @subscriber_security_mode = 0, @subscriber_login = "+login+", @subscriber_password = "+pw+", @frequency_type = 64",
		)...), nil

	case AdminMSReplSnapshot:
		p, err := pub()
		if err != nil {
			return "", err
		}
		return "EXEC " + in + "sp_startpublication_snapshot " + p, nil

	case AdminMSLSBackup:
		return msLSJob("backup_job_id", "log_shipping_primary_databases", "primary_database", database), nil
	case AdminMSLSCopy:
		return msLSJob("copy_job_id", "log_shipping_secondary s JOIN msdb.dbo.log_shipping_secondary_databases d ON d.secondary_id = s.secondary_id",
			"d.secondary_database", database), nil
	case AdminMSLSRestore:
		return msLSJob("restore_job_id", "log_shipping_secondary s JOIN msdb.dbo.log_shipping_secondary_databases d ON d.secondary_id = s.secondary_id",
			"d.secondary_database", database), nil
	}
	return "", fmt.Errorf("%s is not a SQL Server operation", op.Kind)
}

// msLSJob starts the log shipping job that ships database.
func msLSJob(col, from, where, database string) string {
	return "DECLARE @j uniqueidentifier = (SELECT TOP (1) " + col + " FROM msdb.dbo." + from + " WHERE " + where + " = " +
		MSSQLString(database) + ");\nEXEC msdb.dbo.sp_start_job @job_id = @j"
}

// MSSQLReplAlwaysApproval says op waits for approval wherever the
// database is: it ends replication others depend on, moves the primary, or
// changes the server.
func MSSQLReplAlwaysApproval(op *PGAdminOp) bool {
	if op == nil || op.Preview {
		return false
	}
	switch op.Kind {
	case AdminMSAGRemove, AdminMSAGFailover, AdminMSReplDistributor, AdminMSReplPubDrop:
		return true
	case AdminMSReplPublish:
		return op.MSRepl != nil && op.MSRepl.Enabled != nil && !*op.MSRepl.Enabled
	}
	return false
}

// msReplTextAlwaysApproval is MSSQLReplAlwaysApproval read from the text.
func msReplTextAlwaysApproval(s string) bool {
	switch {
	case strings.HasPrefix(s, "ALTER AVAILABILITY GROUP ") && (strings.Contains(s, " REMOVE DATABASE ") ||
		strings.HasSuffix(s, " FAILOVER") || strings.HasSuffix(s, " FORCE_FAILOVER_ALLOW_DATA_LOSS")):
		return true
	case strings.HasPrefix(s, "EXEC master.sys.sp_adddistributor "):
		return true
	case strings.HasPrefix(s, "EXEC ") && strings.Contains(s, ".sys.sp_droppublication "):
		return true
	case strings.HasPrefix(s, "EXEC master.sys.sp_replicationdboption ") && strings.HasSuffix(s, "@optname = N'publish', @value = N'false'"):
		return true
	}
	return false
}
