package agent

import "context"

// helperPoolerConsole on Windows: Argus finds PgBouncer on Linux only.
func helperPoolerConsole(PostgresConfig) poolerConsoleFunc {
	return func(context.Context, string, PoolerConsoleRequest, string) PoolerConsoleResult {
		return PoolerConsoleResult{Error: "reading PgBouncer is not supported on Windows"}
	}
}
