package notify

import (
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
	"strings"
	"sync"
	"time"

	"github.com/gsoultan/argus/internal/secrets"
)

// FCMConfig is the `alerts.fcm` block of the control plane's config.
//
// Firebase Cloud Messaging is what puts an alert on a phone as a push
// notification, through an app the administrator already runs. Messages go to
// a topic the app subscribes to, to individual device tokens, or both.
type FCMConfig struct {
	// CredentialsFile is the service account key downloaded from the Firebase
	// console (Project settings → Service accounts). It holds a private key, so
	// it must be readable by the owner alone, like every other key Argus loads.
	CredentialsFile string `yaml:"credentials_file"`
	// ProjectID overrides the project named in the credentials file.
	ProjectID string `yaml:"project_id"`
	// Topic is a topic name, without the "/topics/" prefix.
	Topic string `yaml:"topic"`
	// Tokens are registration tokens of individual devices.
	Tokens []string `yaml:"tokens"`
}

// serviceAccount is the part of a Google service account key that FCM needs.
type serviceAccount struct {
	Type        string `json:"type"`
	ProjectID   string `json:"project_id"`
	PrivateKey  string `json:"private_key"`
	ClientEmail string `json:"client_email"`
	TokenURI    string `json:"token_uri"`
}

// FCM sends alerts as push notifications through the FCM HTTP v1 API.
//
// The OAuth exchange is done here with the standard library rather than by
// pulling in Google's client libraries: it is one signed JWT and one POST, and
// a PAM control plane should not grow a dependency tree for a notification.
type FCM struct {
	project  string
	email    string
	key      *rsa.PrivateKey
	tokenURI string
	targets  []fcmTarget
	client   *http.Client
	// api is FCM's base URL; replaced in tests.
	api string
	now func() time.Time

	mu      sync.Mutex
	access  string
	expires time.Time
}

type fcmTarget struct {
	kind  string // "topic" or "token"
	value string
}

// label names a target in an error without writing a device token, which is
// an address anyone can push to, into the log in full.
func (t fcmTarget) label() string {
	if t.kind == "topic" {
		return "topic " + t.value
	}
	v := t.value
	if len(v) > 8 {
		v = v[:8] + "…"
	}
	return "device " + v
}

const fcmScope = "https://www.googleapis.com/auth/firebase.messaging"

// NewFCM checks the config, loads the service account and returns a sender.
func NewFCM(cfg FCMConfig, client *http.Client) (*FCM, error) {
	var missing []string
	if cfg.CredentialsFile == "" {
		missing = append(missing, "credentials_file")
	}
	if cfg.Topic == "" && len(cfg.Tokens) == 0 {
		missing = append(missing, "topic or tokens")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("alerts.fcm: %w: set %s", ErrNotConfigured, strings.Join(missing, ", "))
	}

	raw, err := secrets.ReadPrivate(cfg.CredentialsFile)
	if err != nil {
		return nil, fmt.Errorf("alerts.fcm: %w", err)
	}
	var sa serviceAccount
	if err := json.Unmarshal(raw, &sa); err != nil {
		return nil, fmt.Errorf("alerts.fcm: %s is not a service account key: %w", cfg.CredentialsFile, err)
	}
	if sa.Type != "service_account" || sa.ClientEmail == "" || sa.PrivateKey == "" {
		return nil, fmt.Errorf("alerts.fcm: %s is not a service account key "+
			"(download one from Firebase: Project settings, Service accounts)", cfg.CredentialsFile)
	}
	key, err := parseRSAKey(sa.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("alerts.fcm: %s: %w", cfg.CredentialsFile, err)
	}
	project := cfg.ProjectID
	if project == "" {
		project = sa.ProjectID
	}
	if project == "" {
		return nil, fmt.Errorf("alerts.fcm: %w: set project_id, the credentials file names none", ErrNotConfigured)
	}
	tokenURI := sa.TokenURI
	if tokenURI == "" {
		tokenURI = "https://oauth2.googleapis.com/token"
	}

	var targets []fcmTarget
	if cfg.Topic != "" {
		targets = append(targets, fcmTarget{"topic", strings.TrimPrefix(cfg.Topic, "/topics/")})
	}
	for _, t := range cfg.Tokens {
		targets = append(targets, fcmTarget{"token", t})
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &FCM{
		project:  project,
		email:    sa.ClientEmail,
		key:      key,
		tokenURI: tokenURI,
		targets:  targets,
		client:   client,
		api:      "https://fcm.googleapis.com",
		now:      time.Now,
	}, nil
}

func parseRSAKey(p string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(p))
	if block == nil {
		return nil, errors.New("private_key is not PEM")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("private_key is not an RSA key")
		}
		return rk, nil
	}
	k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("private_key could not be parsed")
	}
	return k, nil
}

// Name implements Sender.
func (f *FCM) Name() string { return "fcm" }

// Send implements Sender. The subject becomes the notification's text, since a
// lock screen shows a line or two; the session's id and host ride along as
// data for an app that wants to open the session itself. Every target is
// tried; the error names the ones that failed.
func (f *FCM) Send(ctx context.Context, subject, body string) error {
	token, err := f.accessToken(ctx)
	if err != nil {
		return err
	}
	text := strings.TrimPrefix(subject, "Argus: ")
	var errs []error
	for _, t := range f.targets {
		if err := f.post(ctx, token, t, text, body); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.label(), err))
		}
	}
	return errors.Join(errs...)
}

func (f *FCM) post(ctx context.Context, token string, t fcmTarget, text, body string) error {
	msg := map[string]any{
		"notification": map[string]string{"title": "Argus", "body": text},
		// Data values must be strings. The full body is there for an app that
		// shows the detail; FCM caps the whole message at 4 KB.
		"data":    map[string]string{"detail": truncate(body, 2000)},
		"android": map[string]any{"priority": "high"},
		"apns":    map[string]any{"headers": map[string]string{"apns-priority": "10"}},
	}
	msg[t.kind] = t.value
	payload, err := json.Marshal(map[string]any{"message": msg})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		f.api+"/v1/projects/"+url.PathEscape(f.project)+"/messages:send", strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := f.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil
	}
	if resp.StatusCode == http.StatusUnauthorized {
		// Revoked or rotated underneath us: fetch a fresh one next time rather
		// than failing every alert until the cached token would have expired.
		f.mu.Lock()
		f.access = ""
		f.mu.Unlock()
	}
	return fmt.Errorf("fcm answered %s: %s", resp.Status, googleError(resp.Body))
}

// accessToken returns a cached OAuth token, or exchanges a signed JWT for one.
func (f *FCM) accessToken(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	if f.access != "" && now.Before(f.expires) {
		return f.access, nil
	}

	assertion, err := f.assertion(now)
	if err != nil {
		return "", err
	}
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.tokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := f.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fcm sign-in: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fcm sign-in refused (%s): %s", resp.Status, googleError(resp.Body))
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil || out.AccessToken == "" {
		return "", errors.New("fcm sign-in answered without an access token")
	}
	// A minute early, so a token never expires between this check and FCM
	// reading it.
	life := time.Duration(out.ExpiresIn)*time.Second - time.Minute
	if life < 0 {
		life = 0
	}
	f.access, f.expires = out.AccessToken, now.Add(life)
	return f.access, nil
}

// assertion is the RS256 JWT a service account signs to ask for a token.
func (f *FCM) assertion(now time.Time) (string, error) {
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iss":   f.email,
		"scope": fcmScope,
		"aud":   f.tokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	})
	if err != nil {
		return "", err
	}
	signing := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("fcm: sign assertion: %w", err)
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// googleError pulls the human-readable part out of a Google API error body.
func googleError(r io.Reader) string {
	raw, _ := io.ReadAll(io.LimitReader(r, 64<<10))
	var out struct {
		Error any `json:"error"`
		// The token endpoint uses OAuth's shape instead.
		Description string `json:"error_description"`
	}
	if json.Unmarshal(raw, &out) == nil {
		if out.Description != "" {
			return out.Description
		}
		if m, ok := out.Error.(map[string]any); ok {
			if s, ok := m["message"].(string); ok && s != "" {
				return s
			}
		}
		if s, ok := out.Error.(string); ok && s != "" {
			return s
		}
	}
	return "no detail given"
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
