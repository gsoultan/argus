package agent

import (
	"log/slog"

	"github.com/gsoultan/argus/internal/tunnel"
)

// FilesService is the tunnel service name.
const FilesService = "files"

// FilesConfig is the files service.
type FilesConfig struct {
	Verifier *tunnel.GrantVerifier
	Log      *slog.Logger
	// Spawn is how each operation is started; see SpawnMode.
	Spawn SpawnMode
	// Helper is the program run for each operation, given "file-op" and its
	// arguments. Empty is this executable.
	Helper string
	// HelperEnv is added to the helper's environment. For tests.
	HelperEnv []string
}
