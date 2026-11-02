package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/gsoultan/argus/internal/auth"
	"github.com/gsoultan/argus/internal/control"
)

// `argus-control users add` creates the first account.
//
// Every deployment needs one person who can sign in before anyone can be
// invited, and a product that seeds a default administrator with a known
// password has shipped a backdoor. So the first account is made deliberately,
// on the host, by someone who already has shell access there -- which is a
// privilege boundary that already exists rather than a new one.
//
// The password is read from the terminal without echo, so it does not land in
// shell history, in a process list, or in whatever ships the logs.
func runUsersAdd(configPath string, args []string) error {
	email := ""
	role := "admin"
	name := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--role":
			if i+1 >= len(args) {
				return errors.New("--role needs a value")
			}
			i++
			role = args[i]
		case "--name":
			if i+1 >= len(args) {
				return errors.New("--name needs a value")
			}
			i++
			name = args[i]
		default:
			if strings.HasPrefix(args[i], "-") {
				return fmt.Errorf("unknown flag %q", args[i])
			}
			email = args[i]
		}
	}
	if email == "" {
		return errors.New("usage: argus-control users add <email> [--role admin|owner|approver|operator|auditor] [--name \"Full Name\"]")
	}
	switch role {
	case "owner", "admin", "approver", "operator", "auditor":
	default:
		return fmt.Errorf("unknown role %q", role)
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store, err := control.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer store.Close()

	// CreateAccount overwrites an existing row's password and role. From here
	// that was a silent way to reset somebody, leaving their sessions and
	// second factor as they were and the audit saying "created". Recovery has
	// its own commands, each of which says what it did.
	if existing, err := store.Account(ctx, email); err == nil && existing.PasswordHash != "" {
		return fmt.Errorf("%s already has an account.\n"+
			"To recover it use: argus-control users set-password|reset-mfa|set-role|enable %s",
			email, email)
	} else if err != nil && !errors.Is(err, control.ErrNoAccount) {
		return err
	}

	password, err := readPassword()
	if err != nil {
		return err
	}
	if len([]rune(password)) < auth.MinPasswordLength {
		return fmt.Errorf("the password must be at least %d characters", auth.MinPasswordLength)
	}

	if _, err := store.CreateAccount(ctx, email, name, role, password); err != nil {
		return err
	}
	if _, err := store.AppendAudit(ctx, control.AuditEvent{
		Action: "user.created", Severity: "notice", ActorEmail: "console",
		Target: email,
		Detail: "Local account created from the host with role " + role + ".",
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: account created but not audited: %v\n", err)
	}

	fmt.Printf("\nCreated %s as %s.\n\n", email, role)
	fmt.Printf("  Sign in at the console and enrol a second factor straight away —\n")
	fmt.Printf("  a privileged account with a password and nothing else is one phish\n")
	fmt.Printf("  away from every host Argus brokers.\n\n")
	return nil
}

// readPassword prompts twice, without echo.
func readPassword() (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		// Piped input: read one line and use it. Useful for provisioning, and
		// the operator has chosen to take responsibility for where it came from.
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("read password: %w", err)
		}
		return strings.TrimRight(line, "\r\n"), nil
	}

	fmt.Print("Password: ")
	first, err := term.ReadPassword(fd)
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	fmt.Print("Repeat:   ")
	second, err := term.ReadPassword(fd)
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	if string(first) != string(second) {
		return "", errors.New("the two passwords do not match")
	}
	return string(first), nil
}

// `argus-control users list` shows who can sign in, and who used to.
//
// Disabled accounts are listed rather than hidden: an operator asking who has
// access needs to see that an account exists and is revoked, not to be shown a
// list it is missing from and conclude it was never there.
func runUsersList(configPath string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store, err := control.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer store.Close()

	accounts, err := store.Accounts(ctx)
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		fmt.Println("No accounts. Create one with: argus-control users add <email>")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "EMAIL\tROLE\tMFA\tSTATE")
	for _, a := range accounts {
		mfa := "no"
		if a.MFAEnrolled {
			mfa = "yes"
		}
		state := "active"
		if a.Disabled {
			state = "disabled"
			if a.DisabledAt != nil {
				state += " " + a.DisabledAt.UTC().Format(time.RFC3339)
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", a.Email, a.Role, mfa, state)
	}
	return w.Flush()
}

// `argus-control users disable` and `users enable` revoke and restore access.
//
// The column these set has been read since it was added -- a disabled account
// cannot sign in, does not count toward accountsExist, and does not count as an
// administrator -- and nothing ever wrote it. An account could be created and
// never revoked, which for a product about controlling access is the wrong half
// of the pair to ship.
//
// On the host, like `users add`, and for the same reason: whoever has shell
// there can already reach the database, so this is not a new privilege. What it
// is, is a supported way to do it that leaves an audit entry, instead of an
// UPDATE somebody types from memory at the wrong moment.
func runUsersSetDisabled(configPath string, args []string, disabled bool) error {
	verb := "enable"
	if disabled {
		verb = "disable"
	}
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: argus-control users %s <email>", verb)
	}
	email := args[0]

	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store, err := control.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer store.Close()

	switch err := store.SetAccountDisabled(ctx, email, disabled); {
	case err == nil:
	case errors.Is(err, control.ErrLastAdmin):
		return fmt.Errorf("refusing to disable %s: %w\n"+
			"Give another account the admin or owner role first. A deployment "+
			"nobody can administer is recovered with direct SQL, which is what "+
			"this command exists to avoid needing.", email, err)
	case errors.Is(err, control.ErrAlreadyInState):
		fmt.Printf("%s is already %sd.\n", email, verb)
		return nil
	case errors.Is(err, control.ErrNoAccount):
		return fmt.Errorf("no account for %s", email)
	default:
		return err
	}

	// Written down, because revoking access is exactly the kind of act an
	// investigation asks about afterwards. The actor is the host user: this
	// runs without a console session, and naming the shell that did it is more
	// honest than attributing it to the service.
	if _, err := store.AppendAudit(ctx, control.AuditEvent{
		Action:     "account." + verb + "d",
		Severity:   "notice",
		ActorEmail: hostActor(),
		Target:     email,
		Detail: fmt.Sprintf("Account %s %sd with argus-control on the host.",
			email, verb),
	}); err != nil {
		// The account is already changed; failing here would report the
		// opposite of what happened.
		fmt.Fprintf(os.Stderr, "warning: %sd %s but could not write the audit entry: %v\n",
			verb, email, err)
	}

	fmt.Printf("%sd %s.\n", verb, email)
	return nil
}

// hostActor names whoever ran this, preferring the human behind a sudo.
func hostActor() string {
	if u := os.Getenv("SUDO_USER"); u != "" {
		return u + "@host"
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username + "@host"
	}
	return "unknown@host"
}

/* ── Recovery ────────────────────────────────────────────────────────────── */

// Getting an administrator back in, from the control plane host.
//
// The console cannot do this for the last administrator: the person who would
// click the button is the one who is locked out. So these run where
// `users add` runs, with the same privilege boundary -- a shell on the host
// that can read the configuration file and so reach the database -- and each
// one signs the account out everywhere and lands in the audit chain under the
// host user who ran it. See docs/admin-recovery.md.

// openForRecovery is the common start of every recovery command.
func openForRecovery(configPath string, args []string, usage string) (
	*control.Store, string, context.Context, func(), error) {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return nil, "", nil, nil, errors.New("usage: argus-control users " + usage)
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return nil, "", nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	store, err := control.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		cancel()
		return nil, "", nil, nil, err
	}
	email := strings.ToLower(strings.TrimSpace(args[0]))
	if _, err := store.Account(ctx, email); err != nil {
		store.Close()
		cancel()
		if errors.Is(err, control.ErrNoAccount) {
			return nil, "", nil, nil, fmt.Errorf("no account for %s", email)
		}
		return nil, "", nil, nil, err
	}
	return store, email, ctx, func() { store.Close(); cancel() }, nil
}

// auditRecovery writes the entry every recovery command owes. Severity
// warning: getting into an account by a way other than its credentials is
// what an investigator looks for first.
func auditRecovery(ctx context.Context, store *control.Store, action, email, detail string) {
	if _, err := store.AppendAudit(ctx, control.AuditEvent{
		Action: action, Severity: "warning", ActorEmail: hostActor(),
		Target: email, Detail: detail + " Done with argus-control on the control plane host.",
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: done, but could not write the audit entry: %v\n", err)
	}
}

// `argus-control users reset-mfa <email>`: for an administrator who has lost
// their authenticator and their recovery codes.
func runUsersResetMFA(configPath string, args []string) error {
	store, email, ctx, done, err := openForRecovery(configPath, args, "reset-mfa <email>")
	if err != nil {
		return err
	}
	defer done()
	if err := store.ResetMFA(ctx, email); err != nil {
		return err
	}
	auditRecovery(ctx, store, "auth.mfa_reset", email,
		"Second factor and recovery codes removed; every session signed out.")
	fmt.Printf("Removed the second factor of %s and signed it out everywhere.\n"+
		"At the next sign-in, the console asks them to set up a new one before anything else.\n", email)
	return nil
}

// `argus-control users set-password <email>`: a forgotten password. The new
// one is read without echo, as in `users add`.
func runUsersSetPassword(configPath string, args []string) error {
	store, email, ctx, done, err := openForRecovery(configPath, args, "set-password <email>")
	if err != nil {
		return err
	}
	defer done()
	password, err := readPassword()
	if err != nil {
		return err
	}
	if len([]rune(password)) < auth.MinPasswordLength {
		return fmt.Errorf("the password must be at least %d characters", auth.MinPasswordLength)
	}
	if err := store.SetPassword(ctx, email, password); err != nil {
		return err
	}
	auditRecovery(ctx, store, "auth.password_reset", email,
		"Password replaced; every session signed out. The second factor was left as it was.")
	fmt.Printf("Set a new password for %s and signed it out everywhere.\n"+
		"Their second factor is unchanged; if that is lost too, run users reset-mfa.\n", email)
	return nil
}

// `argus-control users set-role <email> <role>`: put an account back in
// charge when nobody who administers the deployment can sign in.
func runUsersSetRole(configPath string, args []string) error {
	if len(args) != 2 {
		return errors.New("usage: argus-control users set-role <email> owner|admin|approver|operator|auditor")
	}
	switch args[1] {
	case "owner", "admin", "approver", "operator", "auditor":
	default:
		return fmt.Errorf("unknown role %q", args[1])
	}
	store, email, ctx, done, err := openForRecovery(configPath, args, "set-role <email> <role>")
	if err != nil {
		return err
	}
	defer done()
	before, err := store.SetAccountRole(ctx, email, args[1])
	switch {
	case errors.Is(err, control.ErrAlreadyInState):
		fmt.Printf("%s is already %s.\n", email, args[1])
		return nil
	case errors.Is(err, control.ErrLastAdmin):
		return fmt.Errorf("refusing: %w. Make another account owner or admin first", err)
	case err != nil:
		return err
	}
	if err := store.RevokeAllSessions(ctx, email); err != nil {
		return err
	}
	auditRecovery(ctx, store, "user.role_changed", email,
		fmt.Sprintf("Role changed from %s to %s; every session signed out.", before, args[1]))
	fmt.Printf("%s is now %s (was %s), and was signed out everywhere.\n", email, args[1], before)
	return nil
}

// `argus-control users sign-out <email>`: end every session an account has,
// for a cookie that may have been stolen.
func runUsersSignOut(configPath string, args []string) error {
	store, email, ctx, done, err := openForRecovery(configPath, args, "sign-out <email>")
	if err != nil {
		return err
	}
	defer done()
	if err := store.RevokeAllSessions(ctx, email); err != nil {
		return err
	}
	auditRecovery(ctx, store, "auth.sessions_revoked", email, "Every session signed out.")
	fmt.Printf("Signed %s out everywhere.\n", email)
	return nil
}
