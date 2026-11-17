package tunnel

import "testing"

func TestGrantReplicaValidate(t *testing.T) {
	v := "SCRAM-SHA-256$4096:c2FsdA==$c3RvcmVk:c2VydmVy"
	good := []struct {
		job string
		r   GrantReplica
	}{
		{"replica-primary", GrantReplica{PGReplica: PGReplica{Name: "r1"}, Op: ReplicaCheck}},
		{"replica-primary", GrantReplica{PGReplica: PGReplica{Name: "r1", Verifier: v}, Op: ReplicaPrepare}},
		{"replica-primary", GrantReplica{PGReplica: PGReplica{Name: "r1", Address: "10.0.2.15"}, Op: ReplicaAllow}},
		{"replica-clone", GrantReplica{PGReplica: PGReplica{Name: "r1"}, PrimaryHost: "db-1.internal", PrimaryPort: 5432, PrimaryVersion: 16}},
	}
	for _, c := range good {
		if err := c.r.Validate(c.job); err != nil {
			t.Errorf("%+v: %v", c.r, err)
		}
	}
	bad := []struct {
		job string
		r   *GrantReplica
	}{
		{"replica-primary", nil},
		{"replica-primary", &GrantReplica{PGReplica: PGReplica{Name: "R1"}, Op: ReplicaCheck}},
		{"replica-primary", &GrantReplica{PGReplica: PGReplica{Name: "r1"}, Op: "promote"}},
		{"replica-primary", &GrantReplica{PGReplica: PGReplica{Name: "r1"}, Op: ReplicaPrepare}},
		{"replica-primary", &GrantReplica{PGReplica: PGReplica{Name: "r1"}, Op: ReplicaAllow}},
		{"replica-primary", &GrantReplica{PGReplica: PGReplica{Name: "r1", Address: "10.0.0.0/8"}, Op: ReplicaAllow}},
		{"replica-clone", &GrantReplica{PGReplica: PGReplica{Name: "r1"}, PrimaryHost: "-h x", PrimaryPort: 5432, PrimaryVersion: 16}},
		{"replica-clone", &GrantReplica{PGReplica: PGReplica{Name: "r1"}, PrimaryHost: "10.0.0.1", PrimaryVersion: 16}},
		{"replica-clone", &GrantReplica{PGReplica: PGReplica{Name: "r1"}, PrimaryHost: "10.0.0.1", PrimaryPort: 5432}},
	}
	for _, c := range bad {
		if err := c.r.Validate(c.job); err == nil {
			t.Errorf("accepted %+v", c.r)
		}
	}
}
