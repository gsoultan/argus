package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

/*
Which accounts a person can ask for on a server.

The Ask for access form offered deploy, ops and root on every server, written
into the form, whatever the server had. An approval for an account the server
does not have is a grant nobody can use, and the form said nothing about it.

What can be asked for is the server's own Login accounts list -- the accounts
an administrator says Argus brokers there -- checked against what its agent
last reported having. An account on the list that the agent did not report is
still named, with the reason it cannot be picked, so a stale list is visible
rather than silently shortened. Until an agent has reported, the list is taken
as it stands: it is the administrator's word, and nothing contradicts it.
*/

// maxRequestHosts bounds one lookup. The form sends what a person picked;
// nobody asks for more servers than this at once, and the cap keeps the query
// count bounded by something other than the caller.
const maxRequestHosts = 25

// RequestableAccount is one account on a server's Login accounts list.
type RequestableAccount struct {
	Name     string `json:"name"`
	Elevated bool   `json:"elevated"`
	// OnServer is false when the agent reported its login accounts and this
	// one was not among them. True when it was, or when nothing was reported.
	OnServer bool `json:"onServer"`
}

// RequestableHost is what one server lets a person ask for.
type RequestableHost struct {
	// Hostname is what was asked about, as asked.
	Hostname string `json:"hostname"`
	// Known is false when no live asset has that name.
	Known bool `json:"known"`
	// AgentReported is whether OnServer was checked against the agent.
	AgentReported bool                 `json:"agentReported"`
	Accounts      []RequestableAccount `json:"accounts"`
}

// RequestableAccounts answers for each hostname, in the order given.
func (s *Store) RequestableAccounts(ctx context.Context, hostnames []string) ([]RequestableHost, error) {
	out := make([]RequestableHost, 0, len(hostnames))
	for _, h := range hostnames {
		host, err := s.requestableAccountsFor(ctx, h)
		if err != nil {
			return nil, err
		}
		out = append(out, host)
	}
	return out, nil
}

func (s *Store) requestableAccountsFor(ctx context.Context, hostname string) (RequestableHost, error) {
	out := RequestableHost{Hostname: hostname, Accounts: []RequestableAccount{}}
	var (
		principals []string
		reported   []byte
	)
	// The agent is matched the way LoginAccountsFor matches it: by the
	// asset's hostname, the hostname its agent enrolled under, or the short
	// name.
	err := s.db(ctx).QueryRow(ctx, `
		SELECT a.principals, ag.posture -> 'login_accounts'
		  FROM assets a
		  LEFT JOIN LATERAL (
		       SELECT posture FROM agents g
		        WHERE g.hostname = a.hostname OR g.hostname = a.agent_hostname
		           OR g.hostname = split_part(a.hostname, '.', 1)
		        ORDER BY g.last_seen_at DESC NULLS LAST
		        LIMIT 1) ag ON TRUE
		 WHERE a.archived_at IS NULL
		   AND (a.hostname = lower($1) OR split_part(a.hostname, '.', 1) = lower($1))
		 ORDER BY a.hostname = lower($1) DESC
		 LIMIT 1`, hostname).Scan(&principals, &reported)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("requestable accounts for %s: %w", hostname, err)
	}
	out.Known = true

	onServer := map[string]bool{}
	if len(reported) > 0 && string(reported) != "null" {
		var list []struct {
			User string `json:"user"`
		}
		// A report that does not parse is not a report: the list stands as the
		// administrator wrote it, as it does before the agent has said anything.
		if json.Unmarshal(reported, &list) == nil && len(list) > 0 {
			out.AgentReported = true
			for _, l := range list {
				onServer[l.User] = true
			}
		}
	}
	for _, p := range principals {
		out.Accounts = append(out.Accounts, RequestableAccount{
			Name:     p,
			Elevated: isElevated(p),
			OnServer: !out.AgentReported || onServer[p],
		})
	}
	return out, nil
}

// unrequestable says why principal cannot be asked for on hosts, or "" when
// it can. A host that is not in the inventory is not judged here: asking for
// one is how a person reaches a server they cannot see, and the approver is
// the one who decides whether it exists.
func unrequestable(hosts []RequestableHost, principal string) string {
	for _, h := range hosts {
		if !h.Known {
			continue
		}
		i := slices.IndexFunc(h.Accounts, func(a RequestableAccount) bool { return a.Name == principal })
		if i < 0 {
			names := make([]string, 0, len(h.Accounts))
			for _, a := range h.Accounts {
				names = append(names, a.Name)
			}
			listed := "no login accounts"
			if len(names) > 0 {
				listed = "only " + strings.Join(names, ", ")
			}
			return fmt.Sprintf("%s has no login account %q (it lists %s); an administrator adds accounts in the server's settings",
				h.Hostname, principal, listed)
		}
		if !h.Accounts[i].OnServer {
			return fmt.Sprintf("the agent on %s did not find an account %q on the server", h.Hostname, principal)
		}
	}
	return ""
}

// getRequestAccounts lists the accounts a person can ask for on each server
// named by ?host=. Any role that may file a request may ask; it names
// accounts, not who holds them, and only on servers the caller names.
func (a *API) getRequestAccounts(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if sess.Role == "auditor" {
		writeErr(w, http.StatusForbidden, "your role cannot request access")
		return
	}
	hosts := dedupe(r.URL.Query()["host"])
	if len(hosts) > maxRequestHosts {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("ask about at most %d servers at once", maxRequestHosts))
		return
	}
	out, err := a.store.RequestableAccounts(r.Context(), hosts)
	if err != nil {
		a.fail(w, "request accounts", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
