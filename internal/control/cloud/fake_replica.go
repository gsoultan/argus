package cloud

import (
	"context"
	"errors"
	"strings"
)

// CreateReplica adds the replica to the inventory as creating, and the
// primary lists it, as RDS would. Databases are read account-wide, so
// they are kept with the account-wide resources, by their own region.
func (f *Fake) CreateReplica(_ context.Context, a Account, actor Actor, s ReplicaSpec) (Result, error) {
	if err := f.storageCall(a, ActionReplicaCreate, s.Source+" "+s.Name, actor); err != nil {
		return Result{}, err
	}
	if err := s.Check(); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	arn := "arn:aws:rds:" + s.Region + ":123456789012:db:" + s.Name
	var src *Resource
	for i, r := range f.Resources[GlobalRegion] {
		if r.Kind != KindDatabase || r.Region != s.Region {
			continue
		}
		if r.Name == s.Name {
			return Result{}, errors.New("DB instance already exists")
		}
		if r.Name == s.Source {
			src = &f.Resources[GlobalRegion][i]
		}
	}
	if src == nil {
		return Result{}, errors.New("DBInstanceNotFound: " + s.Source)
	}
	tags := map[string]string{ReplicaTag: s.Source}
	for k, v := range s.Tags {
		tags[k] = v
	}
	d := map[string]string{"replica_of": s.Source, "deletion_protection": "yes", "backup_days": "0"}
	for _, k := range []string{"engine", "version", "port", "storage_gb", "storage_type", "encrypted", "public"} {
		if v := src.Details[k]; v != "" {
			d[k] = v
		}
	}
	reps := strings.Split(src.Details["replicas"], ",")
	if src.Details["replicas"] == "" {
		reps = nil
	}
	src.Details["replicas"] = strings.Join(append(reps, s.Name), ",")
	f.Resources[GlobalRegion] = append(f.Resources[GlobalRegion], Resource{Kind: KindDatabase, ID: arn, Name: s.Name, Region: s.Region,
		Zone: s.Zone, State: "creating", Size: s.Size, Tags: tags, Details: d})
	return Result{ResourceID: arn, RequestID: f.request()}, nil
}

// RemoveReplica refuses anything but a replica Argus tagged, as the manage
// role's policy would, and marks it deleting.
func (f *Fake) RemoveReplica(_ context.Context, a Account, actor Actor, region, name string) (Result, error) {
	if err := f.storageCall(a, ActionReplicaRemove, name, actor); err != nil {
		return Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, r := range f.Resources[GlobalRegion] {
		if r.Kind != KindDatabase || r.Region != region || r.Name != name {
			continue
		}
		if r.Tags[ReplicaTag] == "" {
			return Result{}, errors.New("AccessDenied: not authorized to perform rds:DeleteDBInstance on " + name)
		}
		f.Resources[GlobalRegion][i].State = "deleting"
		f.Resources[GlobalRegion][i].Details["deletion_protection"] = "no"
		return Result{ResourceID: r.ID, RequestID: f.request()}, nil
	}
	return Result{}, errors.New("DBInstanceNotFound: " + name)
}

var _ Replicas = (*Fake)(nil)
