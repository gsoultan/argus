package cloud

import (
	"context"
	"errors"
	"regexp"
	"strings"
)

/*
Read replicas of a managed database (RDS): a copy AWS keeps streaming from
the primary, that takes reads and never writes. Argus adds one with the
primary's class in the same region, deletion protection on, and tags that
say which primary and which change made it. Writes stay where they are:
Argus never promotes a replica or moves the application.

Removing one is a person's choice, asked each time, and only for a replica
Argus made: the manage role may delete only a database tagged as one.
*/

const (
	ActionReplicaCreate Action = "replica-create"
	ActionReplicaRemove Action = "replica-remove"
)

// ReplicaTag is the tag on every replica Argus makes, naming its primary.
// The manage role may only delete or change a database that carries it.
const ReplicaTag = "argus:replica-of"

// ReplicaSpec is one new read replica.
type ReplicaSpec struct {
	Region string
	// Source is the primary's identifier, and Name the replica's.
	Source string
	Name   string
	// Size is the database class: db.r6i.large.
	Size string
	// Zone is the Availability Zone, or "" for AWS's choice.
	Zone string
	Tags map[string]string
}

var reDBName = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9-]{0,62}$`)

// ValidDBName is whether s may name an RDS database instance: a letter,
// then letters, digits and single hyphens, at most 63, not ending in one.
func ValidDBName(s string) bool {
	return reDBName.MatchString(s) && !strings.Contains(s, "--") && !strings.HasSuffix(s, "-")
}

var reDBClass = regexp.MustCompile(`^db\.[a-z0-9-]+\.[a-z0-9]+$`)

// ValidDBClass is whether s looks like a database class.
func ValidDBClass(s string) bool { return reDBClass.MatchString(s) }

// Check says what is wrong with s, or nil.
func (s ReplicaSpec) Check() error {
	if !ValidDBName(s.Source) {
		return errors.New("name the database to copy")
	}
	if !ValidDBName(s.Name) {
		return errors.New("name the replica with a letter, then letters, digits and single hyphens, up to 63")
	}
	if strings.EqualFold(s.Name, s.Source) {
		return errors.New("give the replica its own name")
	}
	if !ValidDBClass(s.Size) {
		return errors.New("pick a database class, like db.r6i.large")
	}
	if s.Zone != "" && !strings.HasPrefix(s.Zone, s.Region) {
		return errors.New("pick a zone in " + s.Region)
	}
	return nil
}

// ReplicaEngines are the engines a read replica is offered for. Aurora
// keeps its readers in the cluster, which is a different call.
var ReplicaEngines = map[string]string{
	"postgres": "Postgres", "mysql": "MySQL", "mariadb": "MariaDB",
}

// Replicas is a provider that adds and removes database read replicas. It
// is optional.
type Replicas interface {
	// CreateReplica asks for the replica. The result's id is its ARN; AWS
	// takes some minutes to make it, and the inventory shows it creating
	// until then.
	CreateReplica(ctx context.Context, a Account, actor Actor, s ReplicaSpec) (Result, error)
	// RemoveReplica turns off deletion protection on a replica Argus made
	// and deletes it. A replica keeps no data of its own, so no final
	// snapshot is taken.
	RemoveReplica(ctx context.Context, a Account, actor Actor, region, name string) (Result, error)
}
