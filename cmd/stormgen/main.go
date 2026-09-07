// Command stormgen is this schema's storm tool.
//
//	ARGUS_DATABASE_URL=... go run ./cmd/stormgen generate internal/control/rgen \
//	  -raw-schema live -dsn "$ARGUS_DATABASE_URL"
//
// It is five lines because storm's commands are a library: they need to see
// this module's models, and a binary installed from storm's repository cannot.
// verify, lint and explain come with them, against THIS schema.
//
// The model in rmodel is a PROJECTION. internal/control/migrations is the
// source of truth for this schema and storm never applies DDL; `storm verify`
// is what keeps the two from drifting apart, and it is wired into
// scripts/check.sh.
package main

import (
	argusrmodel "github.com/gsoultan/argus/internal/control/rmodel"
	"github.com/gsoultan/storm/tool"
)

func main() { tool.Main(argusrmodel.All(), nil) }
