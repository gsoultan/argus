//go:build !windows

package agent

import (
	"bytes"
	"context"
	"encoding/json"
)

// helperPoolerConsole runs `argus-agent pgbouncer-stats` as account.
func helperPoolerConsole(cfg PostgresConfig) poolerConsoleFunc {
	return func(ctx context.Context, account string, req PoolerConsoleRequest, actor string) PoolerConsoleResult {
		acct, err := lookupAccount(account)
		if err != nil {
			return PoolerConsoleResult{Error: err.Error()}
		}
		in, _ := json.Marshal(req)
		var out bytes.Buffer
		err = runAs(ctx, helperRun{acct: acct, helper: cfg.Helper, spawn: cfg.Spawn, env: cfg.HelperEnv,
			args: []string{"pgbouncer-stats"}, stdin: bytes.NewReader(in), stdout: &out, actor: actor})
		var res PoolerConsoleResult
		if jerr := json.Unmarshal(out.Bytes(), &res); jerr != nil {
			if err == nil {
				err = jerr
			}
			return PoolerConsoleResult{Error: "the helper's answer could not be read: " + err.Error()}
		}
		return res
	}
}
