package control

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrTeamNotFound is a team id that names nothing.
var ErrTeamNotFound = errors.New("no such team")

// Team is a named group of people, and the servers given to all of them.
type Team struct {
	ID        string           `json:"id"`
	Name      string           `json:"name"`
	Members   []string         `json:"members"`
	Servers   []TeamAssignment `json:"servers"`
	CreatedBy string           `json:"createdBy"`
	CreatedAt time.Time        `json:"createdAt"`
}

// TeamAssignment is one team's access to one asset.
type TeamAssignment struct {
	AssetID    string   `json:"assetId"`
	Hostname   string   `json:"hostname"`
	TeamID     string   `json:"teamId"`
	TeamName   string   `json:"teamName"`
	Principals []string `json:"principals"`
	// PersonalAccounts gives each member an account of their own on the
	// server; Sudo lets those accounts run anything as root without a
	// password; DirectSSH is "off", "approve" or "allow" for SSH straight to
	// them.
	PersonalAccounts bool      `json:"personalAccounts"`
	Sudo             bool      `json:"sudo"`
	DirectSSH        string    `json:"directSsh"`
	GrantedBy        string    `json:"grantedBy"`
	GrantedAt        time.Time `json:"grantedAt"`
}

// TeamAccess is what a team is given on one server.
type TeamAccess struct {
	Principals       []string
	PersonalAccounts bool
	Sudo             bool
	// DirectSSH is "off" (or ""), "approve" or "allow".
	DirectSSH string
}

// Direct SSH to a personal account is off unless a team assignment says so.
const DirectSSHOff = "off"

func normaliseTeamName(name string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return "", ErrInvalidAsset{Reason: "a team needs a name"}
	}
	if len([]rune(name)) > 80 {
		return "", ErrInvalidAsset{Reason: "a team name may be at most 80 characters"}
	}
	return name, nil
}

// Teams lists every team with its members and servers.
//
// Three queries rather than one per team: the page shows them all at once,
// and a team count is not something a caller should be able to turn into a
// query count.
func (s *Store) Teams(ctx context.Context) ([]Team, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, name, created_by, created_at FROM teams ORDER BY lower(name)`)
	if err != nil {
		return nil, err
	}
	teams := []Team{}
	index := map[string]int{}
	for rows.Next() {
		var t Team
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedBy, &t.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		t.Members, t.Servers = []string{}, []TeamAssignment{}
		index[t.ID] = len(teams)
		teams = append(teams, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.pool.Query(ctx, `
		SELECT team_id::text, user_email FROM team_members ORDER BY user_email`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, email string
		if err := rows.Scan(&id, &email); err != nil {
			rows.Close()
			return nil, err
		}
		if i, ok := index[id]; ok {
			teams[i].Members = append(teams[i].Members, email)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	all, err := s.teamAssignments(ctx, "a.archived_at IS NULL")
	if err != nil {
		return nil, err
	}
	for _, g := range all {
		if i, ok := index[g.TeamID]; ok {
			teams[i].Servers = append(teams[i].Servers, g)
		}
	}
	return teams, nil
}

// TeamByID is one team, members and servers included.
func (s *Store) TeamByID(ctx context.Context, id string) (Team, error) {
	teams, err := s.Teams(ctx)
	if err != nil {
		return Team{}, err
	}
	for _, t := range teams {
		if t.ID == id {
			return t, nil
		}
	}
	return Team{}, ErrTeamNotFound
}

// CreateTeam makes a team, with its first members if any are given.
func (s *Store) CreateTeam(ctx context.Context, name string, members []string, actor string) (string, error) {
	name, err := normaliseTeamName(name)
	if err != nil {
		return "", err
	}
	emails, err := normaliseEmails(members)
	if err != nil {
		return "", err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id string
	err = tx.QueryRow(ctx, `INSERT INTO teams (name, created_by) VALUES ($1, $2) RETURNING id::text`,
		name, strings.ToLower(actor)).Scan(&id)
	if err != nil && isUniqueViolation(err) {
		return "", ErrInvalidAsset{Reason: fmt.Sprintf("there is already a team called %q", name)}
	}
	if err != nil {
		return "", err
	}
	for _, e := range emails {
		if _, err := tx.Exec(ctx, `
			INSERT INTO team_members (team_id, user_email, added_by) VALUES ($1::uuid, $2, $3)
			ON CONFLICT DO NOTHING`, id, e, strings.ToLower(actor)); err != nil {
			return "", err
		}
	}
	return id, tx.Commit(ctx)
}

// RenameTeam changes a team's name. Its members and servers are untouched.
func (s *Store) RenameTeam(ctx context.Context, id, name string) error {
	name, err := normaliseTeamName(name)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE teams SET name = $2 WHERE id = $1::uuid`, id, name)
	if err != nil && isUniqueViolation(err) {
		return ErrInvalidAsset{Reason: fmt.Sprintf("there is already a team called %q", name)}
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrTeamNotFound
	}
	return nil
}

// DeleteTeam removes a team. Everyone who reached a server only through it
// loses that access in the same statement: the cascade takes its members and
// its assignments with it.
func (s *Store) DeleteTeam(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM teams WHERE id = $1::uuid`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrTeamNotFound
	}
	return nil
}

// AddTeamMember puts a person in a team. Adding someone already in it is not
// an error: the result is the state that was asked for.
func (s *Store) AddTeamMember(ctx context.Context, teamID, email, actor string) error {
	emails, err := normaliseEmails([]string{email})
	if err != nil {
		return err
	}
	if len(emails) == 0 {
		return ErrInvalidAsset{Reason: "adding someone to a team needs their email"}
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO team_members (team_id, user_email, added_by)
		SELECT id, $2, $3 FROM teams WHERE id = $1::uuid
		ON CONFLICT DO NOTHING`, teamID, emails[0], strings.ToLower(actor))
	if err != nil {
		return err
	}
	if err := s.teamExists(ctx, teamID); err != nil {
		return err
	}
	// Joining a team with personal accounts somewhere means needing a name.
	return s.ensurePersonalLoginNames(ctx)
}

// RemoveTeamMember takes a person out of a team, and with it every server
// they reached only through it.
func (s *Store) RemoveTeamMember(ctx context.Context, teamID, email string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM team_members WHERE team_id = $1::uuid AND user_email = lower($2)`,
		teamID, strings.TrimSpace(email))
	if err != nil {
		return err
	}
	return s.teamExists(ctx, teamID)
}

func (s *Store) teamExists(ctx context.Context, id string) error {
	var ok bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM teams WHERE id = $1::uuid)`, id).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ErrTeamNotFound
	}
	return nil
}

// TeamAssignments lists the teams that may reach one asset.
func (s *Store) TeamAssignments(ctx context.Context, assetID string) ([]TeamAssignment, error) {
	return s.teamAssignments(ctx, "ta.asset_id = $1::uuid", assetID)
}

func (s *Store) teamAssignments(ctx context.Context, where string, args ...any) ([]TeamAssignment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT ta.asset_id::text, a.hostname, ta.team_id::text, t.name, ta.principals,
		       ta.personal_accounts, ta.sudo, ta.direct_ssh, ta.granted_by, ta.granted_at
		  FROM asset_team_assignments ta
		  JOIN assets a ON a.id = ta.asset_id
		  JOIN teams t ON t.id = ta.team_id
		 WHERE `+where+`
		 ORDER BY lower(t.name), a.hostname`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TeamAssignment{}
	for rows.Next() {
		var g TeamAssignment
		if err := rows.Scan(&g.AssetID, &g.Hostname, &g.TeamID, &g.TeamName, &g.Principals,
			&g.PersonalAccounts, &g.Sudo, &g.DirectSSH, &g.GrantedBy, &g.GrantedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// SetTeamAssignment records which accounts a team may log in as on an asset.
// The same rules as SetAssignment: every principal must be one the asset
// permits, and an empty list is a removal.
func (s *Store) SetTeamAssignment(
	ctx context.Context, assetID, teamID string, principals []string, actor string,
) error {
	return s.SetTeamAccess(ctx, assetID, teamID, TeamAccess{Principals: principals}, actor)
}

// SetTeamAccess records what a team is given on an asset: shared accounts,
// an account of each member's own, or both. Giving neither is a removal.
func (s *Store) SetTeamAccess(
	ctx context.Context, assetID, teamID string, in TeamAccess, actor string,
) error {
	principals := dedupe(in.Principals)
	if len(principals) == 0 && !in.PersonalAccounts {
		return s.RemoveTeamAssignment(ctx, assetID, teamID)
	}
	switch in.DirectSSH {
	case "":
		in.DirectSSH = DirectSSHOff
	case DirectSSHOff, DirectSSHApprove, DirectSSHAllow:
	default:
		return ErrInvalidAsset{Reason: `direct SSH must be "off", "approve" or "allow"`}
	}
	if (in.Sudo || in.DirectSSH != DirectSSHOff) && !in.PersonalAccounts {
		// On a shared account there is nobody in particular to give them to.
		return ErrInvalidAsset{Reason: "sudo and direct SSH apply to personal accounts; turn those on first"}
	}
	if len(principals) > maxPrincipals {
		return ErrInvalidAsset{Reason: fmt.Sprintf(
			"an assignment may name at most %d principals", maxPrincipals)}
	}
	asset, err := s.AssetByID(ctx, assetID)
	if err != nil {
		return err
	}
	if err := checkPrincipalsAllowed(asset, principals); err != nil {
		return err
	}
	if err := s.teamExists(ctx, teamID); err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO asset_team_assignments
		       (asset_id, team_id, principals, personal_accounts, sudo, direct_ssh, granted_by)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7)
		ON CONFLICT (asset_id, team_id) DO UPDATE SET
			principals        = EXCLUDED.principals,
			personal_accounts = EXCLUDED.personal_accounts,
			sudo              = EXCLUDED.sudo,
			direct_ssh        = EXCLUDED.direct_ssh,
			granted_by        = EXCLUDED.granted_by,
			granted_at        = now()`,
		assetID, teamID, principals, in.PersonalAccounts, in.Sudo, in.DirectSSH, strings.ToLower(actor))
	if err != nil {
		return err
	}
	if in.PersonalAccounts {
		return s.ensurePersonalLoginNames(ctx)
	}
	return nil
}

// RemoveTeamAssignment ends a team's access to an asset.
func (s *Store) RemoveTeamAssignment(ctx context.Context, assetID, teamID string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM asset_team_assignments WHERE asset_id = $1::uuid AND team_id = $2::uuid`,
		assetID, teamID)
	return err
}

// insertTeamAssignments writes a team assignment per team inside a caller's
// transaction, for approving a server and giving it out in one step.
func insertTeamAssignments(
	ctx context.Context, tx pgx.Tx, assetID string, teamIDs, principals []string,
	personal bool, actor string,
) error {
	for _, id := range teamIDs {
		tag, err := tx.Exec(ctx, `
			INSERT INTO asset_team_assignments (asset_id, team_id, principals, personal_accounts, granted_by)
			SELECT $1::uuid, t.id, $3, $5, $4 FROM teams t WHERE t.id = $2::uuid
			ON CONFLICT (asset_id, team_id) DO UPDATE SET
				principals        = EXCLUDED.principals,
				personal_accounts = EXCLUDED.personal_accounts,
				granted_by        = EXCLUDED.granted_by,
				granted_at        = now()`,
			assetID, id, principals, strings.ToLower(actor), personal)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrTeamNotFound
		}
	}
	return nil
}

func checkPrincipalsAllowed(asset Asset, principals []string) error {
	allowed := map[string]bool{}
	for _, p := range asset.Principals {
		allowed[p] = true
	}
	for _, p := range principals {
		if !allowed[p] {
			return ErrInvalidAsset{Reason: fmt.Sprintf(
				"%s does not permit the principal %q; add it to the asset first",
				asset.Hostname, p)}
		}
	}
	return nil
}

func normaliseEmails(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	for _, e := range in {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if !strings.Contains(e, "@") || strings.ContainsAny(e, " \t\r\n") {
			return nil, ErrInvalidAsset{Reason: fmt.Sprintf("%q is not an email address", e)}
		}
		out = append(out, e)
	}
	return dedupe(out), nil
}
