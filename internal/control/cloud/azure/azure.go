// Package azure is Microsoft Azure behind the provider-neutral cloud layer.
//
// The model is AWS's and Google Cloud's, in Azure's terms. argus-control has
// its own identity in Microsoft Entra: an app registration with a client
// secret kept in a file, or the managed identity of the Azure VM it runs on.
// A connected subscription holds nothing of Argus's but a role assignment:
// Reader, at the subscription, for that identity. Argus never holds a key
// for the subscription; it asks Entra for a short-lived token as itself,
// and Azure lets that token read only because the subscription's owner made
// the assignment.
//
// Azure has no role to assume and no external id of its own, so the
// assignment's description carries this Argus's external id, and every
// read is refused unless the identity's own Reader assignment at the
// subscription says it. One organization cannot read a subscription that
// trusted the same identity for another.
//
// Azure's activity log names the application a call came from, never a
// person: Azure has no field for one. Each call still carries the person in
// its user agent, and Argus's own audit log is where a change is tied to
// who asked.
//
// Plain HTTPS to Azure Resource Manager and Entra: no SDK, so nothing new
// to vendor. Control-plane only. The agent links none of this.
package azure

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
	"github.com/gsoultan/argus/internal/secrets"
)

// ReaderRoleID is Azure's built-in Reader role: list and describe every
// resource, read no data and no secret, change nothing.
const ReaderRoleID = "acdd72a7-3385-48ef-bd42-f606fba81ae7"

// ReadRoleName is what people call it.
const ReadRoleName = "Reader"

// ExternalIDMarker prefixes the external id in a role assignment's
// description.
const ExternalIDMarker = "argus-external-id="

// armScope is what a token is asked for: Azure Resource Manager.
const armScope = "https://management.azure.com/.default"

// maxSessions bounds each cache: one entry per connected subscription,
// plus room for connects being checked.
const maxSessions = 1024

// maxPages bounds one list.
const maxPages = 100

var reGUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ValidGUID reports whether s is a GUID in lower case, the way Azure
// writes subscription, tenant and application ids.
func ValidGUID(s string) bool { return reGUID.MatchString(s) }

// ValidSubscription reports whether id can be an Azure subscription id.
func ValidSubscription(id string) bool { return ValidGUID(id) }

// SubscriptionScope is a subscription as Azure's role assignments name it.
func SubscriptionScope(subscription string) string { return "/subscriptions/" + subscription }

// RoleID is where a role is in a subscription. It is what an account's
// read "role" is stored as: the role Argus must hold there.
func RoleID(subscription, role string) string {
	return SubscriptionScope(subscription) + "/providers/Microsoft.Authorization/roleDefinitions/" + role
}

// ParseRoleID is the subscription and role of a RoleID, or false.
func ParseRoleID(s string) (subscription, role string, ok bool) {
	rest, found := strings.CutPrefix(s, "/subscriptions/")
	if !found {
		return "", "", false
	}
	subscription, role, found = strings.Cut(rest, "/providers/Microsoft.Authorization/roleDefinitions/")
	if !found || !ValidGUID(subscription) || !ValidGUID(role) {
		return "", "", false
	}
	return subscription, role, true
}

// Options is where argus-control's own Azure identity comes from.
type Options struct {
	// TenantID and ClientID name an app registration: the directory it is
	// in and its application (client) id. Both empty is the managed
	// identity of the Azure VM argus-control runs on.
	TenantID string
	ClientID string
	// CredentialsFile holds the app registration's client secret and
	// nothing else. Refused unless only its owner can read it.
	CredentialsFile string
}

// endpoints are Microsoft's, and a test's server in tests.
type endpoints struct {
	metadata, login, arm string
}

var microsoft = endpoints{
	metadata: "http://169.254.169.254",
	login:    "https://login.microsoftonline.com",
	arm:      "https://management.azure.com",
}

type token struct {
	value string
	exp   time.Time
	// oid is the identity's object id in the directory the token is for,
	// which is what a role assignment names. tid is that directory, and
	// app the application (client) id.
	oid, tid, app string
}

func (t token) fresh() bool { return t.value != "" && time.Until(t.exp) > 5*time.Minute }

// Provider is Azure with one identity.
type Provider struct {
	opts Options
	ep   endpoints
	hc   *http.Client
	// mc reaches the VM's metadata service: never through a proxy, which
	// would be answering for another machine.
	mc *http.Client
	// elsewhere reports that this machine is known not to be an Azure VM,
	// so nobody waits on a metadata service that is not there.
	elsewhere func() bool

	mu sync.Mutex
	// tokens is argus-control's own token per directory. The managed
	// identity has one, under "".
	tokens map[string]token
	// tenants is the directory each subscription is in.
	tenants map[string]string
	// admitted is until when a subscription's assignment was last seen
	// to carry the external id, by subscription, role and external id.
	admitted map[string]admission
	inv      map[string]*subscriptionInventory
}

type admission struct {
	until      time.Time
	assignment string
}

// admitTTL is how long an assignment that carried the external id is
// trusted before it is read again: a subscription's owner who removes it
// stops Argus within this, and Azure itself refuses sooner than that.
const admitTTL = 10 * time.Minute

// metadataBudget is the whole wait for the VM's metadata service, which is
// on the machine's own link and answers in milliseconds.
const metadataBudget = 3 * time.Second

// azureAssetTag is the chassis asset tag every Azure VM's firmware carries.
const azureAssetTag = "7783-7084-3265-9085-8269-3286-77"

// notAzureHardware reports whether the firmware says this is not an Azure
// VM. Where the firmware cannot be read (not Linux, a container without
// /sys) it does not know, and the metadata service is asked.
func notAzureHardware() bool {
	b, err := os.ReadFile("/sys/class/dmi/id/chassis_asset_tag")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(b)) != azureAssetTag
}

// New makes a provider. Nothing is read until it is first asked.
func New(opts Options) *Provider {
	return &Provider{opts: opts, ep: microsoft, hc: &http.Client{Timeout: 30 * time.Second},
		mc:        &http.Client{Timeout: metadataBudget, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}},
		elsewhere: notAzureHardware,
		tokens:    map[string]token{}, tenants: map[string]string{}, admitted: map[string]admission{},
		inv: map[string]*subscriptionInventory{}}
}

func (p *Provider) Kind() string { return "azure" }

// managed reports whether the identity is the VM's managed identity.
func (p *Provider) managed() bool { return p.opts.ClientID == "" && p.opts.TenantID == "" }

// checkOptions refuses half an app registration.
func (p *Provider) checkOptions() error {
	if p.managed() {
		if p.opts.CredentialsFile != "" {
			return errors.New("cloud.azure has a credentials_file but no tenant_id and client_id to use it with")
		}
		return nil
	}
	switch {
	case !ValidGUID(p.opts.TenantID):
		return errors.New("cloud.azure.tenant_id is the directory (tenant) id of the app registration: a GUID in lower case")
	case !ValidGUID(p.opts.ClientID):
		return errors.New("cloud.azure.client_id is the application (client) id of the app registration: a GUID in lower case")
	case p.opts.CredentialsFile == "":
		return errors.New("cloud.azure.credentials_file is not set: it names a file holding the app registration's client secret")
	}
	return nil
}

// tokenFor is argus-control's own token for a directory, cached until it
// is near expiry. The managed identity has one token, whatever is asked.
func (p *Provider) tokenFor(ctx context.Context, tenant string) (token, error) {
	if err := p.checkOptions(); err != nil {
		return token{}, fmt.Errorf("%w: %s", cloud.ErrNoBase, err.Error())
	}
	if p.managed() {
		tenant = ""
	} else if tenant == "" {
		tenant = p.opts.TenantID
	}
	p.mu.Lock()
	if t, ok := p.tokens[tenant]; ok && t.fresh() {
		p.mu.Unlock()
		return t, nil
	}
	p.mu.Unlock()
	var (
		t   token
		err error
	)
	if p.managed() {
		t, err = p.fromMetadata(ctx)
	} else {
		t, err = p.fromSecret(ctx, tenant)
	}
	if err != nil {
		return token{}, fmt.Errorf("%w: %s", cloud.ErrNoBase, err.Error())
	}
	p.mu.Lock()
	if len(p.tokens) >= maxSessions {
		for k, v := range p.tokens {
			if !v.fresh() || len(p.tokens) >= maxSessions {
				delete(p.tokens, k)
			}
		}
	}
	p.tokens[tenant] = t
	p.mu.Unlock()
	return t, nil
}

// fromSecret asks Entra for a token as the app registration, in tenant.
func (p *Provider) fromSecret(ctx context.Context, tenant string) (token, error) {
	b, err := secrets.ReadPrivate(p.opts.CredentialsFile)
	if err != nil {
		return token{}, err
	}
	secret := strings.TrimSpace(string(b))
	if secret == "" {
		return token{}, errors.New(p.opts.CredentialsFile + " is empty: it holds the app registration's client secret")
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {p.opts.ClientID},
		"client_secret": {secret}, "scope": {armScope}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.ep.login+"/"+tenant+"/oauth2/v2.0/token", strings.NewReader(form.Encode()))
	if err != nil {
		return token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	now := time.Now()
	if err := p.do(req, &out); err != nil {
		return token{}, fmt.Errorf("Microsoft Entra refused the app registration %s in the directory %s: %w", p.opts.ClientID, tenant, err)
	}
	t := token{value: out.AccessToken, exp: now.Add(time.Duration(out.ExpiresIn) * time.Second)}
	if err := t.readClaims(); err != nil {
		return token{}, err
	}
	return t, nil
}

// fromMetadata asks the VM's metadata service for the managed identity's
// token.
func (p *Provider) fromMetadata(ctx context.Context) (token, error) {
	const nowhere = "argus-control is not on an Azure VM, and no app registration is set"
	if p.elsewhere != nil && p.elsewhere() {
		return token{}, errors.New(nowhere)
	}
	ctx, cancel := context.WithTimeout(ctx, metadataBudget)
	defer cancel()
	q := url.Values{"api-version": {"2018-02-01"}, "resource": {"https://management.azure.com/"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.ep.metadata+"/metadata/identity/oauth2/token?"+q.Encode(), nil)
	if err != nil {
		return token{}, err
	}
	req.Header.Set("Metadata", "true")
	resp, err := p.mc.Do(req)
	if err != nil {
		return token{}, errors.New(nowhere)
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return token{}, errors.New("this VM has no managed identity: " + azureError(resp.StatusCode, b).Error())
	}
	// The metadata service writes its numbers as strings.
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresOn   string `json:"expires_on"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.AccessToken == "" {
		return token{}, errors.New("the VM's metadata service gave no token")
	}
	t := token{value: out.AccessToken, exp: time.Now().Add(time.Hour)}
	if n, err := strconv.ParseInt(out.ExpiresOn, 10, 64); err == nil && n > 0 {
		t.exp = time.Unix(n, 0)
	}
	if err := t.readClaims(); err != nil {
		return token{}, err
	}
	return t, nil
}

// readClaims reads who a token is for from the token itself. Nothing is
// verified here and nothing needs to be: the token came from Microsoft over
// TLS (or the VM's own link), and Azure checks its signature on every call.
// The claims only tell Argus which assignment to look for.
func (t *token) readClaims() error {
	parts := strings.Split(t.value, ".")
	if len(parts) != 3 {
		return errors.New("Microsoft's token is not one Argus can read its identity from")
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return errors.New("Microsoft's token is not one Argus can read its identity from")
	}
	var c struct {
		OID   string `json:"oid"`
		TID   string `json:"tid"`
		AppID string `json:"appid"`
		AZP   string `json:"azp"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return errors.New("Microsoft's token is not one Argus can read its identity from")
	}
	t.oid, t.tid, t.app = strings.ToLower(c.OID), strings.ToLower(c.TID), strings.ToLower(c.AppID)
	if t.app == "" {
		t.app = strings.ToLower(c.AZP)
	}
	if !ValidGUID(t.oid) || !ValidGUID(t.app) {
		return errors.New("Microsoft's token names no application and object id")
	}
	return nil
}

// Base says who argus-control is to Azure: its application (client) id,
// which is what a subscription's setup command names.
func (p *Provider) Base(ctx context.Context) (cloud.Base, error) {
	t, err := p.tokenFor(ctx, "")
	if err != nil {
		return cloud.Base{}, err
	}
	src := "the app registration's client secret in " + p.opts.CredentialsFile
	if p.managed() {
		src = "the managed identity of the VM argus-control runs on"
	}
	return cloud.Base{Principal: t.app, ARN: t.app, Source: src}, nil
}

var reTenantInHeader = regexp.MustCompile(`(?i)login\.(?:microsoftonline\.com|windows\.net)/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)

// tenantOf is the directory a subscription is in. Azure says it to anyone
// who asks without a token: the refusal names where to sign in. An app
// registration needs it, because its token is per directory; "" means
// Azure did not say, and the app's own directory is tried.
func (p *Provider) tenantOf(ctx context.Context, subscription string) string {
	p.mu.Lock()
	if t, ok := p.tenants[subscription]; ok {
		p.mu.Unlock()
		return t
	}
	p.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.ep.arm+SubscriptionScope(subscription)+"?api-version=2022-12-01", nil)
	if err != nil {
		return ""
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		return ""
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	m := reTenantInHeader.FindStringSubmatch(resp.Header.Get("WWW-Authenticate"))
	if m == nil {
		return ""
	}
	tenant := strings.ToLower(m[1])
	p.mu.Lock()
	if len(p.tenants) >= maxSessions {
		clear(p.tenants)
	}
	p.tenants[subscription] = tenant
	p.mu.Unlock()
	return tenant
}

// session is argus-control's token for the subscription's directory, given
// out only once the identity's own assignment of the account's role at the
// subscription has been seen to carry this Argus's external id.
func (p *Provider) session(ctx context.Context, a cloud.Account, actor cloud.Actor) (token, string, error) {
	if !actor.Valid() {
		return token{}, "", fmt.Errorf("%q cannot be named in a call to Azure", string(actor))
	}
	if !ValidSubscription(a.AccountID) {
		return token{}, "", errors.New("a subscription id is a GUID, like 6b1f2c0e-8a44-4f0b-9d55-0c2a7e4d1a90")
	}
	sub, role, ok := ParseRoleID(a.RoleARN)
	if !ok || sub != a.AccountID {
		return token{}, "", fmt.Errorf("%s is not a role in subscription %s", a.RoleARN, a.AccountID)
	}
	if a.ExternalID == "" {
		return token{}, "", errors.New("no external id was given for the subscription")
	}
	tenant := ""
	if !p.managed() {
		tenant = p.tenantOf(ctx, a.AccountID)
	}
	t, err := p.tokenFor(ctx, tenant)
	if err != nil {
		return token{}, "", err
	}
	key := a.AccountID + "|" + role + "|" + a.ExternalID + "|" + t.oid
	p.mu.Lock()
	ad, ok := p.admitted[key]
	p.mu.Unlock()
	if ok && time.Now().Before(ad.until) {
		return t, ad.assignment, nil
	}
	assignment, err := p.findAssignment(ctx, t, actor, a.AccountID, role, a.ExternalID)
	if err != nil {
		return token{}, "", err
	}
	p.mu.Lock()
	if len(p.admitted) >= maxSessions {
		now := time.Now()
		for k, v := range p.admitted {
			if now.After(v.until) || len(p.admitted) >= maxSessions {
				delete(p.admitted, k)
			}
		}
	}
	p.admitted[key] = admission{until: time.Now().Add(admitTTL), assignment: assignment}
	p.mu.Unlock()
	return t, assignment, nil
}

// findAssignment looks for the identity's own assignment of role at the
// subscription itself, with this Argus's external id in its description,
// and returns its id. Reader may read role assignments, so the read role
// is enough to check itself.
func (p *Provider) findAssignment(ctx context.Context, t token, actor cloud.Actor, subscription, role, ext string) (string, error) {
	scope := SubscriptionScope(subscription)
	type assignment struct {
		ID         string `json:"id"`
		Properties struct {
			PrincipalID      string `json:"principalId"`
			RoleDefinitionID string `json:"roleDefinitionId"`
			Scope            string `json:"scope"`
			Description      string `json:"description"`
		} `json:"properties"`
	}
	q := url.Values{"api-version": {"2022-04-01"}, "$filter": {"atScope()"}}
	held, other := false, false
	err := p.pages(ctx, t, actor, p.ep.arm+scope+"/providers/Microsoft.Authorization/roleAssignments?"+q.Encode(), func(raw json.RawMessage) error {
		var list []assignment
		if err := json.Unmarshal(raw, &list); err != nil {
			return err
		}
		for _, as := range list {
			pr := as.Properties
			if !strings.EqualFold(pr.PrincipalID, t.oid) || !strings.HasSuffix(strings.ToLower(pr.RoleDefinitionID), "/"+role) ||
				!strings.EqualFold(strings.TrimRight(pr.Scope, "/"), scope) {
				continue
			}
			held = true
			if hasMarker(pr.Description, ext) {
				return foundAssignment(as.ID)
			}
			other = other || strings.Contains(pr.Description, ExternalIDMarker)
		}
		return nil
	})
	var found foundAssignment
	if errors.As(err, &found) {
		return string(found), nil
	}
	if err != nil {
		return "", fmt.Errorf("argus-control (%s) cannot read subscription %s: %w", t.app, subscription, err)
	}
	switch {
	case other:
		return "", fmt.Errorf("argus-control (%s) has the role in subscription %s, but it was given for another Argus: "+
			"the assignment's description does not carry this Argus's external id. Run the command from the connect page again", t.app, subscription)
	case held:
		return "", fmt.Errorf("argus-control (%s) has the role in subscription %s, but not from this Argus's command: "+
			"the assignment's description does not carry this Argus's external id. Run the command from the connect page", t.app, subscription)
	}
	return "", fmt.Errorf("argus-control (%s) has not been given the role at subscription %s itself", t.app, subscription)
}

// foundAssignment stops a paged read at the assignment looked for.
type foundAssignment string

func (f foundAssignment) Error() string { return "found " + string(f) }

// hasMarker reports whether a description carries exactly this external
// id, not one that merely starts with it.
func hasMarker(desc, ext string) bool {
	if ext == "" {
		return false
	}
	for _, f := range strings.Fields(desc) {
		if strings.TrimRight(f, ".,;") == ExternalIDMarker+ext {
			return true
		}
	}
	return false
}

// userAgent names the person a call runs for.
func userAgent(actor cloud.Actor) string { return "argus-control (for " + string(actor) + ")" }

// Verify checks the identity's assignment as actor and reports what it
// found: the regions the subscription can use.
func (p *Provider) Verify(ctx context.Context, a cloud.Account, actor cloud.Actor) (cloud.Identity, error) {
	t, assignment, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Identity{}, err
	}
	type location struct {
		Name     string `json:"name"`
		Metadata struct {
			RegionType string `json:"regionType"`
		} `json:"metadata"`
	}
	var out []string
	err = p.pages(ctx, t, actor, p.ep.arm+SubscriptionScope(a.AccountID)+"/locations?api-version=2022-12-01", func(raw json.RawMessage) error {
		var list []location
		if err := json.Unmarshal(raw, &list); err != nil {
			return err
		}
		for _, l := range list {
			// Logical locations (a whole geography, a staging stamp) hold
			// no resources of the kinds Argus reads.
			if n := place(l.Name); n != "" && (l.Metadata.RegionType == "" || strings.EqualFold(l.Metadata.RegionType, "Physical")) {
				out = append(out, n)
			}
		}
		return nil
	})
	if err != nil {
		return cloud.Identity{}, fmt.Errorf("argus-control (%s) cannot list the regions of subscription %s: %w", t.app, a.AccountID, err)
	}
	if len(out) == 0 {
		return cloud.Identity{}, fmt.Errorf("Azure listed no regions for subscription %s", a.AccountID)
	}
	sort.Strings(out)
	return cloud.Identity{ARN: assignment, AccountID: a.AccountID, Regions: out}, nil
}

var rePlace = regexp.MustCompile(`^[a-z0-9-]{2,40}$`)

// place is an Azure location as Argus keeps regions: lower case, no
// spaces. "" is one that is not a location name.
func place(s string) string {
	s = strings.ToLower(strings.ReplaceAll(s, " ", ""))
	if !rePlace.MatchString(s) {
		return ""
	}
	return s
}

// pages reads every page of an Azure list, handing each page's value to
// each. A next link is followed only on the endpoint the token is for, so
// an answer cannot send the token anywhere else.
func (p *Provider) pages(ctx context.Context, t token, actor cloud.Actor, u string, each func(json.RawMessage) error) error {
	for range maxPages {
		var resp struct {
			Value    json.RawMessage `json:"value"`
			NextLink string          `json:"nextLink"`
		}
		if err := p.get(ctx, t, actor, u, &resp); err != nil {
			return err
		}
		if len(resp.Value) > 0 && string(resp.Value) != "null" {
			if err := each(resp.Value); err != nil {
				return err
			}
		}
		if resp.NextLink == "" {
			return nil
		}
		if !strings.HasPrefix(resp.NextLink, p.ep.arm+"/") {
			return errors.New("Azure's answer pointed at another address for its next page")
		}
		u = resp.NextLink
	}
	return errors.New("Azure's list did not end")
}

func (p *Provider) get(ctx context.Context, t token, actor cloud.Actor, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+t.value)
	req.Header.Set("User-Agent", userAgent(actor))
	return p.do(req, v)
}

// do sends req and decodes a 200 into v, or Azure's own message.
func (p *Provider) do(req *http.Request, v any) error {
	resp, err := p.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return azureError(resp.StatusCode, b)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("Azure's answer: %w", err)
	}
	return nil
}

// azureError is Microsoft's message, in its words, with a next step for
// the refusals people meet while setting up. Resource Manager answers
// {"error":{"code","message"}}; Entra answers {"error","error_description"}.
func azureError(code int, b []byte) error {
	var arm struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	var entra struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	msg, kind := "", ""
	if json.Unmarshal(b, &arm) == nil && arm.Error.Message != "" {
		msg, kind = arm.Error.Message, arm.Error.Code
	} else if json.Unmarshal(b, &entra) == nil && entra.Description != "" {
		msg, kind = entra.Description, entra.Error
	}
	if msg == "" {
		msg = http.StatusText(code)
	}
	// Entra appends a trace id, a correlation id and a timestamp.
	if i := strings.IndexAny(msg, "\r\n"); i > 0 {
		msg = msg[:i]
	}
	msg = strings.TrimSpace(msg)
	switch {
	case strings.Contains(msg, "AADSTS700016"):
		msg += " (argus-control's application is not in that directory yet: the command from the connect page adds it)"
	case strings.Contains(msg, "AADSTS7000215"), strings.Contains(msg, "AADSTS7000222"):
		msg += " (the client secret in argus-control's config is wrong or has expired: make a new one for the app registration)"
	case kind == "InvalidAuthenticationTokenTenant":
		msg += " (the subscription is in another directory than argus-control's identity: a managed identity only reaches its own directory, so use an app registration)"
	case kind == "AuthorizationFailed":
		msg += " (the role has not been given yet, or Azure has not finished applying it: that can take a few minutes)"
	case kind == "SubscriptionNotFound":
		msg += " (argus-control's identity has no role in it yet, or the id is not a subscription's)"
	}
	return errors.New(msg)
}
