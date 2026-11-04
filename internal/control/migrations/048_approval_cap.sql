-- The cap on an approved change: how many rows the dry run counted for each
-- statement when it was asked for, which is what the approver saw. The relay
-- passes it to the agent, which rolls the statement back if it would change
-- more. NULL is a statement that does not count rows (a schema change), or
-- one asked for without a dry run.
ALTER TABLE database_statement_items ADD COLUMN IF NOT EXISTS row_cap BIGINT;
