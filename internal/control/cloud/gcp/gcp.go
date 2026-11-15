// Package gcp is Google Cloud behind the provider-neutral cloud layer.
//
// The model is AWS's, in Google's terms. argus-control has its own service
// account (the VM's, or a key file); a connected project holds a service
// account of its own, argus-read, that may list and describe Compute Engine
// and nothing else. Argus never holds a key for argus-read: it asks Google
// for a short-lived token for it, which Google allows only because the
// project granted argus-control's account the right to (Service Account
// Token Creator on argus-read alone). The project's account carries this
// Argus's external id in its description, and every token is refused unless
// it does, so one organization cannot read a project that trusted the same
// identity for another.
//
// Plain HTTPS to Google's REST APIs: no SDK, so nothing new to vendor.
// Control-plane only. The agent links none of this.
package gcp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/control/cloud"
	"github.com/gsoultan/argus/internal/secrets"
)

// ReadAccountName is the service account the setup makes in a project. Its
// name is fixed, so Argus knows where it will be and nothing is copied back.
const ReadAccountName = "argus-read"

// ExternalIDMarker prefixes the external id in argus-read's description.
const ExternalIDMarker = "argus-external-id="

const scope = "https://www.googleapis.com/auth/cloud-platform"

// maxSessions bounds the cached tokens: one per connected project's
// account, plus room for connects being checked.
const maxSessions = 1024

var reProject = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)

// ValidProject reports whether id can be a Google Cloud project id.
func ValidProject(id string) bool { return reProject.MatchString(id) }

// AccountEmail is where a project's service account named name is.
func AccountEmail(project, name string) string {
	return name + "@" + project + ".iam.gserviceaccount.com"
}

// ValidAccountEmail reports whether s is a user-managed service account in
// a project.
func ValidAccountEmail(s string) bool {
	name, rest, ok := strings.Cut(s, "@")
	if !ok || name == "" {
		return false
	}
	p, ok := strings.CutSuffix(rest, ".iam.gserviceaccount.com")
	return ok && ValidProject(p)
}

// ProjectOf is the project a service account email is in, or "".
func ProjectOf(email string) string {
	_, rest, _ := strings.Cut(email, "@")
	p, ok := strings.CutSuffix(rest, ".iam.gserviceaccount.com")
	if !ok {
		return ""
	}
	return p
}

// Options is where argus-control's own service account comes from.
type Options struct {
	// CredentialsFile is a service account key file (JSON). Refused unless
	// only its owner can read it. Empty uses GOOGLE_APPLICATION_CREDENTIALS
	// when it names a key, else the VM's service account.
	CredentialsFile string
}

// endpoints are Google's, and a test's server in tests.
type endpoints struct {
	metadata, iamCredentials, iam, compute string
}

var google = endpoints{
	metadata:       "http://metadata.google.internal",
	iamCredentials: "https://iamcredentials.googleapis.com",
	iam:            "https://iam.googleapis.com",
	compute:        "https://compute.googleapis.com",
}

type token struct {
	value string
	exp   time.Time
}

func (t token) fresh() bool { return t.value != "" && time.Until(t.exp) > 5*time.Minute }

// Provider is Google Cloud with one identity.
type Provider struct {
	opts Options
	ep   endpoints
	hc   *http.Client

	mu       sync.Mutex
	base     token
	email    string
	source   string
	sessions map[string]token
	inv      map[string]*projectInventory
}

// New makes a provider. Nothing is read until it is first asked.
func New(opts Options) *Provider {
	return &Provider{opts: opts, ep: google, hc: &http.Client{Timeout: 30 * time.Second},
		sessions: map[string]token{}, inv: map[string]*projectInventory{}}
}

func (p *Provider) Kind() string { return "gcp" }

// keyFile is the parts of a service account key Argus uses.
type keyFile struct {
	Type         string `json:"type"`
	ClientEmail  string `json:"client_email"`
	PrivateKey   string `json:"private_key"`
	PrivateKeyID string `json:"private_key_id"`
	TokenURI     string `json:"token_uri"`
}

// ParseKey reads a service account key, refusing anything else Google
// issues as "credentials": a user's refresh token would make every project
// trust a person rather than this control plane.
func ParseKey(b []byte) (email string, err error) {
	k, _, err := parseKey(b)
	return k.ClientEmail, err
}

func parseKey(b []byte) (keyFile, *rsa.PrivateKey, error) {
	var k keyFile
	if err := json.Unmarshal(b, &k); err != nil {
		return k, nil, errors.New("that is not a service account key file (JSON)")
	}
	if k.Type != "service_account" {
		return k, nil, fmt.Errorf("that is a %q credential, not a service account key; create a key for a service account under IAM › Service accounts › Keys", k.Type)
	}
	if k.ClientEmail == "" || k.PrivateKey == "" {
		return k, nil, errors.New("the key file has no client_email or private_key")
	}
	blk, _ := pem.Decode([]byte(k.PrivateKey))
	if blk == nil {
		return k, nil, errors.New("the key file's private_key is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		if rk, err2 := x509.ParsePKCS1PrivateKey(blk.Bytes); err2 == nil {
			parsed = rk
		} else {
			return k, nil, fmt.Errorf("the key file's private_key: %w", err)
		}
	}
	rk, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return k, nil, errors.New("the key file's private_key is not an RSA key")
	}
	if k.TokenURI == "" {
		k.TokenURI = "https://oauth2.googleapis.com/token"
	}
	return k, rk, nil
}

// keyPath is the key file in use, or "" for the VM's account.
func (p *Provider) keyPath() string {
	if p.opts.CredentialsFile != "" {
		return p.opts.CredentialsFile
	}
	return os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
}

// baseToken is argus-control's own token, cached until it is near expiry.
func (p *Provider) baseToken(ctx context.Context) (token, string, error) {
	p.mu.Lock()
	if p.base.fresh() {
		t, e := p.base, p.email
		p.mu.Unlock()
		return t, e, nil
	}
	p.mu.Unlock()
	var (
		t      token
		email  string
		source string
		err    error
	)
	if path := p.keyPath(); path != "" {
		t, email, err = p.fromKey(ctx, path)
		source = "the service account key in " + path
	} else {
		t, email, err = p.fromMetadata(ctx)
		source = "the service account of the VM argus-control runs on"
	}
	if err != nil {
		return token{}, "", fmt.Errorf("%w: %s", cloud.ErrNoBase, err.Error())
	}
	p.mu.Lock()
	p.base, p.email, p.source = t, email, source
	p.mu.Unlock()
	return t, email, nil
}

func (p *Provider) fromKey(ctx context.Context, path string) (token, string, error) {
	b, err := secrets.ReadPrivate(path)
	if err != nil {
		return token{}, "", err
	}
	k, rk, err := parseKey(b)
	if err != nil {
		return token{}, "", err
	}
	now := time.Now()
	head, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": k.PrivateKeyID})
	claims, _ := json.Marshal(map[string]any{"iss": k.ClientEmail, "scope": scope, "aud": k.TokenURI,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix()})
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(head) + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, rk, crypto.SHA256, sum[:])
	if err != nil {
		return token{}, "", err
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion": {signing + "." + enc.EncodeToString(sig)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, k.TokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return token{}, "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := p.do(req, &out); err != nil {
		return token{}, "", fmt.Errorf("Google refused the key for %s: %w", k.ClientEmail, err)
	}
	return token{out.AccessToken, now.Add(time.Duration(out.ExpiresIn) * time.Second)}, k.ClientEmail, nil
}

func (p *Provider) fromMetadata(ctx context.Context) (token, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	get := func(path string, v any) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.ep.metadata+"/computeMetadata/v1/instance/service-accounts/default/"+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Metadata-Flavor", "Google")
		return p.do(req, v)
	}
	var email string
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.ep.metadata+"/computeMetadata/v1/instance/service-accounts/default/email", nil)
	if err != nil {
		return token{}, "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := p.hc.Do(req)
	if err != nil {
		return token{}, "", errors.New("argus-control is not on a Google Cloud VM, and no service account key is set")
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return token{}, "", errors.New("this VM has no service account attached")
	}
	email = strings.TrimSpace(string(b))
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := get("token", &out); err != nil {
		return token{}, "", fmt.Errorf("the VM's service account %s gave no token: %w", email, err)
	}
	return token{out.AccessToken, time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)}, email, nil
}

// Base says who argus-control is to Google.
func (p *Provider) Base(ctx context.Context) (cloud.Base, error) {
	_, email, err := p.baseToken(ctx)
	if err != nil {
		return cloud.Base{}, err
	}
	p.mu.Lock()
	src := p.source
	p.mu.Unlock()
	return cloud.Base{Principal: email, ARN: email, Source: src}, nil
}

// session is a token for the project's service account, minted with
// argus-control's own and checked for this Argus's external id before it is
// used or kept. Cached until it is near expiry.
func (p *Provider) session(ctx context.Context, a cloud.Account, actor cloud.Actor) (token, error) {
	if !actor.Valid() {
		return token{}, fmt.Errorf("%q cannot be named in Google's log", string(actor))
	}
	if !ValidProject(a.AccountID) {
		return token{}, errors.New("a project id is 6 to 30 lower case letters, digits and dashes")
	}
	if !ValidAccountEmail(a.RoleARN) || ProjectOf(a.RoleARN) != a.AccountID {
		return token{}, fmt.Errorf("%s is not a service account in project %s", a.RoleARN, a.AccountID)
	}
	key := a.AccountID + "|" + a.RoleARN + "|" + a.ExternalID
	p.mu.Lock()
	if t, ok := p.sessions[key]; ok && t.fresh() {
		p.mu.Unlock()
		return t, nil
	}
	p.mu.Unlock()
	bt, _, err := p.baseToken(ctx)
	if err != nil {
		return token{}, err
	}
	body, _ := json.Marshal(map[string]any{"scope": []string{scope}, "lifetime": "3600s"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.ep.iamCredentials+"/v1/projects/-/serviceAccounts/"+url.PathEscape(a.RoleARN)+":generateAccessToken", bytes.NewReader(body))
	if err != nil {
		return token{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bt.value)
	req.Header.Set("User-Agent", userAgent(actor))
	var out struct {
		AccessToken string    `json:"accessToken"`
		ExpireTime  time.Time `json:"expireTime"`
	}
	if err := p.do(req, &out); err != nil {
		return token{}, fmt.Errorf("Google did not let argus-control act as %s: %w", a.RoleARN, err)
	}
	t := token{out.AccessToken, out.ExpireTime}
	var sa struct {
		Email       string `json:"email"`
		ProjectID   string `json:"projectId"`
		Description string `json:"description"`
		Disabled    bool   `json:"disabled"`
	}
	if err := p.get(ctx, t, actor, p.ep.iam+"/v1/projects/"+a.AccountID+"/serviceAccounts/"+url.PathEscape(a.RoleARN), &sa); err != nil {
		return token{}, fmt.Errorf("%s cannot read its own description: %w", a.RoleARN, err)
	}
	if sa.ProjectID != a.AccountID {
		return token{}, fmt.Errorf("%s is in project %s, not %s", a.RoleARN, sa.ProjectID, a.AccountID)
	}
	if !hasMarker(sa.Description, a.ExternalID) {
		return token{}, fmt.Errorf("%s was not made for this Argus: its description does not carry this Argus's external id. Run the command from the connect page again", a.RoleARN)
	}
	p.mu.Lock()
	if len(p.sessions) >= maxSessions {
		for k, v := range p.sessions {
			if !v.fresh() || len(p.sessions) >= maxSessions {
				delete(p.sessions, k)
			}
		}
	}
	p.sessions[key] = t
	p.mu.Unlock()
	return t, nil
}

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

// userAgent names the person a call runs for. Google records it in the
// audit log as the caller-supplied user agent, beside the service account.
func userAgent(actor cloud.Actor) string { return "argus-control (for " + string(actor) + ")" }

// Verify acts as the project's account as actor and reports what it found:
// the regions Compute Engine has, and "global" for what is in none.
func (p *Provider) Verify(ctx context.Context, a cloud.Account, actor cloud.Actor) (cloud.Identity, error) {
	t, err := p.session(ctx, a, actor)
	if err != nil {
		return cloud.Identity{}, err
	}
	var regs struct {
		Items []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"items"`
	}
	if err := p.get(ctx, t, actor, p.ep.compute+"/compute/v1/projects/"+a.AccountID+"/regions?fields=items(name,status)", &regs); err != nil {
		return cloud.Identity{}, fmt.Errorf("%s cannot list Compute Engine regions: %w", a.RoleARN, err)
	}
	var out []string
	for _, r := range regs.Items {
		if r.Status == "" || r.Status == "UP" {
			out = append(out, r.Name)
		}
	}
	sort.Strings(out)
	return cloud.Identity{ARN: a.RoleARN, AccountID: a.AccountID, Regions: append(out, GlobalRegion)}, nil
}

// GlobalRegion holds what Google keeps in no region: networks, firewalls,
// snapshots and images.
const GlobalRegion = "global"

func (p *Provider) get(ctx context.Context, t token, actor cloud.Actor, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+t.value)
	req.Header.Set("User-Agent", userAgent(actor))
	return p.do(req, v)
}

// do sends req and decodes a 200 into v, or Google's own message.
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
		return googleError(resp.StatusCode, b)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("Google's answer: %w", err)
	}
	return nil
}

// googleError is Google's message, in its words, with a next step for the
// refusals people meet while setting up.
func googleError(code int, b []byte) error {
	var e struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.Unmarshal(b, &e)
	msg := e.Error.Message
	if msg == "" {
		msg = e.Description
	}
	if msg == "" {
		msg = http.StatusText(code)
	}
	if i := strings.Index(msg, "\n"); i > 0 {
		msg = msg[:i]
	}
	switch {
	case strings.Contains(msg, "has not been used in project") || strings.Contains(msg, "it is disabled"):
		msg += " (turn the API on, then wait a minute)"
	case strings.Contains(msg, "insufficient authentication scopes"), strings.Contains(msg, "ACCESS_TOKEN_SCOPE_INSUFFICIENT"):
		msg += " (give the VM the cloud-platform access scope)"
	}
	return errors.New(msg)
}
