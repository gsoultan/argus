package control

import (
	"regexp"
	"strings"
)

/*
Which migrations a version that is going back can live with.

An update from the console goes back to the version that was running when
the new one does not come up. The new one has already migrated the database
by then, and nothing un-migrates it: the old binary runs on the new schema.
It ignores tables and columns it does not know, so a migration that only adds
is safe to go back over. One that drops, renames or retypes something the old
binary uses is not: the old version would fail on its first query against it,
and the way back is restoring the backup taken before the update.

So a migration that takes something away says so on its first line:

	-- argus:breaking: drops users.idp_subject

and TestMigrationsSayWhenTheyBreakRollback fails the build for one that does
not. Each release publishes the list (argus-control migrations -json), and the
console reads it before an update to say whether going back is clean. The
rule for new work: ship the code that stops using a column first, drop the
column a release later, and mark that migration.
*/

// MigrationInfo is one embedded migration as a release lists it.
type MigrationInfo struct {
	Name string `json:"name"`
	// Breaking is set when a version from before this migration cannot run
	// on the schema after it; Why says what it takes away.
	Breaking bool   `json:"breaking,omitempty"`
	Why      string `json:"why,omitempty"`
}

const breakingMarker = "-- argus:breaking:"

// MigrationManifest lists every embedded migration, in order.
func MigrationManifest() ([]MigrationInfo, error) {
	names, err := migrationNames()
	if err != nil {
		return nil, err
	}
	out := make([]MigrationInfo, 0, len(names))
	for _, n := range names {
		body, err := migrationFS.ReadFile("migrations/" + n)
		if err != nil {
			return nil, err
		}
		m := MigrationInfo{Name: n}
		first, _, _ := strings.Cut(string(body), "\n")
		if strings.HasPrefix(first, breakingMarker) {
			m.Breaking = true
			m.Why = strings.TrimSpace(strings.TrimPrefix(first, breakingMarker))
		}
		out = append(out, m)
	}
	return out, nil
}

var (
	sqlLineComment = regexp.MustCompile(`--[^\n]*`)
	// What an older binary cannot run against: something it reads is gone or
	// changed, or something it writes without is now required.
	takesAway = []*regexp.Regexp{
		regexp.MustCompile(`(?is)\bDROP\s+(COLUMN|TABLE|SCHEMA|VIEW|TYPE|FUNCTION)\b`),
		regexp.MustCompile(`(?is)\bRENAME\b`),
		regexp.MustCompile(`(?is)\bALTER\s+COLUMN\s+\S+\s+(SET\s+DATA\s+)?TYPE\b`),
		regexp.MustCompile(`(?is)\bSET\s+NOT\s+NULL\b`),
	}
	addColumn  = regexp.MustCompile(`(?is)\bADD\s+COLUMN\b`)
	notNull    = regexp.MustCompile(`(?is)\bNOT\s+NULL\b`)
	hasDefault = regexp.MustCompile(`(?is)\bDEFAULT\b`)
)

// takesAwaySomething reports what in sql an older binary could not run
// against, or "" when it only adds.
func takesAwaySomething(sql string) string {
	sql = sqlLineComment.ReplaceAllString(sql, "")
	for _, stmt := range strings.Split(sql, ";") {
		for _, re := range takesAway {
			if m := re.FindString(stmt); m != "" {
				return strings.Join(strings.Fields(m), " ")
			}
		}
		if addColumn.MatchString(stmt) && notNull.MatchString(stmt) && !hasDefault.MatchString(stmt) {
			return "ADD COLUMN ... NOT NULL without a DEFAULT"
		}
	}
	return ""
}
