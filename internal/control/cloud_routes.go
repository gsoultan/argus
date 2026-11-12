package control

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
	"github.com/gsoultan/argus/internal/control/cloud/aws"
)

/*
Cloud, phase 1: connect an AWS account and read what is in it.

Who: an administrator, owner or auditor sees every connected account (the
same people who see the whole fleet); an administrator or owner connects
and disconnects them. Assignments by level come with changes, which is
where they decide something.
*/

// SetCloudProvider makes a cloud available under Cloud. A provider is an
// interface value, so a nil one is never stored.
func (a *API) SetCloudProvider(p cloud.Provider) {
	if p == nil {
		return
	}
	a.cloud.mu.Lock()
	defer a.cloud.mu.Unlock()
	if a.cloud.providers == nil {
		a.cloud.providers = map[string]cloud.Provider{}
	}
	a.cloud.providers[p.Kind()] = p
}

// cloudState is the API's cloud providers and the syncs in flight.
type cloudState struct {
	mu        sync.Mutex
	providers map[string]cloud.Provider
	// syncing holds the accounts being read now, so a "sync now" and the
	// loop never read one account twice at once. Bounded by the number of
	// connected accounts.
	syncing map[string]bool
	// bases caches each provider's answer about this host's identity for a
	// few minutes: finding no identity can mean waiting on the instance
	// metadata service, which is not something every page load should do.
	bases map[string]cachedBase
}

type cachedBase struct {
	base CloudBase
	at   time.Time
}

// cloudBaseTTL is how long a provider's identity answer is reused.
const cloudBaseTTL = 5 * time.Minute

func (a *API) cloudProvider(kind string) (cloud.Provider, bool) {
	a.cloud.mu.Lock()
	defer a.cloud.mu.Unlock()
	p, ok := a.cloud.providers[kind]
	return p, ok
}

func (a *API) registerCloudRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/cloud/accounts", a.getCloudAccounts)
	mux.HandleFunc("POST /api/v1/cloud/accounts/prepare", a.postCloudPrepare)
	mux.HandleFunc("GET /api/v1/cloud/accounts/template", a.getCloudTemplate)
	mux.HandleFunc("POST /api/v1/cloud/accounts/deploy", a.postCloudDeploy)
	mux.HandleFunc("POST /api/v1/cloud/accounts/check", a.postCloudCheck)
	mux.HandleFunc("POST /api/v1/cloud/accounts", a.postCloudAccount)
	mux.HandleFunc("DELETE /api/v1/cloud/accounts/{id}", a.deleteCloudAccount)
	mux.HandleFunc("POST /api/v1/cloud/accounts/{id}/sync", a.postCloudSync)
	mux.HandleFunc("PUT /api/v1/cloud/accounts/{id}/manage", a.putCloudManage)
	mux.HandleFunc("DELETE /api/v1/cloud/accounts/{id}/manage", a.deleteCloudManage)
	mux.HandleFunc("GET /api/v1/cloud/resources", a.getCloudResources)
}

var reAWSAccount = regexp.MustCompile(`^[0-9]{12}$`)

// cloudExternalID is the external id an account's role requires. Derived
// from the root secret, the organization, the provider and the account, so
// it is the same on every replica and after every restart, is never stored,
// and one organization's id is useless for another's account even on the
// same control plane.
func (a *API) cloudExternalID(ctx context.Context, provider, accountID string) (string, error) {
	if a.signer == nil {
		return "", errors.New("this control plane has no signing secret, which the external id is derived from")
	}
	sum := a.signer.Derive("cloud external id:" + tenantIDOf(ctx) + ":" + provider + ":" + accountID)
	return "argus-" + hex.EncodeToString(sum[:16]), nil
}

// cloudViewer authenticates someone who may see Cloud.
func (a *API) cloudViewer(w http.ResponseWriter, r *http.Request) (string, bool) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return "", false
	}
	if !seesWholeFleet(sess.Role) {
		writeErr(w, http.StatusForbidden, "Cloud is for administrators, owners and auditors")
		return "", false
	}
	return sess.Email, true
}

// cloudAdmin authenticates someone who may connect and disconnect accounts.
func (a *API) cloudAdmin(w http.ResponseWriter, r *http.Request, what string) (string, bool) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return "", false
	}
	if !a.privileged(w, sess, what, "admin", "owner") {
		return "", false
	}
	return sess.Email, true
}

// CloudBase is what the console shows about this control plane's own
// identity in a provider.
type CloudBase struct {
	Provider  string `json:"provider"`
	Principal string `json:"principal,omitempty"`
	Source    string `json:"source,omitempty"`
	Error     string `json:"error,omitempty"`
}

type cloudAccountsView struct {
	Accounts []CloudAccount `json:"accounts"`
	Bases    []CloudBase    `json:"bases"`
}

func (a *API) getCloudAccounts(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.cloudViewer(w, r); !ok {
		return
	}
	list, err := a.store.CloudAccounts(r.Context())
	if err != nil {
		a.fail(w, "cloud accounts", err)
		return
	}
	out := cloudAccountsView{Accounts: list, Bases: []CloudBase{}}
	if p, ok := a.cloudProvider("aws"); ok {
		out.Bases = append(out.Bases, a.cloudBase(r.Context(), p))
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

func (a *API) cloudBase(ctx context.Context, p cloud.Provider) CloudBase {
	a.cloud.mu.Lock()
	c, ok := a.cloud.bases[p.Kind()]
	a.cloud.mu.Unlock()
	if ok && time.Since(c.at) < cloudBaseTTL {
		return c.base
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out := CloudBase{Provider: p.Kind()}
	if b, err := p.Base(ctx); err != nil {
		out.Error = err.Error()
	} else {
		out.Principal, out.Source = b.Principal, b.Source
	}
	a.cloud.mu.Lock()
	if a.cloud.bases == nil {
		a.cloud.bases = map[string]cachedBase{}
	}
	a.cloud.bases[p.Kind()] = cachedBase{base: out, at: time.Now()}
	a.cloud.mu.Unlock()
	return out
}

type cloudPrepareIn struct {
	Provider  string `json:"provider"`
	AccountID string `json:"accountId"`
	// Manage asks for the template that also makes the manage role.
	Manage bool `json:"manage"`
}

// CloudPrepared is what the person needs to make the role in their
// account.
type CloudPrepared struct {
	Provider   string `json:"provider"`
	AccountID  string `json:"accountId"`
	ExternalID string `json:"externalId"`
	Principal  string `json:"principal"`
	Source     string `json:"source"`
	RoleName   string `json:"roleName"`
	// ManageRoleName is set when the template makes the manage role too.
	ManageRoleName string `json:"manageRoleName,omitempty"`
	// ReadRoleARN and ManageRoleARN are where the roles will be: their
	// names are fixed, so nobody copies them out of the stack.
	ReadRoleARN   string `json:"readRoleArn"`
	ManageRoleARN string `json:"manageRoleArn,omitempty"`
	Template      string `json:"template"`
	Command       string `json:"command"`
	// CanDeploy is an account argus-control's own identity is in, where
	// Argus can try to make the stack itself.
	CanDeploy bool `json:"canDeploy"`
}

// cloudRoleARN is where a role the template makes is in an account.
func cloudRoleARN(accountID, name string) string {
	return "arn:aws:iam::" + accountID + ":role/" + name
}

// prepare works out the role an account needs, or says why it cannot.
func (a *API) cloudPrepare(ctx context.Context, provider, accountID string, manage bool) (CloudPrepared, int, error) {
	if provider != "aws" {
		return CloudPrepared{}, http.StatusBadRequest, errors.New("only AWS can be connected so far")
	}
	if !reAWSAccount.MatchString(accountID) {
		return CloudPrepared{}, http.StatusBadRequest, errors.New("an AWS account id is 12 digits")
	}
	p, ok := a.cloudProvider(provider)
	if !ok {
		return CloudPrepared{}, http.StatusNotImplemented, errors.New("this control plane was started without AWS support")
	}
	base := a.cloudBase(ctx, p)
	if base.Error != "" {
		return CloudPrepared{}, http.StatusConflict, errors.New("argus-control has no AWS identity of its own yet: " + base.Error +
			". Run it on EC2 with an instance profile, or give it an IAM user's access key under Settings › Cloud identities")
	}
	ext, err := a.cloudExternalID(ctx, provider, accountID)
	if err != nil {
		return CloudPrepared{}, http.StatusInternalServerError, err
	}
	out := CloudPrepared{
		Provider: provider, AccountID: accountID, ExternalID: ext, Principal: base.Principal, Source: base.Source,
		RoleName: aws.ReadRoleName, ReadRoleARN: cloudRoleARN(accountID, aws.ReadRoleName),
		Template: aws.Template(base.Principal, ext, manage), Command: aws.DeployCommand(base.Principal, ext, manage),
	}
	if manage {
		out.ManageRoleName = aws.ManageRoleName
		out.ManageRoleARN = cloudRoleARN(accountID, aws.ManageRoleName)
	}
	if _, ok := p.(cloud.Deployer); ok && cloud.HomeAccount(base.Principal) == accountID {
		out.CanDeploy = true
	}
	return out, http.StatusOK, nil
}

func (a *API) postCloudPrepare(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.cloudAdmin(w, r, "connecting a cloud account"); !ok {
		return
	}
	var in cloudPrepareIn
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	out, code, err := a.cloudPrepare(r.Context(), in.Provider, strings.TrimSpace(in.AccountID), in.Manage)
	if err != nil {
		writeErr(w, code, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

// getCloudTemplate serves the template as a file to upload in the
// CloudFormation console.
func (a *API) getCloudTemplate(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.cloudAdmin(w, r, "connecting a cloud account"); !ok {
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("accountId"))
	manage := r.URL.Query().Get("manage") == "1"
	out, code, err := a.cloudPrepare(r.Context(), "aws", id, manage)
	if err != nil {
		writeErr(w, code, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/x-yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="argus-access-`+id+`.yaml"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(out.Template))
}

type cloudAccountIn struct {
	Provider  string `json:"provider"`
	Name      string `json:"name"`
	AccountID string `json:"accountId"`
	RoleARN   string `json:"roleArn"`
	// ManageRoleARN, when set, lets Argus make changes in the account.
	ManageRoleARN string `json:"manageRoleArn"`
	// Manage without ManageRoleARN asks for the role the template makes.
	Manage      bool   `json:"manage"`
	Environment string `json:"environment"`
}

func (in *cloudAccountIn) validate() error {
	in.Name, in.AccountID, in.RoleARN = strings.TrimSpace(in.Name), strings.TrimSpace(in.AccountID), strings.TrimSpace(in.RoleARN)
	in.ManageRoleARN = strings.TrimSpace(in.ManageRoleARN)
	// The template's roles have fixed names, so an empty ARN is the one it
	// made; a role made some other way can still be named.
	if reAWSAccount.MatchString(in.AccountID) {
		if in.RoleARN == "" {
			in.RoleARN = cloudRoleARN(in.AccountID, aws.ReadRoleName)
		}
		if in.Manage && in.ManageRoleARN == "" {
			in.ManageRoleARN = cloudRoleARN(in.AccountID, aws.ManageRoleName)
		}
	}
	if in.Provider != "aws" {
		return errors.New("only AWS can be connected so far")
	}
	if in.Name == "" || len(in.Name) > 64 {
		return errors.New("give the account a name of up to 64 characters")
	}
	if !reAWSAccount.MatchString(in.AccountID) {
		return errors.New("an AWS account id is 12 digits")
	}
	if !aws.ValidRoleARN(in.RoleARN) {
		return errors.New("that is not an IAM role ARN; it looks like arn:aws:iam::" + in.AccountID + ":role/" + aws.ReadRoleName)
	}
	if strings.Split(in.RoleARN, ":")[4] != in.AccountID {
		return errors.New("that role is in another account than " + in.AccountID)
	}
	if err := checkManageRole(in.ManageRoleARN, in.AccountID, in.RoleARN); err != nil {
		return err
	}
	if in.Environment == "" {
		in.Environment = "production"
	}
	for _, e := range CloudEnvironments {
		if in.Environment == e {
			return nil
		}
	}
	return errors.New("the environment is production, staging or development")
}

// postCloudAccount connects an account: the role is assumed as the person
// connecting it, so the first entry CloudTrail has for Argus names them.
func (a *API) postCloudAccount(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.cloudAdmin(w, r, "connecting a cloud account")
	if !ok {
		return
	}
	var in cloudAccountIn
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := in.validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p, ok := a.cloudProvider(in.Provider)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "this control plane was started without AWS support")
		return
	}
	ext, err := a.cloudExternalID(r.Context(), in.Provider, in.AccountID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	acct := cloud.Account{Provider: in.Provider, AccountID: in.AccountID, RoleARN: in.RoleARN, ExternalID: ext}
	vctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	id, err := p.Verify(vctx, acct, cloud.Actor(actor))
	cancel()
	if err != nil {
		a.auditAsset(r, AuditEvent{Action: "cloud.account.refused", Severity: "notice", ActorEmail: actor,
			Target: cloudTarget(in.Provider, in.AccountID, in.Name),
			Detail: "Could not connect " + in.RoleARN + ": " + err.Error()})
		writeErr(w, http.StatusUnprocessableEntity, "Argus could not use that role: "+err.Error()+
			". Check the stack finished and the template came from this Argus.")
		return
	}
	access := "read only"
	if in.ManageRoleARN != "" {
		macct := acct
		macct.RoleARN = in.ManageRoleARN
		vctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		_, err := p.Verify(vctx, macct, cloud.Actor(actor))
		cancel()
		if err != nil {
			a.auditAsset(r, AuditEvent{Action: "cloud.account.refused", Severity: "notice", ActorEmail: actor,
				Target: cloudTarget(in.Provider, in.AccountID, in.Name),
				Detail: "Could not use the manage role " + in.ManageRoleARN + ": " + err.Error()})
			writeErr(w, http.StatusUnprocessableEntity, "Argus could read the account but not use the manage role: "+err.Error()+
				". Check the stack was made from the read-and-change template, or connect read only.")
			return
		}
		access = "read and change through " + in.ManageRoleARN
	}
	c, err := a.store.CreateCloudAccount(r.Context(), CloudAccount{
		Provider: in.Provider, Name: in.Name, AccountID: in.AccountID, RoleARN: in.RoleARN, ManageRoleARN: in.ManageRoleARN,
		Environment: in.Environment, IdentityARN: id.ARN, Regions: id.Regions, ConnectedBy: actor,
	})
	if errors.Is(err, ErrCloudAccountExists) {
		writeErr(w, http.StatusConflict, "account "+in.AccountID+" is already connected")
		return
	}
	if err != nil {
		a.fail(w, "connect cloud account", err)
		return
	}
	a.auditAsset(r, AuditEvent{Action: "cloud.account.connected", Severity: "notice", ActorEmail: actor,
		Target: cloudTarget(c.Provider, c.AccountID, c.Name),
		Detail: "Connected " + access + ", reading through " + c.RoleARN + " as " + id.ARN + ", " + c.Environment + ", " +
			strconv.Itoa(len(id.Regions)) + " regions."})
	a.syncCloudAccountSoon(r.Context(), c)
	writeJSON(w, http.StatusCreated, c)
}

func (a *API) deleteCloudAccount(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.cloudAdmin(w, r, "disconnecting a cloud account")
	if !ok {
		return
	}
	c, err := a.store.ArchiveCloudAccount(r.Context(), r.PathValue("id"), actor)
	if errors.Is(err, ErrCloudAccountNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		a.fail(w, "disconnect cloud account", err)
		return
	}
	a.auditAsset(r, AuditEvent{Action: "cloud.account.disconnected", Severity: "notice", ActorEmail: actor,
		Target: cloudTarget(c.Provider, c.AccountID, c.Name),
		Detail: "Disconnected. Argus no longer reads it; the role in the account is still there until someone deletes its stack."})
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) postCloudSync(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.cloudAdmin(w, r, "reading a cloud account again"); !ok {
		return
	}
	c, err := a.store.CloudAccount(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrCloudAccountNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		a.fail(w, "cloud account", err)
		return
	}
	a.syncCloudAccountSoon(r.Context(), c)
	writeJSON(w, http.StatusAccepted, map[string]string{"state": "reading"})
}

func (a *API) getCloudResources(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.cloudViewer(w, r); !ok {
		return
	}
	kind := cloud.Kind(r.URL.Query().Get("kind"))
	if !cloud.ValidKind(kind) {
		writeErr(w, http.StatusBadRequest, "kind is one of instance, volume, firewall, network, subnet, address")
		return
	}
	items, err := a.store.CloudResources(r.Context(), kind)
	if err != nil {
		a.fail(w, "cloud resources", err)
		return
	}
	if kind == cloud.KindInstance {
		assets, err := a.store.Assets(r.Context())
		if err != nil {
			a.fail(w, "cloud resources", err)
			return
		}
		matchServers(items, assets)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// matchServers links instances to servers by address, then by name. The
// console labels it as a match, not as something the agent said.
func matchServers(items []CloudItem, assets []Asset) {
	byAddr, byName := map[string]Asset{}, map[string]Asset{}
	for _, s := range assets {
		if s.Address != "" {
			byAddr[s.Address] = s
		}
		byName[strings.ToLower(s.Hostname)] = s
	}
	for i := range items {
		r := items[i].Resource
		for _, ip := range append(append([]string{}, r.Addresses...), r.PublicIP) {
			if s, ok := byAddr[ip]; ok && ip != "" {
				items[i].Server = &CloudServerMatch{ID: s.ID, Hostname: s.Hostname, By: "address"}
				break
			}
		}
		if items[i].Server == nil && r.Name != r.ID {
			if s, ok := byName[strings.ToLower(r.Name)]; ok {
				items[i].Server = &CloudServerMatch{ID: s.ID, Hostname: s.Hostname, By: "name"}
			}
		}
	}
}

func cloudTarget(provider, accountID, name string) string {
	return provider + " account " + accountID + " (" + name + ")"
}

// checkManageRole checks a manage role ARN beside the read role: in the
// same account, and not the read role itself.
func checkManageRole(arn, accountID, readRole string) error {
	if arn == "" {
		return nil
	}
	if !aws.ValidRoleARN(arn) {
		return errors.New("the manage role is not an IAM role ARN; it looks like arn:aws:iam::" + accountID + ":role/" + aws.ManageRoleName)
	}
	if strings.Split(arn, ":")[4] != accountID {
		return errors.New("the manage role is in another account than " + accountID)
	}
	if arn == readRole {
		return errors.New("the manage role must be a different role from the read role")
	}
	return nil
}

// putCloudManage lets Argus change an account that was read only: the
// person deployed the manage template over the stack and pastes the new
// role, which Argus assumes as them before keeping it.
func (a *API) putCloudManage(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.cloudAdmin(w, r, "allowing changes in a cloud account")
	if !ok {
		return
	}
	var in struct {
		RoleARN string `json:"roleArn"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	c, err := a.store.CloudAccount(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrCloudAccountNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		a.fail(w, "cloud account", err)
		return
	}
	in.RoleARN = strings.TrimSpace(in.RoleARN)
	if in.RoleARN == "" {
		in.RoleARN = cloudRoleARN(c.AccountID, aws.ManageRoleName)
	}
	if err := checkManageRole(in.RoleARN, c.AccountID, c.RoleARN); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p, ok := a.cloudProvider(c.Provider)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "this control plane was started without AWS support")
		return
	}
	ext, err := a.cloudExternalID(r.Context(), c.Provider, c.AccountID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	vctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	_, err = p.Verify(vctx, cloud.Account{Provider: c.Provider, AccountID: c.AccountID, RoleARN: in.RoleARN, ExternalID: ext}, cloud.Actor(actor))
	cancel()
	if err != nil {
		a.auditAsset(r, AuditEvent{Action: "cloud.account.refused", Severity: "notice", ActorEmail: actor,
			Target: cloudTarget(c.Provider, c.AccountID, c.Name),
			Detail: "Could not use the manage role " + in.RoleARN + ": " + err.Error()})
		writeErr(w, http.StatusUnprocessableEntity, "Argus could not use that role: "+err.Error()+
			". Check the stack update finished.")
		return
	}
	c, err = a.store.SetCloudManageRole(r.Context(), c.ID, in.RoleARN)
	if err != nil {
		a.fail(w, "cloud account", err)
		return
	}
	a.auditAsset(r, AuditEvent{Action: "cloud.account.manage_allowed", Severity: "notice", ActorEmail: actor,
		Target: cloudTarget(c.Provider, c.AccountID, c.Name),
		Detail: "Argus may now change instances, disks, images and addresses in this account, through " + in.RoleARN + "."})
	// Read what the role may do now, as an update to the stack changes it.
	a.syncCloudAccountSoon(r.Context(), c)
	writeJSON(w, http.StatusOK, c)
}

// deleteCloudManage makes an account read only again. The role stays in
// the account until someone deploys the read-only template over it.
func (a *API) deleteCloudManage(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.cloudAdmin(w, r, "making a cloud account read only")
	if !ok {
		return
	}
	c, err := a.store.SetCloudManageRole(r.Context(), r.PathValue("id"), "")
	if errors.Is(err, ErrCloudAccountNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		a.fail(w, "cloud account", err)
		return
	}
	a.auditAsset(r, AuditEvent{Action: "cloud.account.manage_removed", Severity: "notice", ActorEmail: actor,
		Target: cloudTarget(c.Provider, c.AccountID, c.Name),
		Detail: "Argus no longer changes anything in this account. The manage role is still in it until the read-only template is deployed over the stack."})
	writeJSON(w, http.StatusOK, c)
}
