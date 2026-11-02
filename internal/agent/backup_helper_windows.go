package agent

import (
	"context"
	"errors"
	"io"
)

// helperTool on Windows: like helperAdmin, there is no peer authentication to
// run pg_dump as the admin account without a stored password.
func helperTool(BackupConfig) func(context.Context, PGBackupRequest, io.Reader, io.Writer, io.Writer) error {
	return func(context.Context, PGBackupRequest, io.Reader, io.Writer, io.Writer) error {
		return errors.New("database backups are not available on Windows servers yet")
	}
}
