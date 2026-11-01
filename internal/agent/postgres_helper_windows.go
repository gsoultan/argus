package agent

import "context"

// helperAdmin on Windows: Postgres there has no peer authentication, so the
// agent cannot set up roles without a stored password, which it will not hold.
func helperAdmin(PostgresConfig) func(context.Context, PGAdminRequest) PGAdminResult {
	return func(context.Context, PGAdminRequest) PGAdminResult {
		return PGAdminResult{Error: "database access is not available on Windows servers yet"}
	}
}
