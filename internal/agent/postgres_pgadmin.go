package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// PGAdminMain is `argus-agent pg-admin`: one request on stdin, one result on
// stdout. It signs in over the socket with peer authentication, so it only
// works as an account Postgres trusts that way.
func PGAdminMain(ctx context.Context, stdin io.Reader, stdout io.Writer) int {
	var req PGAdminRequest
	if err := json.NewDecoder(io.LimitReader(stdin, 1<<16)).Decode(&req); err != nil {
		_ = json.NewEncoder(stdout).Encode(PGAdminResult{Error: "bad request: " + err.Error()})
		return 2
	}
	res := pgAdminDial(ctx, req)
	_ = json.NewEncoder(stdout).Encode(res)
	if !res.OK {
		return 1
	}
	return 0
}

func pgAdminDial(ctx context.Context, req PGAdminRequest) PGAdminResult {
	if !strings.HasPrefix(req.Socket, "/") || strings.Contains(req.Socket, "..") {
		return PGAdminResult{Error: "the socket directory must be an absolute path"}
	}
	user := req.AdminUser
	if user == "" {
		user = "postgres"
	}
	cfg, err := pgconn.ParseConfig("")
	if err != nil {
		return PGAdminResult{Error: err.Error()}
	}
	cfg.Host, cfg.Port, cfg.User, cfg.Database = req.Socket, uint16(req.Port), user, req.Database
	cfg.TLSConfig, cfg.Fallbacks, cfg.Password = nil, nil, ""
	cfg.RuntimeParams = map[string]string{"application_name": "argus-admin"}
	conn, err := pgconn.ConnectConfig(ctx, cfg)
	if err != nil {
		return PGAdminResult{Error: fmt.Sprintf("could not sign in to Postgres as %s over %s (port %s): %v",
			user, req.Socket, strconv.Itoa(req.Port), err)}
	}
	defer conn.Close(context.WithoutCancel(ctx))
	return RunPGAdmin(ctx, conn, req)
}

// DSNAdmin provisions with a superuser connection string instead of the
// helper. For tests, and for a server whose Postgres has no peer login.
func DSNAdmin(dsn func(database string) string) func(context.Context, PGAdminRequest) PGAdminResult {
	return func(ctx context.Context, req PGAdminRequest) PGAdminResult {
		conn, err := pgconn.Connect(ctx, dsn(req.Database))
		if err != nil {
			return PGAdminResult{Error: err.Error()}
		}
		defer conn.Close(context.WithoutCancel(ctx))
		return RunPGAdmin(ctx, conn, req)
	}
}
