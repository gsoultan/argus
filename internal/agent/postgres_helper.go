//go:build !windows

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
)

// helperAdmin runs provisioning in `argus-agent pg-admin`, as the account
// peer authentication maps to a superuser. The agent never holds a Postgres
// password of its own.
func helperAdmin(cfg PostgresConfig) func(context.Context, PGAdminRequest) PGAdminResult {
	return func(ctx context.Context, req PGAdminRequest) PGAdminResult {
		user := cfg.AdminUser
		if user == "" {
			user = "postgres"
		}
		acct, err := lookupAccount(user)
		if err != nil {
			return PGAdminResult{Error: "this server has no " + user + " account to set up database roles as"}
		}
		in, _ := json.Marshal(req)
		var out bytes.Buffer
		err = runAs(ctx, helperRun{acct: acct, helper: cfg.Helper, spawn: cfg.Spawn, env: cfg.HelperEnv,
			args: []string{"pg-admin"}, stdin: bytes.NewReader(in), stdout: &out, actor: req.Owner})
		var res PGAdminResult
		if jerr := json.Unmarshal(out.Bytes(), &res); jerr != nil {
			if err == nil {
				err = errors.New("the helper's answer could not be read")
			}
			return PGAdminResult{Error: "could not set up the database role: " + err.Error()}
		}
		return res
	}
}
