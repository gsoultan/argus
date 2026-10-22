package control

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gsoultan/argus/internal/auth"
)

/*
First-run setup in the browser.

A fresh install holds no accounts, and until now the only way to make the first
one was `argus-control users add` on the host. That stays. This is the same act
from the console, at /setup, for an operator who has the host but would rather
not type a password into an SSH session -- or who reaches the console through a
Cloudflare Tunnel before they have a shell anywhere convenient.

It must not become the thing local-authentication.md forbids: a way in that
nobody was given. An open "create the first administrator" form on a fresh
install is exactly that, raced by whoever finds the address first, and behind a
tunnel the address is on the public internet from the moment it is created.

So the form is bound to the host anyway. At start-up, with no accounts, the
control plane writes a one-time token to a file only its own user can read and
says where in its log. The form demands it. Reading that file is the same
privilege boundary `users add` relies on -- shell on the host -- only exercised
once, with a copy and paste, instead of for the whole command.

And it closes for good. Setup is complete the moment any account has ever had a
password, disabled or not, so disabling everyone does not reopen it; the check
is made inside a transaction holding a lock, so two browsers submitting at once
make one account, not two; and the token is forgotten and its file removed as
soon as it has been used.
*/

// ErrSetupComplete means an account already exists, so there is nothing to set up.
var ErrSetupComplete = errors.New("setup is already complete: this control plane has an account")

// setupGate holds the one-time setup token while setup is open.
type setupGate struct {
	mu    sync.Mutex
	token string
	file  string
	// done is set once the first account exists, for the hot path in
	// authenticate that must not take a lock or a database round trip.
	done atomic.Bool
}

// NewSetupToken returns a fresh one-time setup token: 160 bits, in a form that
// survives being read off a terminal and typed.
func NewSetupToken() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate setup token: %w", err)
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)), nil
}

// OfferSetup opens /setup, guarded by token. file is where the token was
// written, so the page can tell the operator where to find it.
func (a *API) OfferSetup(token, file string) {
	a.setup.mu.Lock()
	defer a.setup.mu.Unlock()
	a.setup.token, a.setup.file = token, file
}

// setupOffer returns the token and its file, empty when setup is not offered.
func (a *API) setupOffer() (token, file string) {
	a.setup.mu.Lock()
	defer a.setup.mu.Unlock()
	return a.setup.token, a.setup.file
}

// closeSetup forgets the token and removes its file. Called once the first
// account exists, by whichever path made it.
func (a *API) closeSetup() {
	a.setup.done.Store(true)
	a.setup.mu.Lock()
	file := a.setup.file
	a.setup.token, a.setup.file = "", ""
	a.setup.mu.Unlock()
	if file != "" {
		if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
			// Harmless once setup is closed -- the token opens nothing now --
			// but worth saying, so nobody wonders later what the file is for.
			a.log.Warn("could not remove the used setup token file", "file", file, "error", err)
		}
	}
}

// setupStatus is what the /setup page needs to decide what to draw.
type setupStatus struct {
	// Complete is true once any account has existed. The page then shows the
	// way to sign in rather than a form that can only refuse.
	Complete bool `json:"complete"`
	// Available is whether this control plane will accept the form. False
	// with Complete also false means it was started without offering it.
	Available bool `json:"available"`
	// TokenFile is where the token was written, so the operator knows which
	// file to read. Only while setup is open.
	TokenFile string `json:"tokenFile,omitempty"`
}

func (a *API) setupState(ctx context.Context) (setupStatus, error) {
	if a.setup.done.Load() {
		return setupStatus{Complete: true}, nil
	}
	complete, err := a.store.SetupComplete(ctx)
	if err != nil {
		return setupStatus{}, err
	}
	if complete {
		return setupStatus{Complete: true}, nil
	}
	token, file := a.setupOffer()
	return setupStatus{Available: token != "" && a.signer != nil, TokenFile: file}, nil
}

// handleSetupStatus serves GET /auth/setup.
func (a *API) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	st, err := a.setupState(r.Context())
	if err != nil {
		a.fail(w, "setup status", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, st)
}

type setupRequest struct {
	Token       string `json:"token"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	Password    string `json:"password"`
}

// handleSetup serves POST /auth/setup: the first account, once.
func (a *API) handleSetup(w http.ResponseWriter, r *http.Request) {
	if a.setup.done.Load() {
		writeErr(w, http.StatusConflict, ErrSetupComplete.Error())
		return
	}
	if a.signer == nil {
		writeErr(w, http.StatusServiceUnavailable,
			"this control plane cannot sign anyone in: set signing_secret, restart it, and run setup again")
		return
	}
	var in setupRequest
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	token, file := a.setupOffer()
	if token == "" {
		writeErr(w, http.StatusForbidden,
			"this control plane is not offering setup in the browser; create the first "+
				"account on the host with: argus-control users add <email>")
		return
	}
	// Before anything else is looked at, and charged like a wrong password: a
	// form that answered "email invalid" to a caller without the token would
	// be a free oracle for whoever is racing the operator to it.
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(in.Token)), []byte(token)) != 1 {
		a.RecordAuthFailure(r)
		a.log.Warn("setup refused: wrong setup token", "client", a.clientAddr(r))
		writeErr(w, http.StatusUnauthorized,
			"that is not this control plane's setup token; read it from "+file+" on the host")
		return
	}

	email := normaliseEmail(in.Email)
	if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email {
		writeErr(w, http.StatusBadRequest, "enter an email address, such as you@example.com")
		return
	}
	if len([]rune(in.Password)) < auth.MinPasswordLength {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("the password must be at least %d characters", auth.MinPasswordLength))
		return
	}

	const role = "admin"
	switch _, err := a.store.CreateFirstAccount(r.Context(), email,
		strings.TrimSpace(in.DisplayName), role, in.Password); {
	case errors.Is(err, ErrSetupComplete):
		// Someone else finished first -- another tab, or `users add` on the
		// host. Closed either way.
		a.closeSetup()
		writeErr(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		a.fail(w, "setup", err)
		return
	}
	a.closeSetup()
	a.RecordAuthSuccess(r)

	client := a.clientAddr(r)
	a.log.Info("first account created through the setup page",
		"email", email, "role", role, "client", client)
	if _, err := a.store.AppendAudit(r.Context(), AuditEvent{
		Action: "user.created", Severity: "notice", ActorEmail: email,
		Target: email,
		Detail: "First local account created through the console's setup page with role " +
			role + ", from " + client + ", using the one-time setup token from the host. " +
			"Setup is now closed.",
	}); err != nil {
		// The account exists; failing the request would report the opposite.
		a.log.Error("audit append failed", "error", err)
	}
	writeJSON(w, http.StatusCreated, map[string]string{"email": email, "role": role})
}

// SetupComplete reports whether any account has ever been given a password.
//
// Not AnyAccountExists: that ignores disabled accounts, so it answers "can
// anyone sign in right now", and setup reopening because every account was
// disabled would hand the deployment to whoever reached /setup next.
func (s *Store) SetupComplete(ctx context.Context) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE password_hash IS NOT NULL)`).Scan(&exists)
	return exists, err
}

// CreateFirstAccount creates an account only if none has ever existed.
//
// The check and the insert are one transaction under an advisory lock, so two
// requests racing each other -- or the setup page racing `users add` -- cannot
// both see an empty table and both succeed.
func (s *Store) CreateFirstAccount(ctx context.Context, email, displayName, role, password string) (string, error) {
	// Hashed before the lock is taken: argon2 is deliberately slow, and holding
	// a lock across it would serialise nothing useful.
	hash, err := auth.HashPassword(password)
	if err != nil {
		return "", err
	}
	return s.createFirstAccountHashed(ctx, email, displayName, role, hash)
}

// createFirstAccountHashed is CreateFirstAccount after the slow part, so a test
// can make the racing transactions actually overlap.
func (s *Store) createFirstAccountHashed(ctx context.Context, email, displayName, role, hash string) (string, error) {
	email = normaliseEmail(email)
	if displayName == "" {
		displayName = strings.SplitN(email, "@", 2)[0]
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('argus_first_account'))`); err != nil {
		return "", fmt.Errorf("lock for setup: %w", err)
	}
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE password_hash IS NOT NULL)`).Scan(&exists); err != nil {
		return "", err
	}
	if exists {
		return "", ErrSetupComplete
	}
	var id string
	// A row with this address and no password can exist -- users are created
	// by more than sign-in -- and giving it one is creating the account.
	if err := tx.QueryRow(ctx, `
		INSERT INTO users (email, display_name, role, password_hash)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (email) DO UPDATE
		   SET display_name = EXCLUDED.display_name,
		       role         = EXCLUDED.role,
		       password_hash = EXCLUDED.password_hash
		RETURNING id::text`, email, displayName, role, hash).Scan(&id); err != nil {
		return "", fmt.Errorf("create first account: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return id, nil
}
