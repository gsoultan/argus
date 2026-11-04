package agent

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gsoultan/argus/internal/tunnel"
)

/*
The trash: what a DELETE, DROP or TRUNCATE in a session would have lost,
kept in the database itself for a while so it can be put back.

	DELETE           the rows it removed are kept, as JSON, for 30 days
	DROP TABLE       the table moves into argus_trash, renamed, for 7 days
	DROP SCHEMA      the schema is renamed argus_trash_<name>_<n>, for 7 days
	TRUNCATE         a copy of the rows moves into argus_trash, for 7 days

It lives in the database, not in the recording: a recording never holds a
customer's rows, and rows kept here stay behind the same grants and backups
as the rest of the database.

The schema and its functions are set up by the pg-admin helper (the server's
postgres account) when a session opens. No one's role can use the schema
directly. Each function is SECURITY DEFINER and checks, as the person
(session_user, which a SET ROLE does not change), that they could have done
the real thing themselves: owned the table to drop it, held TRUNCATE to empty
it, INSERT to put rows back. So the trash never lets anyone do more than
their grants let them, only undo it.

A DROP that something else depends on (a view, a foreign key from another
table, a partition's parent) is refused rather than half done, with what to
drop first; a real DROP … CASCADE would lose those silently, and the trash
could not give them back.
*/

// trashVersion is bumped when trashSQL changes, so an open sets the schema
// up again only when it has to.
const trashVersion = "argus trash v3"

// trashSchema is where kept things live.
const trashSchema = "argus_trash"

// keptRowsMax is the most rows a DELETE may remove while a copy is kept.
const keptRowsMax = 10000

// trashSQL sets the trash up in one database. It runs as the server's
// postgres account; every statement is idempotent.
const trashSQL = `
CREATE SCHEMA IF NOT EXISTS argus_trash;
REVOKE ALL ON SCHEMA argus_trash FROM PUBLIC;
CREATE SCHEMA IF NOT EXISTS argus_trash_fn;
REVOKE ALL ON SCHEMA argus_trash_fn FROM PUBLIC;

CREATE TABLE IF NOT EXISTS argus_trash.items (
  id bigserial PRIMARY KEY,
  kind text NOT NULL CHECK (kind IN ('rows', 'table', 'schema', 'empty')),
  source text NOT NULL,
  source_oid oid,
  kept text,
  parent text,
  bound text,
  acl text[],
  rows bigint,
  bytes bigint,
  by_role text NOT NULL DEFAULT session_user,
  at timestamptz NOT NULL DEFAULT now(),
  expires timestamptz NOT NULL,
  restored timestamptz,
  restored_by text
);
CREATE TABLE IF NOT EXISTS argus_trash.kept_rows (
  item bigint NOT NULL REFERENCES argus_trash.items ON DELETE CASCADE,
  row jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS kept_rows_item ON argus_trash.kept_rows (item);

-- What one item tells the session that made it.
CREATE OR REPLACE FUNCTION argus_trash_fn.out(it argus_trash.items,
  OUT id bigint, OUT kind text, OUT source text, OUT rows bigint, OUT expires text)
LANGUAGE sql IMMUTABLE AS $f$
  SELECT it.id, it.kind, it.source, it.rows,
    to_char(it.expires AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
$f$;

-- Names what depends on a table from outside it, or NULL when nothing does.
CREATE OR REPLACE FUNCTION argus_trash_fn.dependents(t regclass) RETURNS text
LANGUAGE sql STABLE SET search_path = pg_catalog, pg_temp AS $f$
  SELECT string_agg(d, ', ' ORDER BY d) FROM (
    SELECT DISTINCT 'view ' || v.oid::regclass::text AS d
      FROM pg_depend dep JOIN pg_rewrite rw ON rw.oid = dep.objid
      JOIN pg_class v ON v.oid = rw.ev_class
     WHERE dep.classid = 'pg_rewrite'::regclass AND dep.refobjid = t AND v.oid <> t
    UNION
    SELECT 'foreign key ' || quote_ident(c.conname) || ' on ' || c.conrelid::regclass::text
      FROM pg_constraint c WHERE c.contype = 'f' AND c.confrelid = t AND c.conrelid <> t
  ) x
$f$;

-- A DELETE's rows, kept for 30 days.
CREATE OR REPLACE FUNCTION argus_trash_fn.keep_rows(t regclass, deleted jsonb,
  OUT id bigint, OUT kind text, OUT source text, OUT rows bigint, OUT expires text)
RETURNS SETOF record
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $f$
DECLARE
  n bigint := coalesce(jsonb_array_length(deleted), 0);
  it argus_trash.items;
BEGIN
  IF n = 0 THEN RETURN; END IF;
  IF NOT has_table_privilege(session_user, t, 'DELETE') THEN
    RAISE EXCEPTION 'permission denied for table %', t USING ERRCODE = '42501';
  END IF;
  IF n > 10000 THEN
    RAISE EXCEPTION 'This DELETE removes % rows of %. Argus keeps a copy of at most 10,000, so nothing was deleted.', n, t
      USING ERRCODE = 'AR005',
            HINT = 'Delete in smaller batches, or empty the table with TRUNCATE, which keeps the whole table in the trash.';
  END IF;
  INSERT INTO argus_trash.items (kind, source, source_oid, rows, expires)
    VALUES ('rows', t::text, t, n, now() + interval '30 days') RETURNING * INTO it;
  INSERT INTO argus_trash.kept_rows (item, row) SELECT it.id, x FROM jsonb_array_elements(deleted) x;
  RETURN QUERY SELECT * FROM argus_trash_fn.out(it);
END
$f$;

-- DROP TABLE: the table moves into argus_trash for 7 days.
-- names are as typed, for messages; ts the same, resolved by the caller's
-- search_path (to_regclass in the calling statement), NULL where missing.
CREATE OR REPLACE FUNCTION argus_trash_fn.drop_tables(names text[], ts regclass[], cascade boolean, if_exists boolean,
  OUT id bigint, OUT kind text, OUT source text, OUT rows bigint, OUT expires text)
RETURNS SETOF record
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $f$
DECLARE
  n text;
  t regclass;
  c pg_class;
  deps text;
  it argus_trash.items;
  newname text;
  par regclass;
  i int;
BEGIN
  FOR i IN 1 .. coalesce(array_length(names, 1), 0) LOOP
    n := names[i];
    t := ts[i];
    IF t IS NULL THEN
      IF if_exists THEN CONTINUE; END IF;
      RAISE EXCEPTION 'table % does not exist', n USING ERRCODE = '42P01';
    END IF;
    SELECT * INTO c FROM pg_class WHERE oid = t;
    IF c.relkind NOT IN ('r', 'p') THEN
      RAISE EXCEPTION '% is not a table', t USING ERRCODE = '42809';
    END IF;
    IF c.relnamespace = 'argus_trash'::regnamespace THEN
      RAISE EXCEPTION '% is already in the trash; it goes for good when it expires', t USING ERRCODE = '42501';
    END IF;
    IF NOT pg_has_role(session_user, c.relowner, 'USAGE') THEN
      RAISE EXCEPTION 'must be owner of table %', t USING ERRCODE = '42501';
    END IF;
    deps := argus_trash_fn.dependents(t);
    IF deps IS NOT NULL THEN
      RAISE EXCEPTION 'cannot drop % because other objects depend on it: %', t, deps
        USING ERRCODE = '2BP01',
              HINT = 'Drop those first. A DROP that took them with it could not be put back from the trash.';
    END IF;
    par := NULL;
    IF c.relispartition THEN
      SELECT i.inhparent::regclass INTO par FROM pg_inherits i WHERE i.inhrelid = t;
    END IF;
    INSERT INTO argus_trash.items (kind, source, source_oid, parent, bound, rows, bytes, expires)
      VALUES ('table', t::text, t, par::text,
              CASE WHEN par IS NOT NULL THEN pg_get_expr(c.relpartbound, t) END,
              greatest(c.reltuples, 0)::bigint, pg_total_relation_size(t), now() + interval '7 days')
      RETURNING * INTO it;
    newname := left(c.relname, 40) || '__' || it.id;
    IF par IS NOT NULL THEN
      EXECUTE format('ALTER TABLE %s DETACH PARTITION %s', par, t);
    END IF;
    EXECUTE format('ALTER TABLE %s RENAME TO %I', t, newname);
    EXECUTE format('ALTER TABLE %s SET SCHEMA argus_trash', t);
    UPDATE argus_trash.items SET kept = t::text WHERE argus_trash.items.id = it.id RETURNING * INTO it;
    RETURN QUERY SELECT * FROM argus_trash_fn.out(it);
  END LOOP;
END
$f$;

-- DROP SCHEMA: the schema is renamed out of the way for 7 days, and no one
-- but its owner can reach it meanwhile.
CREATE OR REPLACE FUNCTION argus_trash_fn.drop_schemas(names text[], cascade boolean, if_exists boolean,
  OUT id bigint, OUT kind text, OUT source text, OUT rows bigint, OUT expires text)
RETURNS SETOF record
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $f$
DECLARE
  n text;
  ns pg_namespace;
  deps text;
  it argus_trash.items;
  g record;
BEGIN
  FOREACH n IN ARRAY names LOOP
    SELECT * INTO ns FROM pg_namespace WHERE nspname = n;
    IF NOT FOUND THEN
      IF if_exists THEN CONTINUE; END IF;
      RAISE EXCEPTION 'schema % does not exist', quote_ident(n) USING ERRCODE = '3F000';
    END IF;
    IF n LIKE 'argus\_trash%' OR n LIKE 'pg\_%' OR n = 'information_schema' THEN
      RAISE EXCEPTION 'schema % cannot be dropped from a session', quote_ident(n) USING ERRCODE = '42501';
    END IF;
    IF NOT pg_has_role(session_user, ns.nspowner, 'USAGE') THEN
      RAISE EXCEPTION 'must be owner of schema %', quote_ident(n) USING ERRCODE = '42501';
    END IF;
    IF NOT cascade AND EXISTS (SELECT 1 FROM pg_class WHERE relnamespace = ns.oid) THEN
      RAISE EXCEPTION 'cannot drop schema % because other objects depend on it', quote_ident(n)
        USING ERRCODE = '2BP01', HINT = 'Use DROP SCHEMA … CASCADE to drop it with what is in it; it all stays in the trash for 7 days.';
    END IF;
    SELECT string_agg(DISTINCT d, ', ') INTO deps FROM (
      SELECT 'view ' || v.oid::regclass::text AS d
        FROM pg_depend dep JOIN pg_rewrite rw ON rw.oid = dep.objid
        JOIN pg_class v ON v.oid = rw.ev_class JOIN pg_class t ON t.oid = dep.refobjid
       WHERE dep.classid = 'pg_rewrite'::regclass AND t.relnamespace = ns.oid AND v.relnamespace <> ns.oid
      UNION
      SELECT 'foreign key ' || quote_ident(c.conname) || ' on ' || c.conrelid::regclass::text
        FROM pg_constraint c JOIN pg_class t ON t.oid = c.confrelid JOIN pg_class o ON o.oid = c.conrelid
       WHERE c.contype = 'f' AND t.relnamespace = ns.oid AND o.relnamespace <> ns.oid) x;
    IF deps IS NOT NULL THEN
      RAISE EXCEPTION 'cannot drop schema % because objects outside it depend on it: %', quote_ident(n), deps
        USING ERRCODE = '2BP01', HINT = 'Drop those first.';
    END IF;
    INSERT INTO argus_trash.items (kind, source, acl, rows, bytes, expires)
      VALUES ('schema', quote_ident(n), ns.nspacl::text[],
              (SELECT count(*) FROM pg_class WHERE relnamespace = ns.oid AND relkind IN ('r', 'p')),
              (SELECT coalesce(sum(pg_total_relation_size(oid)), 0) FROM pg_class WHERE relnamespace = ns.oid AND relkind IN ('r', 'p', 'm')),
              now() + interval '7 days')
      RETURNING * INTO it;
    FOR g IN SELECT a.grantee FROM aclexplode(ns.nspacl) a WHERE a.grantee <> ns.nspowner AND a.grantee <> 0 LOOP
      EXECUTE format('REVOKE ALL ON SCHEMA %I FROM %I', n, (SELECT rolname FROM pg_roles WHERE oid = g.grantee));
    END LOOP;
    EXECUTE format('REVOKE ALL ON SCHEMA %I FROM PUBLIC', n);
    EXECUTE format('ALTER SCHEMA %I RENAME TO %I', n, 'argus_trash_' || left(n, 40) || '_' || it.id);
    UPDATE argus_trash.items SET kept = quote_ident('argus_trash_' || left(n, 40) || '_' || it.id)
     WHERE argus_trash.items.id = it.id RETURNING * INTO it;
    RETURN QUERY SELECT * FROM argus_trash_fn.out(it);
  END LOOP;
END
$f$;

-- TRUNCATE: a copy of the rows moves into argus_trash for 7 days, then the
-- table is emptied. Up to 1 GB; past that the copy would cost too much.
CREATE OR REPLACE FUNCTION argus_trash_fn.empty_tables(names text[], ts regclass[], cascade boolean,
  OUT id bigint, OUT kind text, OUT source text, OUT rows bigint, OUT expires text)
RETURNS SETOF record
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $f$
DECLARE
  n text;
  t regclass;
  c pg_class;
  deps text;
  it argus_trash.items;
  copy text;
  cnt bigint;
  i int;
BEGIN
  FOR i IN 1 .. coalesce(array_length(names, 1), 0) LOOP
    n := names[i];
    t := ts[i];
    IF t IS NULL THEN
      RAISE EXCEPTION 'table % does not exist', n USING ERRCODE = '42P01';
    END IF;
    SELECT * INTO c FROM pg_class WHERE oid = t;
    IF c.relkind NOT IN ('r', 'p') THEN
      RAISE EXCEPTION '% is not a table', t USING ERRCODE = '42809';
    END IF;
    IF NOT has_table_privilege(session_user, t, 'TRUNCATE') THEN
      RAISE EXCEPTION 'permission denied for table %', t USING ERRCODE = '42501';
    END IF;
    IF pg_total_relation_size(t) > 1073741824 THEN
      RAISE EXCEPTION '% is % on disk, too big for Argus to keep a copy, so it was not emptied.', t, pg_size_pretty(pg_total_relation_size(t))
        USING ERRCODE = 'AR006', HINT = 'Take a backup first, then empty it outside a session.';
    END IF;
    IF cascade THEN
      SELECT string_agg(DISTINCT c2.conrelid::regclass::text, ', ') INTO deps
        FROM pg_constraint c2 WHERE c2.contype = 'f' AND c2.confrelid = t AND c2.conrelid <> t;
      IF deps IS NOT NULL THEN
        RAISE EXCEPTION 'TRUNCATE … CASCADE would also empty %, which Argus would not keep', deps
          USING ERRCODE = 'AR006', HINT = 'Empty those tables first, then this one.';
      END IF;
    END IF;
    INSERT INTO argus_trash.items (kind, source, source_oid, bytes, expires)
      VALUES ('empty', t::text, t, pg_total_relation_size(t), now() + interval '7 days') RETURNING * INTO it;
    copy := left(c.relname, 40) || '__' || it.id;
    EXECUTE format('CREATE TABLE argus_trash.%I (LIKE %s)', copy, t);
    EXECUTE format('INSERT INTO argus_trash.%I SELECT * FROM %s', copy, t);
    GET DIAGNOSTICS cnt = ROW_COUNT;
    EXECUTE format('TRUNCATE %s', t);
    UPDATE argus_trash.items SET kept = 'argus_trash.' || quote_ident(copy), rows = cnt
     WHERE argus_trash.items.id = it.id RETURNING * INTO it;
    RETURN QUERY SELECT * FROM argus_trash_fn.out(it);
  END LOOP;
END
$f$;

-- The columns rows can be written back into: not generated, not dropped.
CREATE OR REPLACE FUNCTION argus_trash_fn.writable(t regclass) RETURNS text
LANGUAGE sql STABLE SET search_path = pg_catalog, pg_temp AS $f$
  SELECT string_agg(quote_ident(attname), ', ' ORDER BY attnum) FROM pg_attribute
   WHERE attrelid = t AND attnum > 0 AND NOT attisdropped AND attgenerated = ''
$f$;

-- Puts one item back: the rows into their table, the table or schema back
-- where it was.
CREATE OR REPLACE FUNCTION argus_trash_fn.restore(item bigint,
  OUT id bigint, OUT kind text, OUT source text, OUT rows bigint, OUT expires text)
RETURNS SETOF record
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $f$
DECLARE
  it argus_trash.items;
  t regclass;
  cols text;
  sch text;
  rel text;
  g text;
  who text;
BEGIN
  SELECT * INTO it FROM argus_trash.items WHERE argus_trash.items.id = item FOR UPDATE;
  IF NOT FOUND OR it.expires <= now() THEN
    RAISE EXCEPTION 'nothing in the trash has number %', item USING ERRCODE = '42704';
  END IF;
  IF it.restored IS NOT NULL THEN
    RAISE EXCEPTION 'that was put back already, by % at %', it.restored_by, it.restored USING ERRCODE = '42710';
  END IF;
  CASE it.kind
  WHEN 'rows', 'empty' THEN
    t := it.source_oid::regclass;
    IF NOT EXISTS (SELECT 1 FROM pg_class WHERE oid = it.source_oid) THEN
      RAISE EXCEPTION '% is gone, so its rows have nowhere to go', it.source USING ERRCODE = '42P01';
    END IF;
    IF NOT has_table_privilege(session_user, t, 'INSERT') THEN
      RAISE EXCEPTION 'permission denied for table %', t USING ERRCODE = '42501';
    END IF;
    cols := argus_trash_fn.writable(t);
    IF it.kind = 'rows' THEN
      EXECUTE format('INSERT INTO %s (%s) OVERRIDING SYSTEM VALUE SELECT %s FROM argus_trash.kept_rows k, '
                     'jsonb_populate_record(NULL::%s, k.row) r WHERE k.item = $1',
                     t, cols, (SELECT string_agg('r.' || x, ', ') FROM unnest(string_to_array(cols, ', ')) x), t)
        USING it.id;
    ELSE
      EXECUTE format('INSERT INTO %s (%s) OVERRIDING SYSTEM VALUE SELECT %s FROM %s', t, cols, cols, it.kept);
      EXECUTE format('DROP TABLE %s', it.kept);
    END IF;
  WHEN 'table' THEN
    t := to_regclass(it.kept);
    IF t IS NULL THEN
      RAISE EXCEPTION 'the table is no longer in the trash' USING ERRCODE = '42P01';
    END IF;
    IF NOT pg_has_role(session_user, (SELECT relowner FROM pg_class WHERE oid = t), 'USAGE') THEN
      RAISE EXCEPTION 'must be owner of table %', it.source USING ERRCODE = '42501';
    END IF;
    -- source is schema.name, as regclass printed it outside any search_path.
    sch := (parse_ident(it.source))[1];
    rel := (parse_ident(it.source))[2];
    IF to_regnamespace(quote_ident(sch)) IS NULL THEN
      RAISE EXCEPTION 'schema % is gone; make it again first', quote_ident(sch) USING ERRCODE = '3F000';
    END IF;
    IF to_regclass(quote_ident(sch) || '.' || quote_ident(rel)) IS NOT NULL THEN
      RAISE EXCEPTION 'a table named % exists again; rename it, then put this one back', it.source USING ERRCODE = '42P07';
    END IF;
    EXECUTE format('ALTER TABLE %s SET SCHEMA %I', t, sch);
    EXECUTE format('ALTER TABLE %s RENAME TO %I', t, rel);
    IF it.parent IS NOT NULL AND to_regclass(it.parent) IS NOT NULL THEN
      EXECUTE format('ALTER TABLE %s ATTACH PARTITION %s %s', it.parent, t, it.bound);
    END IF;
  WHEN 'schema' THEN
    IF to_regnamespace(it.kept) IS NULL THEN
      RAISE EXCEPTION 'the schema is no longer in the trash' USING ERRCODE = '3F000';
    END IF;
    IF NOT pg_has_role(session_user, (SELECT nspowner FROM pg_namespace WHERE oid = to_regnamespace(it.kept)), 'USAGE') THEN
      RAISE EXCEPTION 'must be owner of schema %', it.source USING ERRCODE = '42501';
    END IF;
    IF to_regnamespace(it.source) IS NOT NULL THEN
      RAISE EXCEPTION 'a schema named % exists again; rename it, then put this one back', it.source USING ERRCODE = '42P06';
    END IF;
    EXECUTE format('ALTER SCHEMA %s RENAME TO %s', it.kept, it.source);
    FOREACH g IN ARRAY coalesce(it.acl, '{}') LOOP
      -- An aclitem as text: grantee=privileges/grantor. Usage and create
      -- are all a schema has.
      who := coalesce(nullif(split_part(g, '=', 1), ''), 'PUBLIC');
      IF split_part(split_part(g, '=', 2), '/', 1) ~ 'U' THEN
        EXECUTE format('GRANT USAGE ON SCHEMA %s TO %s', it.source, who);
      END IF;
      IF split_part(split_part(g, '=', 2), '/', 1) ~ 'C' THEN
        EXECUTE format('GRANT CREATE ON SCHEMA %s TO %s', it.source, who);
      END IF;
    END LOOP;
  END CASE;
  UPDATE argus_trash.items SET restored = now(), restored_by = session_user
   WHERE argus_trash.items.id = it.id RETURNING * INTO it;
  RETURN QUERY SELECT * FROM argus_trash_fn.out(it);
END
$f$;

-- What is in the trash, newest first, for anyone who may open the database.
CREATE OR REPLACE FUNCTION argus_trash_fn.list(
  OUT id bigint, OUT kind text, OUT source text, OUT rows bigint, OUT bytes bigint,
  OUT by_role text, OUT at text, OUT expires text, OUT restored text, OUT restored_by text)
RETURNS SETOF record
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $f$
  SELECT i.id, i.kind, i.source, i.rows, i.bytes, i.by_role,
         to_char(i.at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
         to_char(i.expires AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
         to_char(i.restored AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), i.restored_by
    FROM argus_trash.items i WHERE i.expires > now() ORDER BY i.at DESC LIMIT 500
$f$;

-- Removes what has expired, for good. Run by the helper, not by people.
CREATE OR REPLACE FUNCTION argus_trash_fn.purge() RETURNS int
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $f$
DECLARE
  it argus_trash.items;
  n int := 0;
BEGIN
  FOR it IN SELECT * FROM argus_trash.items WHERE expires <= now() LOOP
    IF it.restored IS NULL AND it.kind IN ('table', 'empty') AND to_regclass(it.kept) IS NOT NULL THEN
      EXECUTE format('DROP TABLE %s CASCADE', it.kept);
    ELSIF it.restored IS NULL AND it.kind = 'schema' AND to_regnamespace(it.kept) IS NOT NULL THEN
      EXECUTE format('DROP SCHEMA %s CASCADE', it.kept);
    END IF;
    DELETE FROM argus_trash.items WHERE argus_trash.items.id = it.id;
    n := n + 1;
  END LOOP;
  RETURN n;
END
$f$;

REVOKE ALL ON ALL TABLES IN SCHEMA argus_trash FROM PUBLIC;
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA argus_trash_fn FROM PUBLIC;
`

// pgEnsureTrash sets the trash up in the database the helper is connected
// to, when it is not there at this version, lets Argus people call its
// functions, and removes what has expired.
func pgEnsureTrash(ctx context.Context, conn *pgconn.PgConn) error {
	res := conn.ExecParams(ctx,
		`SELECT coalesce(obj_description(oid, 'pg_namespace'), '') FROM pg_namespace WHERE nspname = $1`,
		[][]byte{[]byte(trashSchema)}, nil, nil, nil).Read()
	if res.Err != nil {
		return res.Err
	}
	if len(res.Rows) == 0 || string(res.Rows[0][0]) != trashVersion {
		// To everyone who can connect: each function checks, as the person,
		// that they could have done the real thing, and a session's level
		// role (after its SET ROLE) is not one of argus_people.
		people := "PUBLIC"
		grants := "GRANT USAGE ON SCHEMA argus_trash_fn TO " + people + ";\n"
		for _, f := range []string{"keep_rows(regclass, jsonb)", "drop_tables(text[], regclass[], boolean, boolean)",
			"drop_schemas(text[], boolean, boolean)", "empty_tables(text[], regclass[], boolean)", "restore(bigint)", "list()"} {
			grants += "GRANT EXECUTE ON FUNCTION argus_trash_fn." + f + " TO " + people + ";\n"
		}
		script := "BEGIN;\n" + trashSQL + grants +
			"COMMENT ON SCHEMA argus_trash IS " + quoteLiteral(trashVersion) + ";\nCOMMIT;"
		if _, err := conn.Exec(ctx, script).ReadAll(); err != nil {
			_, _ = conn.Exec(context.WithoutCancel(ctx), "ROLLBACK").ReadAll()
			return err
		}
	}
	_, err := conn.Exec(ctx, "SELECT argus_trash_fn.purge()").ReadAll()
	return err
}

// trashRewrite is what a session runs in a statement's place so that what it
// removes goes to the trash, or "" to run the statement as it is. It is
// asked only once the guard has let the statement through.
func trashRewrite(c tunnel.SQLClass, sql string) string {
	body := strings.TrimSpace(sql)
	for strings.HasSuffix(body, ";") {
		body = strings.TrimSpace(strings.TrimSuffix(body, ";"))
	}
	// The tables as text, and as the statement's own search_path finds them.
	lits := func() string {
		var out, regs []string
		for _, parts := range c.Names {
			q := make([]string, len(parts))
			for i, p := range parts {
				q[i] = trashIdent(p)
			}
			l := quoteLiteral(strings.Join(q, "."))
			out = append(out, l)
			regs = append(regs, "to_regclass("+l+")")
		}
		return "ARRAY[" + strings.Join(out, ", ") + "]::text[], ARRAY[" + strings.Join(regs, ", ") + "]::regclass[]"
	}
	bools := func(b ...bool) string {
		s := make([]string, len(b))
		for i, v := range b {
			s[i] = "false"
			if v {
				s[i] = "true"
			}
		}
		return strings.Join(s, ", ")
	}
	const cols = "SELECT id, kind, source, rows, expires FROM "
	switch c.Verb {
	case "delete":
		if c.Returning || len(c.Names) != 1 || len(c.Names[0]) == 0 {
			return ""
		}
		ref := c.Alias
		if ref == "" {
			ref = c.Names[0][len(c.Names[0])-1]
		}
		q := make([]string, len(c.Names[0]))
		for i, p := range c.Names[0] {
			q[i] = trashIdent(p)
		}
		return "WITH argus_deleted AS (\n" + body + "\nRETURNING " + trashIdent(ref) + ".*\n)\n" + cols +
			"argus_trash_fn.keep_rows(" + quoteLiteral(strings.Join(q, ".")) + "::regclass, " +
			"(SELECT jsonb_agg(to_jsonb(d)) FROM argus_deleted d))"
	case "truncate":
		if len(c.Names) == 0 {
			return ""
		}
		return cols + "argus_trash_fn.empty_tables(" + lits() + ", " + bools(c.Cascade) + ")"
	case "drop":
		if len(c.Names) == 0 {
			return ""
		}
		switch c.Object {
		case "table":
			return cols + "argus_trash_fn.drop_tables(" + lits() + ", " + bools(c.Cascade, c.IfExists) + ")"
		case "schema":
			var names []string
			for _, p := range c.Names {
				names = append(names, quoteLiteral(strings.Join(p, ".")))
			}
			return cols + "argus_trash_fn.drop_schemas(ARRAY[" + strings.Join(names, ", ") + "]::text[], " +
				bools(c.Cascade, c.IfExists) + ")"
		}
	}
	return ""
}

// trashIdent quotes any identifier for Postgres.
func trashIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// runTrashed runs ran, a trash rewrite of sql, and answers as sql would
// have: a DELETE's tag with how many rows went, a DROP's or TRUNCATE's, and
// what each put in the trash. Both are recorded: the statement as typed and
// what ran in its place.
func (s *pgSession) runTrashed(ctx context.Context, req tunnel.PGRequest, sql, ran string, c tunnel.SQLClass, args [][]byte) error {
	if err := s.record(tunnel.PGRecord{Kind: "stmt", Request: req.ID, SQL: sql, Params: req.Params,
		Ran: ran, Approval: req.Approval}); err != nil {
		return fmt.Errorf("the recording failed: %w", err)
	}
	sctx, cancel := context.WithCancel(ctx)
	s.cmu.Lock()
	s.cancel, s.curID = cancel, req.ID
	s.cmu.Unlock()
	defer func() {
		s.cmu.Lock()
		s.cancel, s.curID = nil, 0
		s.cmu.Unlock()
		cancel()
	}()
	start := time.Now()
	wrapped := false
	if s.pooled && s.conn.TxStatus() == 'I' {
		if _, err := s.conn.Exec(sctx, s.prelude).ReadAll(); err != nil {
			return s.statementFailed(ctx, req.ID, err, 0, time.Since(start).Milliseconds())
		}
		wrapped = true
	}
	res := s.conn.ExecParams(sctx, ran, args, nil, nil, nil).Read()
	err := res.Err
	if wrapped && !s.conn.IsClosed() {
		switch s.conn.TxStatus() {
		case 'T':
			if _, cerr := s.conn.Exec(context.WithoutCancel(sctx), "COMMIT").ReadAll(); cerr != nil && err == nil {
				err = cerr
			}
		case 'E':
			_, _ = s.conn.Exec(context.WithoutCancel(sctx), "ROLLBACK").ReadAll()
		}
	}
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Position > 0 {
			// A position in the DELETE as typed, which the rewrite starts with
			// after its first line; anything else has no place in what was typed.
			const lead = len("WITH argus_deleted AS (\n")
			if c.Verb == "delete" && int(pgErr.Position) > lead && int(pgErr.Position) <= lead+len(sql) {
				pgErr.Position -= int32(lead)
			} else {
				pgErr.Position = 0
			}
		}
		return s.statementFailed(ctx, req.ID, err, 0, elapsed)
	}
	var items []tunnel.PGTrashItem
	var removed int64
	for _, r := range res.Rows {
		if len(r) < 5 {
			continue
		}
		it := tunnel.PGTrashItem{Kind: string(r[1]), Source: string(r[2]), Expires: string(r[4])}
		it.ID, _ = strconv.ParseInt(string(r[0]), 10, 64)
		it.Rows, _ = strconv.ParseInt(string(r[3]), 10, 64)
		removed += it.Rows
		items = append(items, it)
	}
	var tag string
	switch {
	case c.Verb == "delete":
		tag = "DELETE " + strconv.FormatInt(removed, 10)
	case c.Verb == "truncate":
		tag = "TRUNCATE TABLE"
	default:
		tag = "DROP " + strings.ToUpper(c.Object)
	}
	if rerr := s.record(tunnel.PGRecord{Kind: "done", Request: req.ID, Tag: tag, Rows: removed, MS: elapsed}); rerr != nil {
		return rerr
	}
	return s.send(tunnel.PGAnswer{ID: req.ID, Type: "done", Tag: tag, Rows: removed, MS: elapsed, Trash: items})
}
