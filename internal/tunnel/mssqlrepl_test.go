package tunnel

import (
	"strings"
	"testing"
)

func TestMSSQLReplText(t *testing.T) {
	yes, no := true, false
	for _, c := range []struct {
		kind string
		r    *MSSQLReplSpec
		want string
	}{
		{AdminMSReplication, nil, ""},
		{AdminMSAGAdd, &MSSQLReplSpec{Group: "ag]1"}, "ALTER AVAILABILITY GROUP [ag]]1] ADD DATABASE [shop]"},
		{AdminMSAGRemove, &MSSQLReplSpec{Group: "ag1"}, "ALTER AVAILABILITY GROUP [ag1] REMOVE DATABASE [shop]"},
		{AdminMSAGFailover, &MSSQLReplSpec{Group: "ag1"}, "ALTER AVAILABILITY GROUP [ag1] FAILOVER"},
		{AdminMSAGFailover, &MSSQLReplSpec{Group: "ag1", Force: true}, "ALTER AVAILABILITY GROUP [ag1] FORCE_FAILOVER_ALLOW_DATA_LOSS"},
		{AdminMSAGSuspend, nil, "ALTER DATABASE [shop] SET HADR SUSPEND"},
		{AdminMSReplPublish, &MSSQLReplSpec{Enabled: &yes}, "EXEC master.sys.sp_replicationdboption @dbname = N'shop', @optname = N'publish', @value = N'true'"},
		{AdminMSReplSnapshot, &MSSQLReplSpec{Publication: "p'1"}, "EXEC [shop].sys.sp_startpublication_snapshot @publication = N'p''1'"},
		{AdminMSReplArticleAdd, &MSSQLReplSpec{Publication: "p", Schema: "dbo", Table: "orders"},
			"EXEC [shop].sys.sp_addarticle @publication = N'p', @article = N'orders', @source_owner = N'dbo', @source_object = N'orders', @destination_owner = N'dbo', @destination_table = N'orders', @force_invalidate_snapshot = 1"},
		{AdminMSReplSubDrop, &MSSQLReplSpec{Publication: "p", SubscriberDB: "copy"},
			"DECLARE @s sysname = @@SERVERNAME;\nEXEC [shop].sys.sp_dropsubscription @publication = N'p', @article = N'all', @subscriber = @s, @destination_db = N'copy'"},
		{AdminMSLSBackup, nil, "DECLARE @j uniqueidentifier = (SELECT TOP (1) backup_job_id FROM msdb.dbo.log_shipping_primary_databases WHERE primary_database = N'shop');\nEXEC msdb.dbo.sp_start_job @job_id = @j"},
	} {
		got, err := MSSQLAdminText(&PGAdminOp{Kind: c.kind, MSRepl: c.r}, "shop")
		if err != nil || got != c.want {
			t.Errorf("%s: got %q (%v)\nwant %q", c.kind, got, err, c.want)
		}
	}
	_ = no
	// A remote subscriber's password never reaches the text.
	op := &PGAdminOp{Kind: AdminMSReplSubAdd, MSRepl: &MSSQLReplSpec{Publication: "p", Subscriber: "reports-01,1433",
		SubscriberDB: "copy", Login: "repl", Password: "s3cret!"}}
	text, err := MSSQLAdminText(op, "shop")
	if err != nil || strings.Contains(text, "s3cret") || !strings.Contains(text, "@subscriber_password = "+MSSubPassword) ||
		!strings.Contains(text, "@subscriber = N'reports-01,1433'") {
		t.Fatalf("remote subscription: %q %v", text, err)
	}
	if r := op.Redacted(); r.MSRepl.Password != PasswordRedacted || op.MSRepl.Password != "s3cret!" {
		t.Fatal("redacted")
	}
	// This server's subscriber signs in as the admin file's login.
	local, _ := MSSQLAdminText(&PGAdminOp{Kind: AdminMSReplSubAdd, MSRepl: &MSSQLReplSpec{Publication: "p", SubscriberDB: "copy"}}, "shop")
	if !strings.Contains(local, "@subscriber_login = "+MSAdminLogin+", @subscriber_password = "+MSAdminPassword) {
		t.Fatalf("local subscription: %q", local)
	}
	dist, _ := MSSQLAdminText(&PGAdminOp{Kind: AdminMSReplDistributor, MSRepl: &MSSQLReplSpec{WorkingDir: "/var/opt/mssql/data/ReplData"}}, "shop")
	if !strings.Contains(dist, "sp_adddistributor @distributor = @s") || strings.Count(dist, MSAdminPassword) != 2 {
		t.Fatalf("distributor: %q", dist)
	}
}

func TestMSSQLReplRefusals(t *testing.T) {
	if _, err := MSSQLReplSQL(&PGAdminOp{Kind: AdminMSReplSnapshot, MSRepl: &MSSQLReplSpec{Publication: "{admin password}"}}, "shop"); err == nil {
		t.Fatal("a placeholder in a name")
	}
	for _, c := range []struct {
		kind string
		r    *MSSQLReplSpec
		want string
	}{
		{AdminMSAGAdd, &MSSQLReplSpec{}, "the availability group is required"},
		{AdminMSReplPublish, &MSSQLReplSpec{}, "on, or off"},
		{AdminMSReplPubAdd, &MSSQLReplSpec{Publication: "p", Kind: "merge"}, "transactional or snapshot"},
		{AdminMSReplSubAdd, &MSSQLReplSpec{Publication: "p", SubscriberDB: "shop"}, "subscribe to itself"},
		{AdminMSReplSubAdd, &MSSQLReplSpec{Publication: "p", SubscriberDB: "msdb"}, "system database"},
		{AdminMSReplSubAdd, &MSSQLReplSpec{Publication: "p", SubscriberDB: "c", Subscriber: "x'; DROP"}, "not a server name"},
		{AdminMSReplSubAdd, &MSSQLReplSpec{Publication: "p", SubscriberDB: "c", Subscriber: "h", Login: "l"}, "needs its password"},
		{AdminMSReplDistributor, &MSSQLReplSpec{WorkingDir: "relative/x"}, "absolute path"},
		{AdminMSReplDistributor, &MSSQLReplSpec{WorkingDir: "/x'; DROP"}, "absolute path"},
	} {
		_, err := MSSQLReplSQL(&PGAdminOp{Kind: c.kind, MSRepl: c.r}, "shop")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %+v: %v, want %q", c.kind, c.r, err, c.want)
		}
	}
}

// The changes that always wait: the op and its text agree.
func TestMSSQLReplAlwaysWaits(t *testing.T) {
	yes, no := true, false
	for _, c := range []struct {
		kind string
		r    *MSSQLReplSpec
		want bool
	}{
		{AdminMSAGRemove, &MSSQLReplSpec{Group: "g"}, true},
		{AdminMSAGFailover, &MSSQLReplSpec{Group: "g"}, true},
		{AdminMSAGAdd, &MSSQLReplSpec{Group: "g"}, false},
		{AdminMSReplDistributor, &MSSQLReplSpec{WorkingDir: "/r"}, true},
		{AdminMSReplPubDrop, &MSSQLReplSpec{Publication: "p"}, true},
		{AdminMSReplPublish, &MSSQLReplSpec{Enabled: &no}, true},
		{AdminMSReplPublish, &MSSQLReplSpec{Enabled: &yes}, false},
		{AdminMSReplPubAdd, &MSSQLReplSpec{Publication: "p"}, false},
	} {
		op := &PGAdminOp{Kind: c.kind, MSRepl: c.r}
		text, err := MSSQLAdminText(op, "shop")
		if err != nil {
			t.Fatal(err)
		}
		if MSSQLAlwaysApproval(op) != c.want || MSSQLTextAlwaysApproval(text) != c.want {
			t.Errorf("%s: op %v, text %v, want %v", c.kind, MSSQLAlwaysApproval(op), MSSQLTextAlwaysApproval(text), c.want)
		}
	}
	if (&PGAdminOp{Kind: AdminMSReplication}).Mutates() || !MSSQLAdminKind(AdminMSReplSubAdd) {
		t.Fatal("kinds")
	}
}
