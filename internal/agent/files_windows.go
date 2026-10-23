package agent

import (
	"context"
	"log/slog"
)

// Files refuses on Windows: each operation would have to run as the account,
// and the S4U logon the terminal uses is not wired up for file-op yet.
// The console offers a terminal on Windows hosts; files come later.
func Files(cfg FilesConfig) TunnelService {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return func(ctx context.Context, call *Call) {
		log.Warn("tunnel: refused file transfer", "error", "not available on Windows")
		call.Refuse("file transfer is not available on Windows servers yet")
	}
}
