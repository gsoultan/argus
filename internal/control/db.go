package control

import (
	"context"

	"github.com/gsoultan/caduceus"
	"github.com/gsoultan/storm/v2/runtime"
	"github.com/gsoultan/storm/v2/runtime/caduceusdrv"
)

// rowScanner is what a scan helper reads one row from: a *caduceus.Row from
// QueryRow, or a *caduceus.Rows positioned on a row.
type rowScanner interface {
	Scan(dest ...any) error
}

// stormExec runs storm's generated queries on a connection of the store's
// pool, so they share its limits and its organization's search_path. The
// pool's connections are opened with caduceusdrv.Options, the result formats
// storm's scanners decode.
type stormExec struct{ pool *caduceus.Pool }

var _ runtime.Executor = stormExec{}

// with lends fn a connection of the pool for one call.
func (e stormExec) with(ctx context.Context, fn func(caduceusdrv.Conn) error) error {
	c, err := e.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer c.Release()
	return fn(caduceusdrv.Conn{C: c.Conn})
}

func (e stormExec) Query(ctx context.Context, sql string, args []any) (runtime.Rows, error) {
	c, err := e.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := caduceusdrv.Conn{C: c.Conn}.Query(ctx, sql, args)
	if err != nil {
		c.Release()
		return nil, err
	}
	return &releasingRows{Rows: rows, conn: c}, nil
}

func (e stormExec) Exec(ctx context.Context, sql string, args []any) (n int64, err error) {
	err = e.with(ctx, func(c caduceusdrv.Conn) error {
		n, err = c.Exec(ctx, sql, args)
		return err
	})
	return n, err
}

func (e stormExec) CopyFrom(ctx context.Context, table string, cols []string, src runtime.CopySource) (n int64, err error) {
	err = e.with(ctx, func(c caduceusdrv.Conn) error {
		n, err = c.CopyFrom(ctx, table, cols, src)
		return err
	})
	return n, err
}

func (e stormExec) Batch(ctx context.Context, ops []runtime.BatchOp, each func(int, runtime.Rows, int64, error) error) error {
	return e.with(ctx, func(c caduceusdrv.Conn) error { return c.Batch(ctx, ops, each) })
}

// releasingRows gives the connection back when storm closes the rows.
type releasingRows struct {
	runtime.Rows
	conn *caduceus.PoolConn
}

func (r *releasingRows) Close() {
	r.Rows.Close()
	r.conn.Release()
}
