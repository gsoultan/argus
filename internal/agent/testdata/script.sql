-- Clean up a widget and everything that depends on it.
--
-- Run the whole file at once (one transaction). It ends in ROLLBACK: the
-- preview shows and nothing is deleted; change it to COMMIT to keep it.

BEGIN;

-- ===================================================================
-- INPUT
-- ===================================================================
CREATE TEMP TABLE input_widget (id int PRIMARY KEY) ON COMMIT DROP;
INSERT INTO input_widget (id) VALUES
    (1); -- the one to remove

CREATE TEMP TABLE purge_row (
    tbl regclass NOT NULL,
    id  int      NOT NULL,
    PRIMARY KEY (tbl, id)
) ON COMMIT DROP;
CREATE INDEX ON purge_row (id);

/* A helper whose body is full of semicolons; /* nested */ comments too; */
CREATE FUNCTION pg_temp.purge_delete()
RETURNS TABLE (table_name text, deleted bigint) LANGUAGE plpgsql AS $$
DECLARE
    r record;
    n bigint;
BEGIN
    FOR r IN SELECT DISTINCT p.tbl FROM purge_row p LOOP
        EXECUTE format('DELETE FROM %s t WHERE t.id IN (SELECT p.id FROM purge_row p WHERE p.tbl = %L::regclass)', r.tbl, r.tbl);
        GET DIAGNOSTICS n = ROW_COUNT;
        table_name := r.tbl::text;
        deleted := n;
        RETURN NEXT;
    END LOOP;
END $$;

CREATE FUNCTION pg_temp.label(i int) RETURNS text LANGUAGE sql AS $fn$ SELECT 'widget; ' || i $fn$;

INSERT INTO purge_row (tbl, id)
SELECT 'argus_t_widget'::regclass, w.id FROM argus_t_widget w WHERE w.id IN (SELECT id FROM input_widget);

-- Widgets that were not found (a typo).
SELECT i.id AS widget_id_not_found FROM input_widget i
WHERE NOT EXISTS (SELECT 1 FROM argus_t_widget w WHERE w.id = i.id);

SELECT pg_temp.label(id) AS label, 'it''s; quoted' AS q, E'back\'slash;' AS e, "id" FROM input_widget;

SELECT * FROM pg_temp.purge_delete();

-- Change to COMMIT once the preview is right.
ROLLBACK;
