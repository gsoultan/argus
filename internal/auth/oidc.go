package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDCConfig describes the identity provider.
type OIDCConfig struct {
	Issuer       string   `yaml:"issuer"`
	ClientID     string   `yaml:"client_id"`
	ClientSecret string   `yaml:"client_secret"`
	RedirectURL  string   `yaml:"redirect_url"`
	Scopes       []string `yaml:"scopes"`

	// RoleClaim is the claim carrying group membership.
	RoleClaim string `yaml:"role_claim"`
	// RoleMap translates an IdP group to an Argus role. A user whose groups
	// match nothing gets DefaultRole — which should be the least privileged
	// one, so a misconfigured mapping fails closed.
	RoleMap     map[string]string `yaml:"role_map"`
	DefaultRole string            `yaml:"default_role"`
}

// OIDC handles the authorization code flow.
type OIDC struct {
	cfg      OIDCConfig
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauth    oauth2.Config
}

// NewOIDC discovers the provider and prepares the flow.
func NewOIDC(ctx context.Context, cfg OIDCConfig) (*OIDC, error) {
	if cfg.Issuer == "" {
		return nil, nil // not configured; caller falls back to static tokens
	}
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery for %s: %w", cfg.Issuer, err)
	}
	if cfg.DefaultRole == "" {
		cfg.DefaultRole = "auditor" // least privileged: read, never connect
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{oidc.ScopeOpenID, "profile", "email", "groups"}
	}

	return &OIDC{
		cfg:      cfg,
		provider: provider,
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		oauth: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Endpoint:     provider.Endpoint(),
			Scopes:       cfg.Scopes,
		},
	}, nil
}

// flowState is the short-lived state carried across the redirect, signed into a
// cookie so the server keeps no per-login memory.
type flowState struct {
	State     string    `json:"state"`
	Nonce     string    `json:"nonce"`
	Verifier  string    `json:"verifier"`
	ReturnTo  string    `json:"return_to"`
	ExpiresAt time.Time `json:"exp"`
}

// AuthCodeURL begins a login, returning the redirect target and the state
// cookie value that must accompany it.
func (o *OIDC) AuthCodeURL(signer *Signer, returnTo string) (redirect, stateCookie string, err error) {
	state, err := randomID()
	if err != nil {
		return "", "", err
	}
	nonce, err := randomID()
	if err != nil {
		return "", "", err
	}
	// PKCE, even though this is a confidential client. It costs nothing and
	// removes an entire class of code-interception attack if the secret ever
	// leaks or the client is later reused from a public context.
	verifier := oauth2.GenerateVerifier()

	sc, err := signer.sign(flowState{
		State:     state,
		Nonce:     nonce,
		Verifier:  verifier,
		ReturnTo:  returnTo,
		ExpiresAt: time.Now().UTC().Add(10 * time.Minute),
	})
	if err != nil {
		return "", "", err
	}

	url := o.oauth.AuthCodeURL(state,
		oidc.Nonce(nonce),
		oauth2.S256ChallengeOption(verifier),
	)
	return url, sc, nil
}

// Claims is what Argus reads out of an ID token.
type Claims struct {
	Subject       string   `json:"sub"`
	Email         string   `json:"email"`
	EmailVerified bool     `json:"email_verified"`
	Name          string   `json:"name"`
	Groups        []string `json:"groups"`
}

// Exchange completes the flow and returns the authenticated identity.
func (o *OIDC) Exchange(ctx context.Context, signer *Signer,
	code, state, stateCookie string) (Session, string, error) {

	var fs flowState
	if err := signer.verify(stateCookie, &fs); err != nil {
		return Session{}, "", fmt.Errorf("login state is invalid: %w", err)
	}
	if time.Now().After(fs.ExpiresAt) {
		return Session{}, "", fmt.Errorf("login took too long, please try again")
	}
	// CSRF: without this, an attacker can complete a login in the victim's
	// browser using their own code and silently take over the session.
	if subtleCompare(fs.State, state) != 1 {
		return Session{}, "", fmt.Errorf("login state mismatch")
	}

	token, err := o.oauth.Exchange(ctx, code, oauth2.VerifierOption(fs.Verifier))
	if err != nil {
		return Session{}, "", fmt.Errorf("code exchange: %w", err)
	}

	rawID, ok := token.Extra("id_token").(string)
	if !ok {
		return Session{}, "", fmt.Errorf("no id_token in response")
	}
	idToken, err := o.verifier.Verify(ctx, rawID)
	if err != nil {
		return Session{}, "", fmt.Errorf("verify id_token: %w", err)
	}
	// Replay protection: a captured id_token cannot be reused in a new flow.
	if subtleCompare(idToken.Nonce, fs.Nonce) != 1 {
		return Session{}, "", fmt.Errorf("id_token nonce mismatch")
	}

	var c Claims
	if err := idToken.Claims(&c); err != nil {
		return Session{}, "", fmt.Errorf("parse claims: %w", err)
	}
	// Providers disagree on where group membership lives — "groups", "roles",
	// or a namespaced claim. Read the configured one rather than assuming.
	if claim := o.cfg.RoleClaim; claim != "" && claim != "groups" {
		var raw map[string]any
		if err := idToken.Claims(&raw); err == nil {
			c.Groups = append(c.Groups, stringsFrom(raw[claim])...)
		}
	}
	if c.Email == "" {
		return Session{}, "", fmt.Errorf("id_token has no email claim; Argus attributes every action to a person")
	}

	return Session{
		Email:   strings.ToLower(c.Email),
		Name:    firstNonEmpty(c.Name, c.Email),
		Role:    o.mapRole(c),
		Subject: c.Subject,
	}, fs.ReturnTo, nil
}

// mapRole translates IdP groups to an Argus role.
//
// Most privileged match wins, so someone in both "argus-admins" and
// "engineering" gets admin rather than whichever the map happened to iterate
// first. An unmapped user gets the default, which is read-only.
func (o *OIDC) mapRole(c Claims) string {
	rank := map[string]int{"auditor": 0, "operator": 1, "approver": 2, "admin": 3, "owner": 4}
	best := o.cfg.DefaultRole

	for _, g := range c.Groups {
		if role, ok := o.cfg.RoleMap[g]; ok {
			if rank[role] > rank[best] {
				best = role
			}
		}
	}
	return best
}

// StateCookieName is where the signed flow state lives during a login.
const StateCookieName = "argus_login"

// SessionCookieName is the console's session.
const SessionCookieName = "argus_session"

// SetCookie writes a hardened cookie.
func SetCookie(w http.ResponseWriter, name, value string, ttl time.Duration, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:  name,
		Value: value,
		Path:  "/",
		// HttpOnly: the console never needs to read this from JavaScript, and
		// keeping it out of reach means an XSS bug does not become a session
		// theft.
		HttpOnly: true,
		Secure:   secure,
		// Lax rather than Strict: Strict would drop the cookie on the redirect
		// back from the IdP, breaking login entirely.
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ttl.Seconds()),
	})
}

// ClearCookie expires a cookie.
func ClearCookie(w http.ResponseWriter, name string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/",
		HttpOnly: true, Secure: secure,
		SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

// stringsFrom coerces a claim that may be a string or a list of them.
func stringsFrom(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func subtleCompare(a, b string) int {
	if len(a) != len(b) {
		return 0
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	if diff == 0 {
		return 1
	}
	return 0
}
