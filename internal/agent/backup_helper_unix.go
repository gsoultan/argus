//go:build !windows

package agent

import (
	"context"
	"errors"
	"io"
)

// helperTool runs `argus-agent pg-backup` as the admin account.
func helperTool(cfg BackupConfig) func(context.Context, PGBackupRequest, io.Reader, io.Writer, io.Writer) error {
	return func(ctx context.Context, req PGBackupRequest, stdin io.Reader, stdout, stderr io.Writer) error {
		user := cfg.AdminUser
		if user == "" {
			user = "postgres"
		}
		acct, err := lookupAccount(user)
		if err != nil {
			return errors.New("this server has no " + user + " account to run the backup as")
		}
		return runAs(ctx, helperRun{acct: acct, helper: cfg.Helper, spawn: cfg.Spawn, env: cfg.HelperEnv,
			args: []string{"pg-backup", EncodePGBackupRequest(req)}, stdin: stdin, stdout: stdout,
			stderr: stderr, actor: "backup"})
	}
}
