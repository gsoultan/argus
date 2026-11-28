package agent

import (
	"context"

	"github.com/gsoultan/caduceus"
	"github.com/gsoultan/caduceus/wire"
)

// pgOptions are how the agent's Postgres connections run their statements.
//
// Every result column comes back in text, as Postgres prints it: the console
// shows the person exactly that, and the admin and catalog queries read their
// few values as strings. Nothing is kept on the server between statements, so
// a session through PgBouncer in transaction mode works, and a person's DDL
// cannot leave a cached statement describing a table that has changed.
var pgOptions = caduceus.Options{
	StatementCacheCapacity: -1,
	ResultFormat:           func(uint32) int16 { return 0 },
}

// pgConnect opens a connection with pgOptions.
func pgConnect(ctx context.Context, connString string) (*caduceus.Conn, error) {
	return caduceus.Connect(ctx, connString, pgOptions)
}

// pgConnectConfig opens a connection from config with pgOptions.
func pgConnectConfig(ctx context.Context, config *wire.Config) (*caduceus.Conn, error) {
	return caduceus.ConnectConfig(ctx, config, pgOptions)
}

// pgResult is one statement's whole result, read at once: the catalog and
// admin queries' few rows, in text. Rows are copies, safe to keep; a NULL is a
// nil slice.
type pgResult struct {
	Fields []wire.Field
	Rows   [][][]byte
	Tag    caduceus.CommandTag
	Err    error
}

// pgRead runs one statement with its parameters bound as text, the server
// inferring their types, and reads every row. A nil parameter is NULL.
func pgRead(ctx context.Context, conn *caduceus.Conn, sql string, params [][]byte) *pgResult {
	args := make([]any, len(params))
	for i, p := range params {
		if p != nil {
			args[i] = string(p)
		}
	}
	res := &pgResult{}
	rows, err := conn.Query(ctx, sql, args...)
	if err != nil {
		res.Err = err
		return res
	}
	defer rows.Close()
	res.Fields = rows.Fields()
	for rows.Next() {
		raw := rows.RawValues()
		row := make([][]byte, len(raw))
		for i, v := range raw {
			if v != nil {
				row[i] = append([]byte{}, v...)
			}
		}
		res.Rows = append(res.Rows, row)
	}
	rows.Close()
	res.Tag, res.Err = rows.Tag(), rows.Err()
	return res
}

// pgExec runs sql over the simple protocol, which allows several statements
// separated by semicolons, and returns the last one's result. Nothing is
// bound: sql must not carry a value from outside the program.
func pgExec(ctx context.Context, conn *caduceus.Conn, sql string) (*pgResult, error) {
	r, err := conn.Wire().Exec(ctx, sql)
	if err != nil {
		return nil, err
	}
	return &pgResult{Fields: r.Fields, Rows: r.Rows, Tag: caduceus.CommandTag(r.Tag)}, nil
}

// pgStream is a statement's result read a row at a time, for the console,
// whose results can be far larger than anyone keeps in memory. Values alias
// the connection's buffer until the next NextRow.
type pgStream struct {
	rows *caduceus.Rows
	err  error
}

// pgQuery runs one statement with its parameters bound as text and returns
// its rows as they arrive. A nil parameter is NULL.
func pgQuery(ctx context.Context, conn *caduceus.Conn, sql string, params [][]byte) *pgStream {
	args := make([]any, len(params))
	for i, p := range params {
		if p != nil {
			args[i] = string(p)
		}
	}
	rows, err := conn.Query(ctx, sql, args...)
	return &pgStream{rows: rows, err: err}
}

// NextRow advances to the next row.
func (s *pgStream) NextRow() bool { return s.rows != nil && s.rows.Next() }

// Values are the current row's columns in text, nil for NULL.
func (s *pgStream) Values() [][]byte { return s.rows.RawValues() }

// FieldDescriptions describes the result's columns.
func (s *pgStream) FieldDescriptions() []wire.Field {
	if s.rows == nil {
		return nil
	}
	return s.rows.Fields()
}

// Close reads what is left of the result and returns its command tag and
// the statement's error.
func (s *pgStream) Close() (caduceus.CommandTag, error) {
	if s.rows == nil {
		return "", s.err
	}
	s.rows.Close()
	return s.rows.Tag(), s.rows.Err()
}

// pgRun is pgExec for a caller that needs only to know it worked.
func pgRun(ctx context.Context, conn *caduceus.Conn, sql string) error {
	_, err := conn.Wire().Exec(ctx, sql)
	return err
}
