-- One address, one account.
--
-- users.email carried a UNIQUE constraint, which Postgres applies to the exact
-- string, while every lookup in accounts.go matches on lower(email). Those two
-- rules disagreed, and the gap between them was reachable:
--
--   `users add Alice@corp.com` inserted a second row alongside alice@corp.com,
--   because the two differ as exact strings. Sign-in then matched both, and
--   QueryRow returned whichever the planner produced first. Worse, SetPassword
--   and BeginMFAEnrolment update WHERE lower(email) = lower($1) with no LIMIT,
--   so an admin changing their own password wrote that hash onto the second
--   row as well -- an account with someone else's role and the admin's
--   credential.
--
-- Refuse rather than guess. Merging two accounts means choosing which role,
-- which password and which audit history survive, and that is not a decision a
-- migration gets to make silently at start-up.
DO $$
DECLARE
  dups TEXT;
BEGIN
  SELECT string_agg(e, ', ') INTO dups
    FROM (SELECT lower(email) AS e FROM users GROUP BY lower(email) HAVING count(*) > 1) d;
  IF dups IS NOT NULL THEN
    RAISE EXCEPTION
      'these addresses exist more than once with different capitalisation: %. '
      'Argus matches sign-in on the lowercased address, so each is two accounts '
      'answering to one login. Decide which to keep, remove the other, and start again.',
      dups;
  END IF;
END $$;

UPDATE users SET email = lower(email) WHERE email <> lower(email);

-- The plain index this replaces made the lookup fast; it did not make it
-- unambiguous. Uniqueness has to be stated on the same expression the lookup
-- uses, or the constraint guards a different question than the one being asked.
DROP INDEX IF EXISTS users_email_lower_idx;
CREATE UNIQUE INDEX IF NOT EXISTS users_email_lower_key ON users (lower(email));
